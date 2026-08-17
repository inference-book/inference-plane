package vast

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/inference-book/inference-plane/internal/provisioners"
)

func TestClassifyEnginePhaseSeparatesThePullFromTheLoad(t *testing.T) {
	// The distinction the whole issue is about. Both conditions used to
	// report "waiting for port 8000 to be mapped", so an operator could
	// not tell a 10 GB download from an engine loading weights, and the
	// phase histogram recorded one opaque block instead of two stages.
	cases := []struct {
		status        string
		endpointReady bool
		want          string
	}{
		{"created", false, phaseScheduling},
		{"loading", false, phaseImagePull},
		{"running", false, phaseEngineInit},
		{"", false, phaseScheduling},
	}
	for _, tc := range cases {
		if got := classifyEnginePhase(tc.status, tc.endpointReady); got != tc.want {
			t.Errorf("actual_status %q -> %q, want %q", tc.status, got, tc.want)
		}
	}
}

func TestAMappedPortOutranksALaggingStatus(t *testing.T) {
	// The port is mapped only once the container runs, and Vast's status
	// field lags the docker daemon it reports on. Believing the status
	// over the port would walk the operator backwards into image-pull
	// while the engine was already loading.
	if got := classifyEnginePhase("loading", true); got != phaseEngineInit {
		t.Errorf("phase = %q with a mapped port, want %q", got, phaseEngineInit)
	}
}

func TestUnknownStatusNeverCountsAsProgress(t *testing.T) {
	// A word we do not model, or a status read that flakes, has to rank
	// below every real rung. Ranking it above one would let a bad read
	// advance the ladder and close a phase that had not finished.
	if got := enginePhaseOrdinal("some-new-vast-word"); got != 0 {
		t.Errorf("unknown ordinal = %d, want 0", got)
	}
	for _, phase := range []string{phaseScheduling, phaseImagePull, phaseEngineInit} {
		if enginePhaseOrdinal(phase) <= 0 {
			t.Errorf("%q ranks at or below unknown", phase)
		}
	}
}

func TestTheLadderOnlyClimbs(t *testing.T) {
	if !(enginePhaseOrdinal(phaseScheduling) < enginePhaseOrdinal(phaseImagePull) &&
		enginePhaseOrdinal(phaseImagePull) < enginePhaseOrdinal(phaseEngineInit)) {
		t.Error("the rungs are not ordered scheduling < image-pull < engine-init")
	}
}

func TestEngineInitIsNotProviderPrefixed(t *testing.T) {
	// A deploy dashboard slicing phase duration should put a RunPod
	// engine-init next to a Vast one. The rungs that describe the
	// provider's own work keep their prefix; the one describing the
	// engine does not, and RunPod already spells it this way.
	if phaseEngineInit != "engine:init" {
		t.Errorf("engine rung = %q, want the unprefixed %q so it slices with RunPod's", phaseEngineInit, "engine:init")
	}
	for _, phase := range []string{phaseScheduling, phaseImagePull} {
		if !strings.HasPrefix(phase, "vast:") {
			t.Errorf("%q describes vast's own work and should carry the provider prefix", phase)
		}
	}
}

func TestPullProgressSurfacesTheRetrySignal(t *testing.T) {
	// The one phrase that separates a pull that is slow from a pull that
	// will never finish. Hiding it behind a generic wait is what cost
	// nine minutes and a rental on 2026-08-11.
	got := pullProgress("Retrying in 5 seconds")
	if !strings.Contains(strings.ToLower(got), "retrying") {
		t.Errorf("pullProgress dropped the retry signal: %q", got)
	}
}

func TestPullProgressCarriesDockersOwnWords(t *testing.T) {
	for _, msg := range []string{"Downloading", "Verifying Checksum", "Pull complete", "Extracting"} {
		if got := pullProgress(msg); got == "" {
			t.Errorf("pullProgress(%q) returned nothing; the host's words are the value here", msg)
		}
	}
}

func TestPullProgressStaysSilentOnATerminalMessage(t *testing.T) {
	// A container that has already died is the FailureReporter's to
	// report. Repeating its error as though it were progress would tell
	// the operator to keep waiting for something that is never coming.
	terminal := "Error response from daemon: OCI runtime create failed: failed to inject CDI devices"
	if got := pullProgress(terminal); got != "" {
		t.Errorf("pullProgress surfaced a terminal message as progress: %q", got)
	}
}

func TestPullProgressFlattensAndBoundsHostOutput(t *testing.T) {
	// status_msg is docker's multi-line output rendered into a single
	// progress field, and a chatty host must not swamp it.
	multi := "Downloading\n  layer 1\n  layer 2"
	got := pullProgress(multi)
	if strings.Contains(got, "\n") {
		t.Errorf("progress kept its newlines: %q", got)
	}
	long := "Downloading " + strings.Repeat("x", 500)
	if got := pullProgress(long); len(got) > 130 {
		t.Errorf("progress is %d chars, want it bounded", len(got))
	}
}

func TestPhaseProgressPrefersTheHostsAccount(t *testing.T) {
	// When the host is saying something specific, that beats our generic
	// description of the rung. When it is not, the rung still has to say
	// what is happening rather than falling back to an endpoint note the
	// operator cannot interpret.
	withHost := phaseProgress(phaseImagePull, "Retrying in 5 seconds", "waiting for port 8000 to be mapped")
	if !strings.Contains(withHost, "Retrying") {
		t.Errorf("host message lost: %q", withHost)
	}
	withoutHost := phaseProgress(phaseImagePull, "", "waiting for port 8000 to be mapped")
	if !strings.Contains(withoutHost, "pulling engine image") {
		t.Errorf("no rung description without a host message: %q", withoutHost)
	}
	bare := phaseProgress(phaseEngineInit, "", "")
	if bare == "" {
		t.Error("phaseProgress returned nothing with no inputs")
	}
}

// getsPerTick is how many times the readiness loop fetches the contract on
// one pass: once to read the endpoint and the status, and once more through
// provisioners.TerminalFailure. The deployer documents that second GET as the
// deliberate price of the terminal-failure policy living in one place. A
// staged fixture has to know the number to advance one record per tick; if
// the loop is ever hoisted (#268) and the duplicate read goes away, the
// ladder tests below fail and this constant is the thing to change.
const getsPerTick = 2

// stagedContracts serves a different instance record on each poll, so a test
// can walk the loop through a real cold start rather than one frozen tick.
// The last record is served indefinitely once the sequence runs out.
func stagedContracts(t *testing.T, records []map[string]any) *httptest.Server {
	t.Helper()
	var n int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		rec := records[min(n/getsPerTick, len(records)-1)]
		n++
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"instances": rec})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestTheLoopWalksTheLadderThroughAColdStart(t *testing.T) {
	// End to end over the loop, because the unit tests above prove the
	// mapping and prove nothing about the wiring. Before this, all three
	// of these ticks emitted the single phase "vast:engine-ready" and the
	// phase histogram recorded one undifferentiated block (#259).
	srv := stagedContracts(t, []map[string]any{
		{"id": 1, "actual_status": "created", "cur_state": "loading"},
		{"id": 1, "actual_status": "loading", "cur_state": "loading", "status_msg": "0d30c18b26f8: Downloading"},
		{"id": 1, "actual_status": "running", "cur_state": "running", "status_msg": "0d30c18b26f8: Pull complete"},
	})

	p, _ := newTestProvider(t, nil)
	p.client = NewClient("k", WithBaseURL(srv.URL))
	p.engineReadyTimeout = 250 * time.Millisecond
	p.sshReadyInterval = 10 * time.Millisecond

	var mu sync.Mutex
	var phases []string
	_, _ = p.waitForEngineReady(context.Background(), 1, 8000,
		func(u provisioners.DeployStateUpdate) {
			mu.Lock()
			defer mu.Unlock()
			if len(phases) == 0 || phases[len(phases)-1] != u.Phase {
				phases = append(phases, u.Phase)
			}
		})

	mu.Lock()
	defer mu.Unlock()
	want := []string{phaseScheduling, phaseImagePull, phaseEngineInit}
	if len(phases) != len(want) {
		t.Fatalf("phases = %v, want the three rungs %v", phases, want)
	}
	for i := range want {
		if phases[i] != want[i] {
			t.Errorf("phase %d = %q, want %q (full: %v)", i, phases[i], want[i], phases)
		}
	}
}

func TestTheLadderDoesNotRewindOnAFlakyRead(t *testing.T) {
	// A phase change opens and closes a bucket in the deploy phase
	// histogram, so a rung that rewinds records two short image-pulls
	// where there was one long one. Vast's status field lags and its
	// control API goes slow in bursts, so this is the normal case rather
	// than the exotic one.
	srv := stagedContracts(t, []map[string]any{
		{"id": 1, "actual_status": "running", "cur_state": "running"},
		{"id": 1, "actual_status": "loading", "cur_state": "loading"},
		{"id": 1, "actual_status": "", "cur_state": ""},
	})

	p, _ := newTestProvider(t, nil)
	p.client = NewClient("k", WithBaseURL(srv.URL))
	p.engineReadyTimeout = 250 * time.Millisecond
	p.sshReadyInterval = 10 * time.Millisecond

	var mu sync.Mutex
	var lowest = 99
	_, _ = p.waitForEngineReady(context.Background(), 1, 8000,
		func(u provisioners.DeployStateUpdate) {
			mu.Lock()
			defer mu.Unlock()
			if o := enginePhaseOrdinal(u.Phase); o < lowest {
				lowest = o
			}
		})

	mu.Lock()
	defer mu.Unlock()
	if lowest != enginePhaseOrdinal(phaseEngineInit) {
		t.Errorf("the ladder rewound below engine-init after reaching it (lowest ordinal %d)", lowest)
	}
}

func TestATimeoutNamesTheRungItDiedOn(t *testing.T) {
	// A 45-minute timeout that says only "did not answer /health" leaves
	// an operator unable to tell a host that never finished pulling from
	// an engine that could not load the model. Those have different
	// fixes: a different host, or a smaller plan.
	srv := stagedContracts(t, []map[string]any{
		{"id": 1, "actual_status": "loading", "cur_state": "loading", "status_msg": "Downloading"},
	})

	p, _ := newTestProvider(t, nil)
	p.client = NewClient("k", WithBaseURL(srv.URL))
	p.engineReadyTimeout = 60 * time.Millisecond
	p.sshReadyInterval = 10 * time.Millisecond

	_, err := p.waitForEngineReady(context.Background(), 1, 8000, func(provisioners.DeployStateUpdate) {})
	if err == nil {
		t.Fatal("want a timeout")
	}
	if !strings.Contains(err.Error(), phaseImagePull) {
		t.Errorf("timeout does not name the rung it died on: %v", err)
	}
}
