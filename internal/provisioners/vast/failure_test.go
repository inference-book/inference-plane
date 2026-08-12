package vast

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
)

// The two real failures, verbatim from the hosts that produced them on
// 2026-08-11. Kept as literals rather than paraphrased, because a paraphrase
// would test the paraphrase.
const (
	msgRegistryUnreachable = `error pulling image configuration: download failed after attempts=6: read tcp [2409:8a3c:5c00:efc0:ae1f:6bff:fe25:21b6]:41550->[2600:9000:2079:c00:9:4855:aac0:93a1]:443: read: connection reset by peer`

	msgBrokenCDI = `Error response from daemon: failed to create task for container: failed to create shim task: OCI runtime create failed: could not apply required modification to OCI specification: error modifying OCI spec: failed to inject CDI devices: unresolvable CDI devices D.c0b49a0a826324dc80459dac9601c9f1694f3141ab95994fa307725c15de5534/gpu=0: unknown
Error: failed to start containers: C.47474765`
)

func TestTerminalHostFailureRecognisesTheRealFailures(t *testing.T) {
	for _, tc := range []struct {
		name     string
		curState string
		msg      string
	}{
		{"registry unreachable over IPv6", "stopped", msgRegistryUnreachable},
		{"broken NVIDIA CDI", "stopped", msgBrokenCDI},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dead, why := terminalHostFailure(tc.curState, tc.msg)
			if !dead {
				t.Fatalf("not recognised as terminal, so the deploy would burn the full timeout: %q", tc.msg)
			}
			// The message is the diagnosis. A generic "deploy failed" would
			// have left nothing to act on: it was the IPv6 CDN address in the
			// first one that identified a broken host network path.
			if why == "" {
				t.Error("no reason returned; the operator gets a failure with no evidence")
			}
			if strings.Contains(why, "\n") {
				t.Errorf("reason spans lines and would wreck the progress display: %q", why)
			}
		})
	}
}

// The expensive mistake in the other direction. A slow deploy is the NORMAL
// case here -- a 10 GB engine image on community capacity routinely takes
// minutes -- so anything that reads as progress must keep waiting.
func TestTerminalHostFailureIgnoresProgressAndTransients(t *testing.T) {
	for _, msg := range []string{
		"",
		"   ",
		"0d30c18b26f8: Retrying in 5 seconds",
		"80ee424dc764: Downloading",
		"3d97a47c3c73: Verifying Checksum",
		"648b00041f00: Download complete",
		"30c3b0c7b61e: Pull complete",
		"12720c26d92e: Extracting",
		"#0 building with \"default\" instance using docker driver",
	} {
		if dead, why := terminalHostFailure("stopped", msg); dead {
			t.Errorf("healthy-but-slow message treated as terminal (%q -> %q); this kills working deploys", msg, why)
		}
	}
}

// Retrying is the trap. Docker prints it while it is still working, and the
// host that printed it forty times did eventually fail with a real terminal
// message. Several other hosts recovered. So a message carrying BOTH a retry
// notice and error-ish words must keep waiting.
func TestRetryingBeatsAnErrorWordInTheSameMessage(t *testing.T) {
	msg := "80ee424dc764: error pulling image configuration, Retrying in 5 seconds"
	if dead, _ := terminalHostFailure("stopped", msg); dead {
		t.Error("gave up while docker was still retrying; hosts do recover from this")
	}
}

// cur_state alone is not evidence. It reaches "stopped" for benign reasons
// early in a container's life, and acting on it would abort healthy deploys
// before they ever started.
func TestCurStateAloneIsNotTerminal(t *testing.T) {
	if dead, _ := terminalHostFailure("stopped", ""); dead {
		t.Error("cur_state=stopped with no message treated as terminal")
	}
	if dead, _ := terminalHostFailure("exited", ""); dead {
		t.Error("cur_state=exited with no message treated as terminal")
	}
}

// End to end through the readiness loop: a host that reports a terminal
// failure must abort promptly rather than run to the timeout, and the error
// must carry the host's own words.
func TestWaitForEngineReadyAbortsOnATerminalFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"instances": map[string]any{
			"id":            1,
			"actual_status": "created",
			"cur_state":     "stopped",
			"status_msg":    msgBrokenCDI,
		}})
	}))
	defer srv.Close()

	p, _ := newTestProvider(t, nil)
	p.client = NewClient("k", WithBaseURL(srv.URL))
	// A timeout far longer than the test's patience: if the abort does not
	// work, this fails by running long rather than by asserting.
	p.engineReadyTimeout = 30 * time.Second
	p.sshReadyInterval = 10 * time.Millisecond

	start := time.Now()
	_, err := p.waitForEngineReady(context.Background(), 1, 8000,
		func(provisioners.DeployStateUpdate) {})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("waited out a container that had already failed and returned success")
	}
	if elapsed > 5*time.Second {
		t.Errorf("took %v to notice a terminal failure; it polled instead of giving up", elapsed)
	}
	if !strings.Contains(err.Error(), "CDI") {
		t.Errorf("error dropped the host's diagnosis: %v", err)
	}
}

// A slow-but-healthy host must still be waited on. This is the regression
// that would cost real money in the other direction.
func TestWaitForEngineReadyKeepsWaitingWhilePulling(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"instances": map[string]any{
			"id":            1,
			"actual_status": "loading",
			"cur_state":     "stopped",
			"status_msg":    "0d30c18b26f8: Retrying in 5 seconds",
		}})
	}))
	defer srv.Close()

	p, _ := newTestProvider(t, nil)
	p.client = NewClient("k", WithBaseURL(srv.URL))
	p.engineReadyTimeout = 300 * time.Millisecond
	p.sshReadyInterval = 10 * time.Millisecond

	_, err := p.waitForEngineReady(context.Background(), 1, 8000,
		func(provisioners.DeployStateUpdate) {})

	if err == nil {
		t.Fatal("expected the timeout to be what ends this")
	}
	if strings.Contains(err.Error(), "will not start") {
		t.Errorf("aborted a host that was still pulling: %v", err)
	}
}

// Vast's control API goes slow in bursts and recovers. A describe failure was
// observed resolving itself mid-deploy, so it must read as progress, not as a
// dead host.
func TestDescribeFailureIsNotTreatedAsTerminal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	p, _ := newTestProvider(t, nil)
	p.client = NewClient("k", WithBaseURL(srv.URL))
	p.engineReadyTimeout = 200 * time.Millisecond
	p.sshReadyInterval = 10 * time.Millisecond

	var sawProgress bool
	_, err := p.waitForEngineReady(context.Background(), 1, 8000,
		func(u provisioners.DeployStateUpdate) {
			if strings.Contains(u.ProgressMessage, "describe contract") {
				sawProgress = true
			}
		})

	if err == nil {
		t.Fatal("expected the timeout to end this")
	}
	if strings.Contains(err.Error(), "will not start") {
		t.Errorf("a flaky control API was reported as a dead host: %v", err)
	}
	if !sawProgress {
		t.Error("the describe failure never surfaced as progress; the operator sees a silent wait")
	}
	_ = provisionerv1.DeploymentState_DEPLOYMENT_STATE_CONFIGURING
}
