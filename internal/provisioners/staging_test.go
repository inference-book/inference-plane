package provisioners

import (
	"context"
	"testing"
	"time"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/vrambudget"
)

func reader(s *provisionerv1.StagingProgress, ok bool) StagingReader {
	return func(context.Context, string) (*provisionerv1.StagingProgress, bool) { return s, ok }
}

func refineWith(t *testing.T, r StagingReader, phase string) DeployStateUpdate {
	t.Helper()
	p := &phaseRefiner{read: r, engineID: "repl-0"}
	return p.refine(context.Background(), DeployStateUpdate{Phase: phase})
}

// No reader, no registry, no registration: engine:init survives untouched.
// A control plane without an engine registry must behave exactly as before.
func TestRefineWithoutReaderLeavesPhaseAlone(t *testing.T) {
	var p *phaseRefiner
	got := p.refine(context.Background(), DeployStateUpdate{Phase: PhaseEngineInit})
	if got.Phase != PhaseEngineInit {
		t.Fatalf("phase = %q, want unchanged", got.Phase)
	}
}

// Every kind of nothing resolves to saying nothing. The next consumer of
// these phases is an early abort with a rental attached, so a missing
// reading must never become a phase nobody observed.
func TestRefineSaysNothingWithoutAReading(t *testing.T) {
	cases := map[string]StagingReader{
		"not registered": reader(nil, false),
		// Bytes and an interval present but available=false: only the
		// availability check stands between this and a fabricated phase.
		"unavailable": reader(&provisionerv1.StagingProgress{
			Available: false, BytesLocal: 474 * vrambudget.GB, BytesPerSecond: 1e8, IntervalSeconds: 30}, true),
		"unseeded sensor": reader(&provisionerv1.StagingProgress{Available: true, BytesLocal: 5}, true),
		"nothing arrived": reader(&provisionerv1.StagingProgress{Available: true, IntervalSeconds: 30}, true),
	}
	for name, r := range cases {
		t.Run(name, func(t *testing.T) {
			if got := refineWith(t, r, PhaseEngineInit); got.Phase != PhaseEngineInit {
				t.Fatalf("phase = %q, want %q", got.Phase, PhaseEngineInit)
			}
		})
	}
}

// Only engine:init is ours to refine. Every earlier rung describes the
// provider's own machinery.
func TestRefineIgnoresOtherPhases(t *testing.T) {
	r := reader(&provisionerv1.StagingProgress{
		Available: true, BytesLocal: 1 << 30, BytesPerSecond: 1e8, IntervalSeconds: 30}, true)
	for _, phase := range []string{"vast:scheduling", "vast:image-pull", "runpod:starting", ""} {
		if got := refineWith(t, r, phase); got.Phase != phase {
			t.Fatalf("phase %q was rewritten to %q", phase, got.Phase)
		}
	}
}

func TestRefineReportsDownloadWhileBytesMove(t *testing.T) {
	got := refineWith(t, reader(&provisionerv1.StagingProgress{
		Available: true, BytesLocal: 150 * vrambudget.GB, BytesPerSecond: 134e6, IntervalSeconds: 30}, true),
		PhaseEngineInit)
	if got.Phase != PhaseEngineDownload {
		t.Fatalf("phase = %q, want %q", got.Phase, PhaseEngineDownload)
	}
	if got.ProgressMessage == "" {
		t.Fatal("download phase carried no progress message")
	}
}

// Present and not growing. A cold deploy that finished fetching and a warm
// deploy that mounted a pre-staged volume are the same state from out here.
func TestRefineReportsLoadWhenBytesStop(t *testing.T) {
	got := refineWith(t, reader(&provisionerv1.StagingProgress{
		Available: true, BytesLocal: 474 * vrambudget.GB, BytesPerSecond: 0, IntervalSeconds: 30}, true),
		PhaseEngineInit)
	if got.Phase != PhaseEngineLoad {
		t.Fatalf("phase = %q, want %q", got.Phase, PhaseEngineLoad)
	}
}

// The ladder's rule is that a phase never regresses. Weights arrive in bursts
// with pauses between shards, so a rate that dips to zero and recovers must
// not record two downloads where there was one.
func TestRefineNeverRegressesFromLoadToDownload(t *testing.T) {
	staging := &provisionerv1.StagingProgress{
		Available: true, BytesLocal: 474 * vrambudget.GB, IntervalSeconds: 30}
	p := &phaseRefiner{read: func(context.Context, string) (*provisionerv1.StagingProgress, bool) {
		return staging, true
	}}

	if got := p.refine(context.Background(), DeployStateUpdate{Phase: PhaseEngineInit}); got.Phase != PhaseEngineLoad {
		t.Fatalf("phase = %q, want %q", got.Phase, PhaseEngineLoad)
	}

	staging.BytesPerSecond = 5e7 // a straggler file lands after loading began
	got := p.refine(context.Background(), DeployStateUpdate{Phase: PhaseEngineInit})
	if got.Phase == PhaseEngineDownload {
		t.Fatal("phase regressed from load back to download")
	}
}

// Refining must not disturb anything else the update carries. The rate is
// stamped once at rent time and losing it silently loses the rental from
// cost accounting (#397).
func TestRefinePreservesTheRestOfTheUpdate(t *testing.T) {
	p := &phaseRefiner{read: reader(&provisionerv1.StagingProgress{
		Available: true, BytesLocal: 1 << 30, BytesPerSecond: 1e8, IntervalSeconds: 30}, true)}
	in := DeployStateUpdate{
		Phase:         PhaseEngineInit,
		State:         provisionerv1.DeploymentState_DEPLOYMENT_STATE_STARTING,
		ContainerID:   "c-1",
		HourlyRateUSD: 21.59,
	}
	got := p.refine(context.Background(), in)
	if got.ContainerID != "c-1" || got.HourlyRateUSD != 21.59 || got.State != in.State {
		t.Fatalf("refine disturbed the update: %+v", got)
	}
}

// judging harness: drives n readings through a refiner and reports what it
// decided, so each rule is tested by the thing that would actually trip it.
type judged struct {
	aborted bool
	reason  error
}

func judge(t *testing.T, checkpoint int64, deadlineIn time.Duration, s *provisionerv1.StagingProgress, ticks int) *judged {
	t.Helper()
	out := &judged{}
	p := &phaseRefiner{
		read:       reader(s, true),
		engineID:   "repl-0",
		checkpoint: checkpoint,
		abort:      func(err error) { out.aborted, out.reason = true, err },
	}
	u := DeployStateUpdate{Phase: PhaseEngineInit, Deadline: time.Now().Add(deadlineIn)}
	for i := 0; i < ticks; i++ {
		p.refine(context.Background(), u)
	}
	return out
}

// 400 GB left at 130 MB/s needs ~55 minutes against 10 remaining. This is
// the GLM-5.2 run that cost $22 twice.
func slowDownload() *provisionerv1.StagingProgress {
	return &provisionerv1.StagingProgress{
		Available: true, BytesLocal: 74 * vrambudget.GB,
		BytesPerSecond: 130e6, IntervalSeconds: 120,
	}
}

func TestAbortsAHopelessDownload(t *testing.T) {
	got := judge(t, 474*vrambudget.GB, 10*time.Minute, slowDownload(), abortConsecutive)
	if !got.aborted {
		t.Fatal("a download needing 55 minutes with 10 left was not abandoned")
	}
	if got.reason == nil {
		t.Fatal("aborted without a reason")
	}
}

// One bad window inside an otherwise healthy download must not end it.
func TestDoesNotAbortBeforeEnoughAgreeingReadings(t *testing.T) {
	if got := judge(t, 474*vrambudget.GB, 10*time.Minute, slowDownload(), abortConsecutive-1); got.aborted {
		t.Fatalf("aborted after %d readings, want at least %d", abortConsecutive-1, abortConsecutive)
	}
}

// A rate that recovers must clear the count, so "hopeless three times ever"
// is not mistaken for "hopeless three times running".
func TestRecoveryClearsTheHopelessCount(t *testing.T) {
	out := &judged{}
	staging := slowDownload()
	p := &phaseRefiner{
		read:       func(context.Context, string) (*provisionerv1.StagingProgress, bool) { return staging, true },
		checkpoint: 474 * vrambudget.GB,
		abort:      func(err error) { out.aborted, out.reason = true, err },
	}
	u := DeployStateUpdate{Phase: PhaseEngineInit, Deadline: time.Now().Add(10 * time.Minute)}

	for i := 0; i < abortConsecutive-1; i++ {
		p.refine(context.Background(), u)
	}
	staging.BytesPerSecond = 2e9 // the host speeds up
	p.refine(context.Background(), u)
	staging.BytesPerSecond = 130e6 // and slows again
	for i := 0; i < abortConsecutive-1; i++ {
		p.refine(context.Background(), u)
	}
	if out.aborted {
		t.Fatal("aborted on non-consecutive slow readings")
	}
}

// THE trap. A stalled download projects to infinite time, so treating zero
// as too-slow aborts every deploy that pauses between shards, and fires
// hardest on exactly the multi-shard checkpoints this protects.
func TestNeverAbortsOnAStalledDownload(t *testing.T) {
	stalled := &provisionerv1.StagingProgress{
		Available: true, BytesLocal: 10 * vrambudget.GB,
		BytesPerSecond: 0, IntervalSeconds: 600,
	}
	if got := judge(t, 474*vrambudget.GB, time.Minute, stalled, 20); got.aborted {
		t.Fatal("a stalled download was abandoned; only a measured positive rate may abort")
	}
}

// Every missing input leaves the deploy alone.
func TestAbortRequiresEveryInput(t *testing.T) {
	full := slowDownload()
	t.Run("no checkpoint size", func(t *testing.T) {
		if judge(t, 0, 10*time.Minute, full, 10).aborted {
			t.Fatal("aborted without knowing the download size")
		}
	})
	t.Run("no deadline", func(t *testing.T) {
		out := &judged{}
		p := &phaseRefiner{read: reader(full, true), checkpoint: 474 * vrambudget.GB,
			abort: func(err error) { out.aborted = true }}
		for i := 0; i < 10; i++ {
			p.refine(context.Background(), DeployStateUpdate{Phase: PhaseEngineInit})
		}
		if out.aborted {
			t.Fatal("aborted without a deadline to miss")
		}
	})
	t.Run("window too short", func(t *testing.T) {
		brief := slowDownload()
		brief.IntervalSeconds = abortMinWindow.Seconds() - 1
		if judge(t, 474*vrambudget.GB, 10*time.Minute, brief, 10).aborted {
			t.Fatal("aborted on a window shorter than abortMinWindow")
		}
	})
	t.Run("no abort func", func(t *testing.T) {
		p := &phaseRefiner{read: reader(full, true), checkpoint: 474 * vrambudget.GB}
		for i := 0; i < 10; i++ {
			p.refine(context.Background(), DeployStateUpdate{
				Phase: PhaseEngineInit, Deadline: time.Now().Add(time.Minute)})
		}
	})
}

// Running close to the wire is not hopeless. Only an overshoot beyond
// abortSlack ends a deploy.
func TestDoesNotAbortADownloadMerelyRunningLate(t *testing.T) {
	// 100 GB left at 200 MB/s needs ~8.3 min; 7 min remain, an overshoot of
	// about 1.19x, under the 1.5x bar.
	tight := &provisionerv1.StagingProgress{
		Available: true, BytesLocal: 374 * vrambudget.GB,
		BytesPerSecond: 200e6, IntervalSeconds: 300,
	}
	if got := judge(t, 474*vrambudget.GB, 7*time.Minute, tight, 10); got.aborted {
		t.Fatal("aborted a download that only needed 1.19x its remaining time")
	}
}

// Once loading has begun the remaining time is the engine's to spend, and
// nothing here models how long a load takes.
func TestNeverAbortsOnceLoading(t *testing.T) {
	out := &judged{}
	staging := &provisionerv1.StagingProgress{
		Available: true, BytesLocal: 474 * vrambudget.GB, IntervalSeconds: 300}
	p := &phaseRefiner{
		read:       func(context.Context, string) (*provisionerv1.StagingProgress, bool) { return staging, true },
		checkpoint: 474 * vrambudget.GB,
		abort:      func(err error) { out.aborted = true },
	}
	u := DeployStateUpdate{Phase: PhaseEngineInit, Deadline: time.Now().Add(time.Minute)}
	if got := p.refine(context.Background(), u); got.Phase != PhaseEngineLoad {
		t.Fatalf("phase = %q, want %q", got.Phase, PhaseEngineLoad)
	}
	staging.BytesLocal = 10 * vrambudget.GB // a stray write after loading began
	staging.BytesPerSecond = 1e6
	for i := 0; i < 10; i++ {
		p.refine(context.Background(), u)
	}
	if out.aborted {
		t.Fatal("aborted after loading had already begun")
	}
}

// A cache larger than the download means the two numbers describe different
// things (another model on the volume, or a size read against another
// revision), so the subtraction is meaningless.
func TestNeverAbortsWhenTheCacheExceedsTheCheckpoint(t *testing.T) {
	odd := slowDownload()
	odd.BytesLocal = 900 * vrambudget.GB
	if got := judge(t, 474*vrambudget.GB, time.Minute, odd, 10); got.aborted {
		t.Fatal("aborted on figures that describe different things")
	}
}

func TestAbortCanBeDisabled(t *testing.T) {
	t.Setenv("IPLANE_ENGINE_ABORT_ON_SLOW_DOWNLOAD", "0")
	if got := judge(t, 474*vrambudget.GB, 10*time.Minute, slowDownload(), 10); got.aborted {
		t.Fatal("aborted with IPLANE_ENGINE_ABORT_ON_SLOW_DOWNLOAD=0")
	}
}
