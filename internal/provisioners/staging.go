package provisioners

import (
	"context"
	"fmt"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// Splitting engine:init, using what the agent reports about the disk.
//
// `engine:init` covers everything between the container starting and the
// engine answering /health: the weight download, the load into VRAM, and
// graph capture. One rung for all of it, and no way to tell which part is
// slow. Two GLM-5.2 deploys spent an hour each in it and timed out, and the
// record cannot say to this day whether the network or the load was at
// fault.
//
// The engine cannot help. vLLM suppresses the download progress bar, so the
// container prints nothing until the fetch finishes. The agent can: it is
// running throughout, and since #413's sensor half it reports how many bytes
// have landed and how fast they are arriving.
//
// So the split happens here rather than in either provider. The Service
// already wraps every provider's emit, and it already holds the replica id
// the agent registers under, so one refinement covers RunPod and Vast alike
// and neither adapter learns about the engine registry.

const (
	// PhaseEngineInit is what a provider emits once its own rungs are done
	// and the engine is starting. Refined into one of the two below when
	// there is a reading to refine it with.
	PhaseEngineInit = "engine:init"
	// PhaseEngineDownload is weights arriving on the box.
	PhaseEngineDownload = "engine:download"
	// PhaseEngineLoad is weights present and no longer growing, with the
	// engine still not answering. Covers the load into VRAM and graph
	// capture, which nothing yet separates.
	PhaseEngineLoad = "engine:load"
)

// StagingReader answers what an engine's agent last reported about weights
// arriving on its node, or false when there is nothing to report.
//
// A func rather than an interface because there is one method and one
// implementation, and because the dependency has to be injected: the engine
// registry (internal/engines) imports this package, so this package cannot
// import it back. serve.go ties the knot, the same way it already injects a
// provisioners-backed Drainer into engines.
//
// false covers every kind of nothing: no registry wired, no registration
// yet, an agent that never started, a reading marked unavailable. All of
// them mean the same thing to a caller, which is that no claim can be made.
type StagingReader func(ctx context.Context, engineID string) (*provisionerv1.StagingProgress, bool)

// WithStagingReader supplies the reader that lets the deploy path tell a
// download from a load.
//
// Left unset, `engine:init` is emitted exactly as it was before, which is
// what a control plane running without an engine registry should do.
func WithStagingReader(r StagingReader) Option {
	return func(s *Service) {
		if r != nil {
			s.stagingReader = r
		}
	}
}

// phaseRefiner turns a provider's engine:init into engine:download or
// engine:load, and holds the little state that decision needs.
//
// One per replica deploy, because the state is per-replica and because the
// emit closure it hooks into is already per-replica.
type phaseRefiner struct {
	read     StagingReader
	engineID string
	// reachedLoad latches. Weights arrive in bursts with pauses between
	// shards, so a rate that momentarily reads zero must not bounce the
	// phase back to download: the ladder's whole rule is that a phase never
	// regresses, and a rung that rewound would record two short downloads
	// where there was one long one.
	reachedLoad bool
}

// refine rewrites an update's phase in place when it can say something more
// specific, and returns it otherwise unchanged.
//
// Only ever touches engine:init. Every provider rung before it describes the
// provider's own machinery and is none of this function's business.
//
// Silence is the default in every ambiguous case, which is the opposite
// polarity to most of this package but the right one here. A missing reading
// must never be dressed up as a phase nobody observed, because the next thing
// to consume these is an early abort with a rental attached to its decision.
func (p *phaseRefiner) refine(ctx context.Context, u DeployStateUpdate) DeployStateUpdate {
	if p == nil || p.read == nil || u.Phase != PhaseEngineInit {
		return u
	}
	s, ok := p.read(ctx, p.engineID)
	if !ok || !s.GetAvailable() {
		return u
	}
	// An unseeded sensor has bytes but no rate yet: one observation cannot
	// support one. Saying nothing for a tick costs a tick.
	if s.GetIntervalSeconds() <= 0 {
		return u
	}

	switch {
	case s.GetBytesPerSecond() > 0:
		if p.reachedLoad {
			// Already loading and the disk grew anyway. Ambiguous rather
			// than a regression, so hold the higher rung and say nothing.
			return u
		}
		u.Phase = PhaseEngineDownload
		u.ProgressMessage = fmt.Sprintf("%s arrived, %s/s", formatGB(s.GetBytesLocal()), formatGB(int64(s.GetBytesPerSecond())))
	case s.GetBytesLocal() > 0:
		// Present and not growing. A cold deploy that finished fetching and
		// a warm deploy that mounted a pre-staged volume both land here,
		// which is right: from the outside they are the same state, and the
		// storage_tier label already distinguishes them.
		p.reachedLoad = true
		u.Phase = PhaseEngineLoad
		u.ProgressMessage = fmt.Sprintf("%s on disk, loading", formatGB(s.GetBytesLocal()))
	default:
		// Measured, and nothing has arrived at all. Not a download yet.
		return u
	}
	return u
}

// newPhaseRefiner returns a refiner for one replica, or nil when no reader is
// configured. A nil refiner is safe to call.
func (s *Service) newPhaseRefiner(engineID string) *phaseRefiner {
	if s.stagingReader == nil {
		return nil
	}
	return &phaseRefiner{read: s.stagingReader, engineID: engineID}
}
