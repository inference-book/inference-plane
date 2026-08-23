package provisioners

import (
	"context"
	"testing"

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
