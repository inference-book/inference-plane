package cmd

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"text/tabwriter"
	"time"

	"github.com/inference-book/inference-plane/gen/go/provisioner/v1/provisionerv1connect"
	"github.com/inference-book/inference-plane/internal/version"
)

// Sweep mode drives a concurrency ladder rather than a fixed rate.
//
// The rest of `iplane load` is open loop: a ticker offers requests at a
// rate the operator names, and what the engine does with them is the
// measurement. That is the right shape for asking whether a deployment
// holds up under a known arrival rate, and the wrong shape for asking
// what an engine's throughput curve looks like, because past saturation
// an open loop just accumulates a queue and every latency number becomes
// a statement about how long the run was.
//
// A closed loop asks the other question. Hold exactly N requests in
// flight, replace each one as it completes, and the arrival rate becomes
// an output. Throughput against N is then the curve the cost model needs,
// since cost per token is the rented hour divided by the tokens that hour
// produced and N is the only knob that moves the denominator.

var (
	loadSweepLevels     string
	loadSweepDuration   time.Duration
	loadSweepWarmupMax  time.Duration
	loadSweepWindow     time.Duration
	loadSweepTolerance  float64
	loadSweepStableRuns int

	// A flat window per level is the wrong shape when the levels differ by
	// an order of magnitude in latency. The 8k sweep earned 558 samples in
	// its 600s and the 120k sweep earned 6 in the same 600s, because the
	// window was chosen before anything was known about how long a request
	// takes. After warm-up the completion rate is known, so the window can
	// be sized to earn a sample count instead of a stopwatch reading.
	loadSweepMinSamples  int
	loadSweepDurationMax time.Duration
)

// sweepLevel is one point on the ladder, after measurement.
type sweepLevel struct {
	Concurrency int `json:"concurrency"`

	// AchievedBatch is throughput times mean latency, Little's law
	// against the offered level. It should land on Concurrency, and it is
	// reported because the interesting case is when it does not.
	//
	// Two ways it sags. Workers can spend part of the window erroring
	// rather than holding a request in flight. More often, and more
	// usefully, the requests still in flight when the window closes are
	// never recorded, and they are disproportionately the slow ones, so
	// a level whose latencies are long relative to --sweep-duration
	// undercounts. Both mean the row is describing a smaller batch than
	// its label, and both are a reason to lengthen the window before
	// trusting the numbers beside it. A level queueing at the engine's
	// admission gate shows exactly this.
	AchievedBatch float64 `json:"achieved_batch"`

	RequestsPerSec float64 `json:"requests_per_sec"`
	TokensPerSec   float64 `json:"tokens_per_sec"`

	Successes int64 `json:"successes"`
	// TruncatedRequests counts responses the measurement window closed on
	// before the engine finished them. Separate from both Successes and
	// Errors: nothing failed, and the request is not an observation of a
	// completed one. A large value beside a small Successes says the level
	// was too short for its context length rather than that the engine
	// was slow.
	TruncatedRequests int64 `json:"truncated_requests"`
	Errors            int64 `json:"errors"`
	Tokens            int64 `json:"completion_tokens"`

	LatencyP50Ms int64 `json:"latency_p50_ms"`
	LatencyP95Ms int64 `json:"latency_p95_ms"`

	TTFTSamples int64 `json:"ttft_samples"`
	TTFTP50Ms   int64 `json:"ttft_p50_ms"`
	TTFTP95Ms   int64 `json:"ttft_p95_ms"`

	// Inter-token gaps are quoted with a decimal where the other
	// latencies are whole milliseconds, because the quantity is an order
	// of magnitude smaller. Rounding a 0.4ms gap to zero would report a
	// fast engine and an unpaced mock as the same thing.
	ITLSamples int64   `json:"itl_samples"`
	ITLP50Ms   float64 `json:"itl_p50_ms"`
	ITLP95Ms   float64 `json:"itl_p95_ms"`

	// WarmupSec is how long the level ran before its throughput settled,
	// and DiscardedRequests is what completed during it. Both are
	// reported rather than logged, because a reader comparing two sweeps
	// needs to know what was excluded to know whether the comparison
	// holds.
	WarmupSec float64 `json:"warmup_sec"`

	// MeasureSec is how long this level's window actually ran. Constant
	// across a sweep until --sweep-min-samples is set, after which levels
	// get different windows and a reader comparing two rows cannot infer
	// either one from the run header.
	MeasureSec float64 `json:"measure_sec"`

	DiscardedRequests int64 `json:"discarded_requests"`

	// SteadyState is false when the level hit --sweep-warmup-max with its
	// throughput still drifting. The row is still reported, since a
	// measurement taken before things settled is more useful than a gap,
	// but it is not the same kind of number as the rows around it.
	SteadyState bool `json:"steady_state"`
}

// sweepProgress is where a running sweep narrates itself. Stderr, never the
// writer the artifact goes to, because the artifact is captured by redirect
// and one stray line corrupts a file that costs thousands of dollars of
// rented hardware to reproduce.
//
// A variable rather than a parameter so tests can read what was said without
// every helper in this file growing a writer argument. Reset by
// resetSweepFlags alongside the other package-level knobs.
var sweepProgress io.Writer = os.Stderr

// progressf writes one line of narration.
func progressf(format string, args ...any) {
	fmt.Fprintf(sweepProgress, format+"\n", args...)
}

// sweepReport is the machine-readable form, and the artifact a figure
// gets drawn from.
//
// Part IV's figures cost thousands of dollars of rented hardware to
// reproduce, so a number that reaches a caption by being retyped out of a
// terminal is a number nobody can check. Everything here is written so a
// figure regenerates from the file: the measurements, and enough about
// the run to say what was measured and on what.
//
// SchemaVersion exists because the book will read files produced by
// whatever iplane looked like when each run happened. Bump it when a
// column changes meaning, and never when one is added.
type sweepReport struct {
	SchemaVersion int    `json:"schema_version"`
	CapturedAt    string `json:"captured_at"`
	IplaneVersion string `json:"iplane_version"`

	Model    string `json:"model"`
	Endpoint string `json:"endpoint"`
	DeployID string `json:"deploy_id,omitempty"`

	// What the control plane knows about the hardware. Empty when the
	// sweep drove a bare URL, since then nothing was asked.
	Fleet fleetProvenance `json:"fleet"`

	// The request shape, which sets where the concurrency ceiling lands.
	// PromptTokens especially: the ceiling falls as context rises, so a
	// curve without it is a curve nobody can place.
	PromptTokens int  `json:"prompt_tokens"`
	MaxTokens    int  `json:"max_tokens"`
	Stream       bool `json:"stream"`

	// The method, so a reader can tell a settled measurement from a
	// hurried one without being told.
	// MeasureSeconds is the floor once MinSamples is set, not the window
	// every level ran; each row carries its own measure_sec.
	MeasureSeconds    float64 `json:"measure_seconds"`
	MinSamples        int     `json:"min_samples"`
	MaxMeasureSeconds float64 `json:"max_measure_seconds"`
	WindowSeconds     float64 `json:"window_seconds"`
	Tolerance         float64 `json:"tolerance"`
	StableWindows     int     `json:"stable_windows"`

	Levels []sweepLevel `json:"levels"`
}

// sweepSchemaVersion is the artifact's contract with the book.
const sweepSchemaVersion = 1

// parseSweepLevels reads the ladder.
//
// Sorted ascending and de-duplicated, because the curve is read down the
// page and a ladder that doubles back reads as two curves. Repeating a
// level to measure it twice is a job for two runs, which is also how the
// A/B comparator in demo 09e already treats repetition.
func parseSweepLevels(s string) ([]int, error) {
	var out []int
	seen := map[int]bool{}
	for _, field := range strings.Split(s, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		n, err := strconv.Atoi(field)
		if err != nil {
			return nil, fmt.Errorf("--sweep: %q is not a concurrency level", field)
		}
		if n < 1 {
			return nil, fmt.Errorf("--sweep: concurrency must be at least 1, got %d", n)
		}
		if seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("--sweep: no concurrency levels given")
	}
	sort.Ints(out)
	return out, nil
}

// steadyDetector decides when a level has settled.
//
// A fixed warm-up sleep is the usual approach and it is wrong in both
// directions at once: too short at the levels that take longest to fill
// the engine's batch, and pure waste at the levels that settle in two
// seconds. This watches the thing that actually has to stop moving.
//
// The rule is that the last stableRuns windows all sit within tolerance
// of their own mean. Comparing against the running mean rather than
// against the previous window is what keeps a slow monotonic climb from
// reading as stable: consecutive windows during a gradual ramp are always
// close to each other, and are not close to where the level started.
type steadyDetector struct {
	tolerance  float64
	stableRuns int
	window     time.Duration
	counts     []int64
}

func newSteadyDetector(tolerance float64, stableRuns int, window time.Duration) *steadyDetector {
	return &steadyDetector{tolerance: tolerance, stableRuns: stableRuns, window: window}
}

// observe records one window's completion count and reports whether the
// level has settled.
//
// A window that completed nothing is recorded rather than skipped. At a
// high concurrency with long requests it is a real observation, and
// dropping it would let a level producing nothing at all look settled
// once two later windows agreed with each other.
//
// The tolerance has a floor of one completion per window, which is the
// finest change the sample can express. Without it a level running at
// five requests a second against a one-second window never settles: the
// count alternates between four and five, that is a twenty percent swing,
// and no amount of waiting makes an integer count land on 4.5. Widening
// the window is the other fix and it is the operator's to choose; this
// one stops the tool from reporting quantisation as instability.
func (d *steadyDetector) observe(count int64) bool {
	d.counts = append(d.counts, count)
	if len(d.counts) > d.stableRuns {
		d.counts = d.counts[len(d.counts)-d.stableRuns:]
	}
	if len(d.counts) < d.stableRuns {
		return false
	}

	var sum int64
	for _, c := range d.counts {
		sum += c
	}
	mean := float64(sum) / float64(len(d.counts))
	if mean <= 0 {
		// Nothing completed across the whole window set. Not settled;
		// this is the shape of an engine that has stopped answering, and
		// calling it steady would report a zero-throughput level as a
		// measurement rather than as a failure.
		return false
	}

	tolerance := math.Max(d.tolerance, 1/mean)
	for _, c := range d.counts {
		if math.Abs(float64(c)-mean)/mean > tolerance {
			return false
		}
	}
	return true
}

// rate converts a window's count into completions per second, which is
// what the level's throughput reading is quoted in.
func (d *steadyDetector) rate(count int64) float64 {
	if d.window <= 0 {
		return 0
	}
	return float64(count) / d.window.Seconds()
}

// runLoadSweep walks the ladder, measuring each level once it settles.
func runLoadSweep(ctx context.Context, out io.Writer) error {
	levels, err := parseSweepLevels(loadSweepLevels)
	if err != nil {
		return err
	}
	if loadSweepDuration <= 0 {
		return fmt.Errorf("--sweep-duration must be positive")
	}
	if loadSweepWindow <= 0 {
		return fmt.Errorf("--sweep-window must be positive")
	}
	if loadSweepTolerance <= 0 || loadSweepTolerance >= 1 {
		return fmt.Errorf("--sweep-tolerance must be between 0 and 1, got %g", loadSweepTolerance)
	}
	if loadSweepStableRuns < 2 {
		return fmt.Errorf("--sweep-stable-windows must be at least 2, since one window cannot be compared to anything")
	}
	if loadSweepMinSamples < 0 {
		return fmt.Errorf("--sweep-min-samples cannot be negative")
	}
	// The cap is required rather than defaulted. Sizing a window from a
	// measured rate is the same thing as letting the engine decide how long
	// a rented GPU is held, and a default multiplier would be this tool
	// picking a number nobody agreed to spend.
	if loadSweepMinSamples > 0 && loadSweepDurationMax <= 0 {
		return fmt.Errorf("--sweep-min-samples needs --sweep-duration-max, which is the longest any one level may hold the hardware")
	}
	if loadSweepDurationMax > 0 && loadSweepDurationMax < loadSweepDuration {
		return fmt.Errorf("--sweep-duration-max (%s) is below --sweep-duration (%s); the floor cannot exceed the ceiling", loadSweepDurationMax, loadSweepDuration)
	}

	base, chatPath, completionsPath, err := loadEndpoint()
	if err != nil {
		return err
	}
	cfg := loadFireConfig{
		base:            base,
		chatPath:        chatPath,
		completionsPath: completionsPath,
		stream:          loadStream,
		priority:        loadPriority,
		tenant:          loadTenant,
	}
	client := &http.Client{Timeout: 5 * time.Minute}

	if loadSweepMinSamples > 0 {
		progressf("iplane load --sweep: levels %v, %s per level after steady state, extended toward %d samples and capped at %s -> %s",
			levels, loadSweepDuration, loadSweepMinSamples, loadSweepDurationMax, base)
	} else {
		progressf("iplane load --sweep: levels %v, %s per level after steady state -> %s",
			levels, loadSweepDuration, base)
	}

	report := sweepReport{
		SchemaVersion:     sweepSchemaVersion,
		CapturedAt:        time.Now().UTC().Format(time.RFC3339),
		IplaneVersion:     version.Version,
		Model:             loadModel,
		Endpoint:          base,
		DeployID:          loadTarget,
		PromptTokens:      loadPromptTokens,
		MaxTokens:         loadMaxTokens,
		Stream:            loadStream,
		MeasureSeconds:    loadSweepDuration.Seconds(),
		MinSamples:        loadSweepMinSamples,
		MaxMeasureSeconds: loadSweepDurationMax.Seconds(),
		WindowSeconds:     loadSweepWindow.Seconds(),
		Tolerance:         loadSweepTolerance,
		StableWindows:     loadSweepStableRuns,
		Fleet:             sweepFleetProvenance(ctx),
	}
	for i, n := range levels {
		lvl, err := measureSweepLevel(ctx, client, &cfg, n)
		if err != nil {
			return err
		}
		reportSweepLevel(i+1, len(levels), lvl)
		report.Levels = append(report.Levels, lvl)
		if ctx.Err() != nil {
			progressf("interrupted after %d concurrent; reporting what completed", n)
			break
		}
	}

	switch loadOutput {
	case outputJSON:
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	case outputCSV:
		return writeSweepCSV(out, report)
	}
	writeSweepTable(os.Stderr, report)
	return nil
}

// reportSweepLevel narrates one finished level.
//
// The fields are chosen so a reader can tell a healthy level from a broken
// one without waiting for the table at the end. Request count separates "slow
// but working" from "nothing is happening", which is the distinction a closed
// port hides: a sweep firing into a refused connection produces no traffic and
// looks exactly like a level still warming up. Whether it settled says whether
// the row beside it is a measurement or a snapshot of a ramp.
func reportSweepLevel(idx, total int, l sweepLevel) {
	settled := fmt.Sprintf("settled after %.0fs", l.WarmupSec)
	if !l.SteadyState {
		settled = fmt.Sprintf("never settled in %.0fs", l.WarmupSec)
	}
	line := fmt.Sprintf("level %d/%d (n=%d): %s, %d requests, %.2f req/s, %.1f tok/s",
		idx, total, l.Concurrency, settled, l.Successes, l.RequestsPerSec, l.TokensPerSec)
	if l.LatencyP50Ms > 0 {
		line += fmt.Sprintf(", p50 %dms", l.LatencyP50Ms)
	}
	if l.Errors > 0 {
		line += fmt.Sprintf(", %d errors", l.Errors)
	}
	progressf("%s", line)
}

// sweepFleetProvenance asks the control plane what hardware this sweep is
// about to measure.
//
// Only in --target mode, because that is the only mode where a deployment
// id exists to ask about. A sweep against a bare URL leaves the fields
// empty, which reads as "nobody asked" rather than as a claim.
//
// Failure is reported and then ignored. A sweep about to spend an hour of
// rented time must not be refused because a metadata read did not answer,
// and a file missing its hardware block is recoverable in a way a run
// that never happened is not.
func sweepFleetProvenance(ctx context.Context) fleetProvenance {
	if loadTarget == "" || loadServiceURL == "" {
		return fleetProvenance{}
	}
	base := strings.TrimRight(loadServiceURL, "/")
	dc := &connectDeploymentClient{c: provisionerv1connect.NewDeploymentServiceClient(http.DefaultClient, base)}
	ic := &connectProvisionerClient{c: provisionerv1connect.NewProvisionerServiceClient(http.DefaultClient, base)}

	fleet, err := describeFleet(ctx, dc, ic, loadTarget)
	if err != nil {
		progressf("warning: could not describe %q, the artifact will name no hardware: %v", loadTarget, err)
		return fleetProvenance{}
	}
	return fleet
}

// measureSweepLevel holds exactly n requests in flight, waits for the
// throughput to settle, then measures for the configured window.
//
// The workers keep running across the boundary rather than restarting
// after warm-up. Restarting would drain the engine's batch and hand the
// measured window the same ramp the warm-up just finished discarding.
func measureSweepLevel(ctx context.Context, client *http.Client, cfg *loadFireConfig, n int) (sweepLevel, error) {
	var completed atomic.Int64

	stats := &loadStats{}
	// warmStats exists so the warm-up can be narrated. It is a second
	// object rather than an early switch-on, so `stats` still never sees a
	// warm-up request and the discard is exactly what it was.
	//
	// It is needed because the completion counter alone lies about the case
	// this narration exists for. A refused connection returns instantly, so
	// a sweep firing at a closed port completes tens of thousands of
	// attempts per window, reports a stable rate, and settles. The number
	// looks healthier than a working engine's. Successes and errors are the
	// pair that tells them apart.
	warmStats := &loadStats{}
	var statsOn atomic.Bool
	recording := func() *loadStats {
		if statsOn.Load() {
			return stats
		}
		return warmStats
	}

	levelCtx, stop := context.WithCancel(ctx)
	defer stop()

	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for levelCtx.Err() == nil {
				// A discarded request still has to be fired, since the
				// point of the warm-up is that the engine is under load
				// throughout it. Only the recording is conditional, and
				// a nil *loadStats is what discards it.
				//
				// A request that started during warm-up and finished
				// after it counts, which is correct: it completed inside
				// the measured window and the engine was already at this
				// level when it started.
				fireLoadRequest(levelCtx, client, cfg, recording())
				completed.Add(1)
			}
		}()
	}

	warmupStart := time.Now()
	discarded, steady, okRate := awaitSteadyState(levelCtx, &completed, warmStats, n)
	warmup := time.Since(warmupStart)

	statsOn.Store(true)
	measureStart := time.Now()
	awaitMeasureWindow(levelCtx, stats, n, okRate)
	elapsed := time.Since(measureStart)
	stop()
	wg.Wait()

	return summariseSweepLevel(n, stats, elapsed, warmup, discarded, steady), nil
}

// describeProgress renders the running counts, naming errors only when there
// are some.
//
// Cumulative rather than per-window, because the number an operator is
// deciding on is whether anything has worked at all. A per-window figure of
// zero successes is ambiguous during a slow level; a cumulative zero after
// four minutes is not.
func describeProgress(successes, errors int64) string {
	if errors == 0 {
		return fmt.Sprintf("%d ok", successes)
	}
	return fmt.Sprintf("%d ok, %d errors", successes, errors)
}

// settledSuffix labels a warm-up window with what the detector made of it.
func settledSuffix(settled bool) string {
	if settled {
		return " (settled)"
	}
	return ""
}

// projectedMeasureWindow estimates how long a level needs to earn
// --sweep-min-samples, from the rate its warm-up settled at.
//
// An estimate and nothing more. It is narrated so an operator watching a paid
// run knows whether the next level is two minutes or fifteen, and the window
// itself is ended by counting samples rather than by trusting this number. A
// warm-up rate projects badly whenever latency is heavy-tailed: a local
// rehearsal against the mock projected 30s for a level that went on to earn
// four samples in it, because the warm-up happened to catch the fast mode.
func projectedMeasureWindow(okRatePerSec float64) time.Duration {
	if loadSweepMinSamples <= 0 || okRatePerSec <= 0 {
		return loadSweepDuration
	}
	need := time.Duration(float64(loadSweepMinSamples) / okRatePerSec * float64(time.Second))
	if need < loadSweepDuration {
		return loadSweepDuration
	}
	if loadSweepDurationMax > 0 && need > loadSweepDurationMax {
		return loadSweepDurationMax
	}
	return need
}

// awaitMeasureWindow holds the level open until it has been measured, and
// narrates as it goes.
//
// Without --sweep-min-samples this is --sweep-duration and nothing else, which
// is the historical behaviour. With it, the window runs until the level has
// completed the requested number of successes, floored at --sweep-duration so
// it can never come back shorter than what was asked for and capped at
// --sweep-duration-max so it can never run away with the hardware.
//
// Counting samples beats projecting them from the warm-up rate. The projection
// is one number taken before the window starts, and it is wrong in exactly the
// case the sample target exists for: a heavy-tailed latency distribution whose
// warm-up caught the fast mode. Counting is a measurement of the thing that
// actually has to be earned.
//
// This was one silent sleep, and it is the longer of a level's two phases on
// the real settings. Ticking on the same window the warm-up uses keeps the
// cadence uniform, so an operator watching the log sees the same rhythm
// throughout a level rather than a burst and then nothing.
//
// Elapsed time is still measured by the caller from before this call to after
// it, so the ticking changes what is reported not at all.
func awaitMeasureWindow(ctx context.Context, stats *loadStats, n int, okRate float64) {
	floor, ceiling := loadSweepDuration, loadSweepDuration
	if loadSweepMinSamples > 0 {
		ceiling = loadSweepDurationMax
		progressf("  n=%d measuring until %d samples: floor %s, cap %s, roughly %s at the warm-up rate of %.2f req/s",
			n, loadSweepMinSamples, floor, ceiling,
			projectedMeasureWindow(okRate).Round(time.Second), okRate)
	}

	start := time.Now()
	tick := loadSweepWindow
	if tick <= 0 || tick > floor {
		tick = floor
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		elapsed := time.Since(start)
		ok, failed := stats.snapshot()
		if elapsed >= ceiling {
			return
		}
		if elapsed >= floor && (loadSweepMinSamples <= 0 || ok >= int64(loadSweepMinSamples)) {
			return
		}
		if loadSweepMinSamples > 0 {
			progressf("  n=%d measuring: %.0fs elapsed of at most %.0fs, %d of %d samples, %s",
				n, elapsed.Seconds(), ceiling.Seconds(), ok, loadSweepMinSamples,
				describeProgress(ok, failed))
			continue
		}
		progressf("  n=%d measuring: %.0fs of %.0fs, %s",
			n, elapsed.Seconds(), floor.Seconds(), describeProgress(ok, failed))
	}
}

// awaitSteadyState samples completion counts until the rate settles or
// --sweep-warmup-max runs out. Returns what completed during warm-up, whether
// it settled, and the rate it settled at in successes per second.
//
// The returned rate counts successes rather than the completions the detector
// watches, and the difference is the whole point of quoting it. A refused
// connection completes instantly, so a closed port produces a completion rate
// that is enormous and perfectly stable. Sizing the measurement window from
// that number would hand the shortest window to the run that measured
// nothing. Successes are what the window has to earn, so successes are what
// it is sized from.
func awaitSteadyState(ctx context.Context, completed *atomic.Int64, warm *loadStats, n int) (int64, bool, float64) {
	det := newSteadyDetector(loadSweepTolerance, loadSweepStableRuns, loadSweepWindow)
	deadline := time.Now().Add(loadSweepWarmupMax)
	ticker := time.NewTicker(loadSweepWindow)
	defer ticker.Stop()

	// The success deltas of the same windows the detector retains, so the
	// rate describes the settled tail rather than the ramp that preceded it.
	var okDeltas []int64
	lastOK, _ := warm.snapshot()

	last := completed.Load()
	for {
		select {
		case <-ctx.Done():
			return completed.Load(), false, meanPerSec(okDeltas, loadSweepWindow)
		case <-ticker.C:
		}
		now := completed.Load()
		count := now - last
		last = now
		settled := det.observe(count)
		// Successes and errors, not the completion count the detector uses.
		// A refused connection completes instantly, so the counter spins and
		// the rate settles: at a closed port this line would otherwise read
		// "23439 requests in the last 2s" and look better than a working
		// engine. The error column is what makes the two distinguishable
		// while there is still money to save.
		ok, failed := warm.snapshot()
		okDeltas = append(okDeltas, ok-lastOK)
		if len(okDeltas) > loadSweepStableRuns {
			okDeltas = okDeltas[len(okDeltas)-loadSweepStableRuns:]
		}
		lastOK = ok
		progressf("  n=%d warming up: %s%s",
			n, describeProgress(ok, failed), settledSuffix(settled))
		if settled {
			return now, true, meanPerSec(okDeltas, loadSweepWindow)
		}
		if time.Now().After(deadline) {
			// Not settled, so the rate is a reading of a level still
			// moving. Returned anyway: it only feeds the narrated estimate,
			// and the window itself ends on the sample count.
			return now, false, meanPerSec(okDeltas, loadSweepWindow)
		}
	}
}

// meanPerSec averages per-window counts into a rate, and returns zero when
// there is nothing to average. Zero is read downstream as no reading.
func meanPerSec(counts []int64, window time.Duration) float64 {
	if len(counts) == 0 || window <= 0 {
		return 0
	}
	var sum int64
	for _, c := range counts {
		sum += c
	}
	return float64(sum) / float64(len(counts)) / window.Seconds()
}

// summariseSweepLevel turns one level's samples into its row.
func summariseSweepLevel(n int, s *loadStats, elapsed, warmup time.Duration, discarded int64, steady bool) sweepLevel {
	s.mu.Lock()
	defer s.mu.Unlock()

	lvl := sweepLevel{
		Concurrency:       n,
		Successes:         s.successes,
		TruncatedRequests: s.truncated,
		Errors:            s.errors,
		Tokens:            s.tokens,
		WarmupSec:         warmup.Seconds(),
		MeasureSec:        elapsed.Seconds(),
		DiscardedRequests: discarded,
		SteadyState:       steady,
	}
	if secs := elapsed.Seconds(); secs > 0 {
		lvl.RequestsPerSec = float64(s.successes) / secs
		lvl.TokensPerSec = float64(s.tokens) / secs
	}
	if len(s.latencies) > 0 {
		var total time.Duration
		for _, d := range s.latencies {
			total += d
		}
		mean := total.Seconds() / float64(len(s.latencies))
		// Little's law. With a closed loop this reconstructs the offered
		// concurrency, which is the point: it is a check that the level
		// ran as labelled rather than a new measurement.
		lvl.AchievedBatch = lvl.RequestsPerSec * mean
		lvl.LatencyP50Ms = quantile(s.latencies, 0.50).Milliseconds()
		lvl.LatencyP95Ms = quantile(s.latencies, 0.95).Milliseconds()
	}
	if len(s.ttfts) > 0 {
		lvl.TTFTSamples = int64(len(s.ttfts))
		lvl.TTFTP50Ms = quantile(s.ttfts, 0.50).Milliseconds()
		lvl.TTFTP95Ms = quantile(s.ttfts, 0.95).Milliseconds()
	}
	if len(s.itls) > 0 {
		lvl.ITLSamples = int64(len(s.itls))
		lvl.ITLP50Ms = float64(quantile(s.itls, 0.50).Microseconds()) / 1000
		lvl.ITLP95Ms = float64(quantile(s.itls, 0.95).Microseconds()) / 1000
	}
	return lvl
}

// writeSweepTable prints the curve.
func writeSweepTable(w io.Writer, r sweepReport) {
	fmt.Fprintf(w, "\n=== iplane load sweep ===\n")
	fmt.Fprintf(w, "model %s, max-tokens %d, %.0fs measured per level after steady state\n\n", r.Model, r.MaxTokens, r.MeasureSeconds)

	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', tabwriter.AlignRight)
	fmt.Fprintf(tw, "conc\tbatch\treq/s\ttok/s\tlat p50\tlat p95\tttft p50\tttft p95\titl p50\terrors\twarmup\tdiscarded\t   steady\n")
	for _, l := range r.Levels {
		fmt.Fprintf(tw, "%d\t%.1f\t%.2f\t%.1f\t%s\t%s\t%s\t%s\t%s\t%d\t%.0fs\t%d\t%s\n",
			l.Concurrency, l.AchievedBatch, l.RequestsPerSec, l.TokensPerSec,
			msOrDash(l.LatencyP50Ms), msOrDash(l.LatencyP95Ms),
			msOrDash(l.TTFTP50Ms), msOrDash(l.TTFTP95Ms), fracMsOrDash(l.ITLP50Ms),
			l.Errors, l.WarmupSec, l.DiscardedRequests, "   "+steadyLabel(l.SteadyState))
	}
	_ = tw.Flush()

	fmt.Fprintln(w)
	if !r.Stream {
		fmt.Fprintln(w, "ttft and itl are blank without --stream: a non-streamed reply arrives in one piece,")
		fmt.Fprintln(w, "so its first token and its last land at the same instant.")
	}
	for _, l := range r.Levels {
		if l.AchievedBatch > 0 && l.AchievedBatch < float64(l.Concurrency)*0.9 {
			fmt.Fprintf(w, "level %d sustained a batch of %.1f against the %d offered; its requests are outlasting the measurement window.\n",
				l.Concurrency, l.AchievedBatch, l.Concurrency)
		}
		if !l.SteadyState {
			fmt.Fprintf(w, "level %d never settled within --sweep-warmup-max; its row was measured mid-ramp.\n", l.Concurrency)
		}
		if note := inconsistencyNote(l); note != "" {
			fmt.Fprintf(w, "level %d %s\n", l.Concurrency, note)
		}
	}
}

// tokenDisagreementFactor is how far the two independent token counts may
// diverge before the row is called contradictory.
//
// Generous on purpose. The two are counted differently (a usage total against
// a frame tally) and neither is exact, so a small gap is ordinary. The failure
// worth catching is not subtle: a 120k level reported 6.2 tokens per request
// from usage while its frames implied 103, a factor of 17, and every published
// figure derived from the first number (#451).
const tokenDisagreementFactor = 3.0

// inconsistencyNote reports when a level's own columns contradict each other,
// and returns "" when they agree or cannot be compared.
//
// A sweep row carries two independent counts of the same quantity. Tokens come
// from the engine's usage block; inter-token gaps come from counting frames as
// they arrive. On a level that measured what it claims, tokens-per-request and
// gaps-per-request land within a few percent of each other. When they diverge
// by a large factor, one of the two is describing something other than the
// engine's output, and the row cannot be trusted whichever way it resolves.
//
// This is a cross-check rather than a threshold: it needs no notion of what a
// correct value looks like, only that two measurements of one thing should
// agree. That is what makes it useful against defects nobody predicted, which
// is the category that has cost the most here.
func inconsistencyNote(l sweepLevel) string {
	if l.Successes <= 0 || l.TTFTSamples <= 0 || l.ITLSamples <= 0 {
		return ""
	}
	fromUsage := float64(l.Tokens) / float64(l.Successes)
	// A request of n tokens yields n-1 gaps between them, so the frame tally
	// is put back into token units before the two are compared. Correcting
	// the relation is what lets the check run on every row: the previous
	// version excused any row whose per-request count fell under four, which
	// is the shape a pathological row has rather than a benign one. The 120k
	// N=32 level reported 1.0 token per request from usage against 61.6 from
	// frames and the floor waved it through, because one side was small.
	fromFrames := float64(l.ITLSamples)/float64(l.TTFTSamples) + 1
	if fromUsage <= 0 {
		return fmt.Sprintf(
			"disagrees with itself: usage reported no completion tokens while streamed frames imply %.1f per request. "+
				"One of the two is not the engine's output; do not chart this row.", fromFrames)
	}
	hi, lo := fromUsage, fromFrames
	if lo > hi {
		hi, lo = lo, hi
	}
	if hi/lo < tokenDisagreementFactor {
		return ""
	}
	return fmt.Sprintf(
		"disagrees with itself: %.1f tokens per request from usage against %.1f implied by streamed frames (%.0fx). "+
			"One of the two is not the engine's output; do not chart this row.",
		fromUsage, fromFrames, hi/lo)
}

// msOrDash keeps an unmeasured cell distinguishable from a fast one.
func msOrDash(ms int64) string {
	if ms <= 0 {
		return "-"
	}
	return fmt.Sprintf("%dms", ms)
}

// fracMsOrDash is msOrDash with a decimal, for the inter-token column.
func fracMsOrDash(ms float64) string {
	if ms <= 0 {
		return "-"
	}
	return fmt.Sprintf("%.1fms", ms)
}

func steadyLabel(ok bool) string {
	if ok {
		return "yes"
	}
	return "no"
}

// sweepCSVColumns and sweepCSVRow derive the CSV shape from sweepLevel's
// own json tags, in declaration order.
//
// Reflected rather than written out twice so the two artifacts cannot
// disagree. A field added to sweepLevel appears in both files with the
// same name, and a field renamed in one is renamed in both, which is the
// property that lets a figure read either format and get the same
// columns. Adding a column is safe for a reader keyed by name; changing
// what one means is what sweepSchemaVersion is for.
func sweepCSVColumns() []string {
	t := reflect.TypeOf(sweepLevel{})
	out := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		out = append(out, jsonFieldName(t.Field(i)))
	}
	return out
}

func sweepCSVRow(l sweepLevel) []string {
	v := reflect.ValueOf(l)
	out := make([]string, 0, v.NumField())
	for i := 0; i < v.NumField(); i++ {
		out = append(out, csvScalar(v.Field(i)))
	}
	return out
}

// jsonFieldName reads the tag, falling back to the Go name for a field
// that never got one.
func jsonFieldName(f reflect.StructField) string {
	tag := f.Tag.Get("json")
	if tag == "" {
		return f.Name
	}
	if i := strings.IndexByte(tag, ','); i >= 0 {
		tag = tag[:i]
	}
	if tag == "" {
		return f.Name
	}
	return tag
}

// csvScalar renders one value. Floats use the shortest representation
// that round-trips, so a figure reads the measured number rather than a
// rounded one, and an integral float does not grow a decimal point.
func csvScalar(v reflect.Value) string {
	switch v.Kind() {
	case reflect.Bool:
		return strconv.FormatBool(v.Bool())
	case reflect.Int, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(v.Int(), 10)
	case reflect.Float32, reflect.Float64:
		return strconv.FormatFloat(v.Float(), 'f', -1, 64)
	default:
		return fmt.Sprint(v.Interface())
	}
}

// writeSweepCSV emits the ladder as one row per level, with the run's
// provenance as leading comment lines.
//
// Comments rather than extra columns, because the provenance is constant
// down the file and pgfplots wants a rectangle of numbers. Read it with
// `comment chars=#`, which is the default for \pgfplotstableread.
func writeSweepCSV(w io.Writer, r sweepReport) error {
	for _, line := range sweepCSVPreamble(r) {
		if _, err := fmt.Fprintf(w, "# %s\n", line); err != nil {
			return err
		}
	}
	cw := csv.NewWriter(w)
	if err := cw.Write(sweepCSVColumns()); err != nil {
		return err
	}
	for _, lvl := range r.Levels {
		if err := cw.Write(sweepCSVRow(lvl)); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// sweepCSVPreamble is the provenance block, as `key value` lines. Same
// facts the JSON carries as fields, in a form a comment-stripping reader
// ignores and a human reading the file head does not.
func sweepCSVPreamble(r sweepReport) []string {
	lines := []string{
		fmt.Sprintf("schema_version %d", r.SchemaVersion),
		fmt.Sprintf("captured_at %s", r.CapturedAt),
		fmt.Sprintf("iplane_version %s", r.IplaneVersion),
		fmt.Sprintf("model %s", r.Model),
		fmt.Sprintf("endpoint %s", r.Endpoint),
	}
	if r.DeployID != "" {
		lines = append(lines, fmt.Sprintf("deploy_id %s", r.DeployID))
	}
	if r.Fleet.Provider != "" {
		lines = append(lines, fmt.Sprintf("provider %s", r.Fleet.Provider))
	}
	if r.Fleet.GPUSKU != "" {
		lines = append(lines, fmt.Sprintf("gpu_sku %s", r.Fleet.GPUSKU))
	}
	if r.Fleet.GPUCount > 0 {
		lines = append(lines, fmt.Sprintf("gpu_count %d", r.Fleet.GPUCount))
	}
	if r.Fleet.Replicas > 0 {
		lines = append(lines, fmt.Sprintf("replicas %d", r.Fleet.Replicas))
	}
	if r.Fleet.Plan != "" {
		lines = append(lines, fmt.Sprintf("plan %s", r.Fleet.Plan))
	}
	return append(lines,
		fmt.Sprintf("prompt_tokens %d", r.PromptTokens),
		fmt.Sprintf("max_tokens %d", r.MaxTokens),
		fmt.Sprintf("stream %t", r.Stream),
		fmt.Sprintf("measure_seconds %g", r.MeasureSeconds),
		fmt.Sprintf("window_seconds %g", r.WindowSeconds),
		fmt.Sprintf("tolerance %g", r.Tolerance),
		fmt.Sprintf("stable_windows %d", r.StableWindows),
	)
}
