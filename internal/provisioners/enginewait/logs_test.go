package enginewait_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/enginewait"
)

// A wait that ends without saying what the engine was doing costs the
// whole rental and teaches nothing. The GLM-5.2 run spent sixty minutes
// and $23 timing out at engine:init, and the only record was iplane's own
// "still waiting" line: vLLM's stdout never left the pod (#47).
//
// The engine's own words are what distinguish "downloading, be patient"
// from "refused to start an hour ago".
func TestATimeoutCarriesTheEnginesOwnWords(t *testing.T) {
	_, err := enginewait.Wait(context.Background(), enginewait.Config{
		Timeout:  10 * time.Millisecond,
		Interval: time.Millisecond,
		Endpoint: "http://engine:8000",
		Ladder:   testLadder(),
		Observe:  func(context.Context, string) enginewait.Observation { return enginewait.Observation{} },
		Probe:    func(context.Context, string) (bool, string) { return false, "connection refused" },
		Emit:     func(provisioners.DeployStateUpdate) {},
		Logs: func(context.Context) string {
			return "ValueError: The model's max seq len (262144) is larger than the maximum number of tokens that can be stored in KV cache (131072)"
		},
	})
	if err == nil {
		t.Fatal("want a timeout")
	}
	if !strings.Contains(err.Error(), "max seq len") {
		t.Errorf("the timeout does not carry the engine's own output, so an operator learns nothing: %v", err)
	}
}

// A provider that cannot report logs must not turn a real timeout into a
// confusing one. RunPod's REST API exposes no logs at all, which is the
// case this has to leave alone.
func TestATimeoutWithoutLogsIsUnchanged(t *testing.T) {
	_, err := enginewait.Wait(context.Background(), enginewait.Config{
		Timeout:  10 * time.Millisecond,
		Interval: time.Millisecond,
		Endpoint: "http://engine:8000",
		Ladder:   testLadder(),
		Observe:  func(context.Context, string) enginewait.Observation { return enginewait.Observation{} },
		Probe:    func(context.Context, string) (bool, string) { return false, "connection refused" },
		Emit:     func(provisioners.DeployStateUpdate) {},
	})
	if err == nil {
		t.Fatal("want a timeout")
	}
	if strings.Contains(err.Error(), "engine said") {
		t.Errorf("invented an engine-log section with no logs to show: %v", err)
	}
	if !strings.Contains(err.Error(), "did not answer") {
		t.Errorf("lost the original timeout message: %v", err)
	}
}

// A provider that CAN be asked and has nothing to show is the common
// case on a young container, and it must not produce an empty "engine
// said:" section. That reads as an engine that printed nothing, which is
// a different and wronger claim than silence.
func TestAnEmptyLogAddsNoSection(t *testing.T) {
	_, err := enginewait.Wait(context.Background(), enginewait.Config{
		Timeout:  10 * time.Millisecond,
		Interval: time.Millisecond,
		Endpoint: "http://engine:8000",
		Ladder:   testLadder(),
		Observe:  func(context.Context, string) enginewait.Observation { return enginewait.Observation{} },
		Probe:    func(context.Context, string) (bool, string) { return false, "connection refused" },
		Emit:     func(provisioners.DeployStateUpdate) {},
		Logs:     func(context.Context) string { return "   \n  \n" },
	})
	if err == nil {
		t.Fatal("want a timeout")
	}
	if strings.Contains(err.Error(), "engine said") {
		t.Errorf("added an empty engine-log section: %q", err.Error())
	}
}

// The same on the terminal-failure path. A host that will never run the
// container is exactly when the container's own error matters, and it is
// the path that already exists to stop billing early.
func TestATerminalFailureCarriesTheEnginesOwnWords(t *testing.T) {
	_, err := enginewait.Wait(context.Background(), enginewait.Config{
		Timeout:  time.Minute,
		Interval: time.Millisecond,
		Ladder:   testLadder(),
		Observe: func(context.Context, string) enginewait.Observation {
			return enginewait.Observation{Fatal: errTerminal}
		},
		Probe: func(context.Context, string) (bool, string) { return false, "" },
		Emit:  func(provisioners.DeployStateUpdate) {},
		Logs:  func(context.Context) string { return "CUDA out of memory" },
	})
	if err == nil {
		t.Fatal("want the fatal error")
	}
	if !strings.Contains(err.Error(), "CUDA out of memory") {
		t.Errorf("terminal failure without the engine's words: %v", err)
	}
}

var errTerminal = &terminalErr{}

type terminalErr struct{}

func (*terminalErr) Error() string { return "host will never run this container" }

// testLadder is the minimum a Ladder has to provide: every real caller
// fills both funcs, and Wait calls them per tick.
func testLadder() enginewait.Ladder {
	return enginewait.Ladder{
		Ordinal:     func(string) int { return 0 },
		Description: func(string) string { return "starting" },
	}
}
