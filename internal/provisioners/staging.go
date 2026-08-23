package provisioners

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/modelstores"
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
// Abort thresholds. Every one of them exists to make the judgement harder to
// reach, not easier, because the cost of being wrong is asymmetric: a missed
// abort wastes the rest of a timeout, and a wrong abort throws away a deploy
// that was going to work.
const (
	// abortMinWindow is how long the sensor must have been measuring before
	// its rate is allowed to end a deploy. A download's first minute is not
	// its steady state: connections are still opening and the cache is
	// still being laid out.
	abortMinWindow = 90 * time.Second

	// abortConsecutive is how many readings in a row must agree. Throughput
	// to a CDN is bursty, and one slow window inside an otherwise healthy
	// download is normal.
	abortConsecutive = 3

	// abortSlack is the factor by which the projection must overshoot. At
	// 1.5 a deploy is abandoned only when it would need half again as long
	// as it has, so a download merely running close to the wire is left to
	// finish.
	abortSlack = 1.5
)

// abortEnabled reports whether a hopeless download may end its own deploy.
//
// On by default, because the whole point is to stop paying for an hour of
// downloading that was never going to finish, and an operator cannot watch
// every deploy. The escape hatch is for the case where the judgement is
// wrong in a way nobody predicted, which on a feature that has not yet run
// on hardware is worth leaving reachable.
func abortEnabled() bool {
	return os.Getenv("IPLANE_ENGINE_ABORT_ON_SLOW_DOWNLOAD") != "0"
}

type phaseRefiner struct {
	read       StagingReader
	engineID   string
	checkpoint int64
	// abort ends the deploy, carrying the reason. Nil when nothing gave the
	// refiner a way to stop the wait, which is every path that only wants
	// the phase.
	abort func(error)
	// hopeless counts consecutive readings that projected past the
	// deadline. Reset by any reading that did not, so the count means
	// "still hopeless" rather than "was hopeless this often".
	hopeless int
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

	p.judge(u, s)

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
func (s *Service) newPhaseRefiner(engineID string, checkpointBytes int64, abort func(error)) *phaseRefiner {
	if s.stagingReader == nil {
		return nil
	}
	return &phaseRefiner{
		read:       s.stagingReader,
		engineID:   engineID,
		checkpoint: checkpointBytes,
		abort:      abort,
	}
}

// judge decides whether this download can still finish in the time it has,
// and ends the deploy when it cannot.
//
// # What it takes to abort
//
// Every one of these must hold, and any one missing means carry on:
//
//   - the abort is enabled, and something gave us a way to stop the wait
//   - the wait told us its deadline
//   - we know how many bytes the download is
//   - the sensor has been measuring for abortMinWindow
//   - the rate is POSITIVE
//   - the projection overshoots the remaining time by abortSlack
//   - and it has done so abortConsecutive times running
//
// # Why a rate of zero must never abort
//
// This is the trap the whole function is shaped around. A stalled download
// projects to infinity, so treating zero as "too slow" would abort every
// deploy the instant it paused between shards, which is the one thing a
// download does constantly. Worse, it would fire hardest on exactly the
// large multi-shard checkpoints this exists to protect, and the failure
// would look like a correct decision: the numbers all agree that infinity
// exceeds any deadline.
//
// So only a measured, positive, too-slow rate ends a deploy. A download that
// has genuinely died is left to the deadline, which is what the deadline is
// for and costs no more than it did before any of this existed.
func (p *phaseRefiner) judge(u DeployStateUpdate, s *provisionerv1.StagingProgress) {
	if p.abort == nil || p.checkpoint <= 0 || u.Deadline.IsZero() || !abortEnabled() {
		return
	}
	// Only while weights are still arriving. Once loading has begun the
	// remaining time is the engine's to spend and nothing here models it.
	if p.reachedLoad {
		return
	}
	rate := s.GetBytesPerSecond()
	if rate <= 0 || s.GetIntervalSeconds() < abortMinWindow.Seconds() {
		p.hopeless = 0
		return
	}
	remainingBytes := p.checkpoint - s.GetBytesLocal()
	if remainingBytes <= 0 {
		p.hopeless = 0
		return
	}
	// A cache holding more than this download needs means the figures are
	// describing different things: another model on the same volume, or a
	// checkpoint size read against a different revision. Either way the
	// subtraction above is meaningless and the honest move is silence.
	if s.GetBytesLocal() > p.checkpoint {
		p.hopeless = 0
		return
	}

	needed := time.Duration(float64(remainingBytes) / rate * float64(time.Second))
	left := time.Until(u.Deadline)
	if left <= 0 || float64(needed) <= float64(left)*abortSlack {
		p.hopeless = 0
		return
	}

	p.hopeless++
	if p.hopeless < abortConsecutive {
		return
	}
	p.abort(fmt.Errorf(
		"abandoning this deploy: %s of %s still to fetch at %s/s needs about %s, and only %s remains before the engine-ready deadline. "+
			"the host is roughly %.1fx too slow for this checkpoint; retry elsewhere, or raise IPLANE_ENGINE_READY_TIMEOUT, or set IPLANE_ENGINE_ABORT_ON_SLOW_DOWNLOAD=0",
		formatGB(remainingBytes), formatGB(p.checkpoint), formatGB(int64(rate)),
		needed.Round(time.Minute), left.Round(time.Minute), float64(needed)/float64(left)))
}

// checkpointBytes reports how many bytes this deployment's model downloads,
// or 0 when that cannot be had.
//
// 0 disables the abort for this deploy, which is the right failure: the
// judgement needs a target to project against, and guessing one from the
// parameter count is precisely the packed-elements mistake #382 already cost
// us on a quantized checkpoint.
func (s *Service) checkpointBytes(ctx context.Context, dep *provisionerv1.Deployment) int64 {
	src, ok := s.modelStore.(modelstores.CheckpointSource)
	if !ok {
		return 0
	}
	cp, err := src.Checkpoint(ctx, &provisionerv1.DescribeModelRequest{ModelSpec: dep.GetModel()})
	if err != nil {
		slog.Info("no download size for this model; a slow host will run to the deadline rather than being abandoned",
			"deployment", dep.GetId(), "model", dep.GetModel(), "reason", err)
		return 0
	}
	return cp.GetDownloadBytes()
}
