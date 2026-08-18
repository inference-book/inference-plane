package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"text/tabwriter"
	"time"
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
)

// sweepLevel is one point on the ladder, after measurement.
type sweepLevel struct {
	Concurrency int `json:"concurrency"`

	// AchievedBatch is throughput times mean latency, which is the
	// concurrency the run actually sustained. It should land on
	// Concurrency and is reported because the interesting case is when it
	// does not: a level whose achieved batch sags below its nominal one
	// spent part of the window with workers erroring out rather than
	// holding a request in flight, and every per-token figure on that row
	// is then describing a smaller batch than its label claims.
	AchievedBatch float64 `json:"achieved_batch"`

	RequestsPerSec float64 `json:"requests_per_sec"`
	TokensPerSec   float64 `json:"tokens_per_sec"`

	Successes int64 `json:"successes"`
	Errors    int64 `json:"errors"`
	Tokens    int64 `json:"completion_tokens"`

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
	WarmupSec         float64 `json:"warmup_sec"`
	DiscardedRequests int64   `json:"discarded_requests"`

	// SteadyState is false when the level hit --sweep-warmup-max with its
	// throughput still drifting. The row is still reported, since a
	// measurement taken before things settled is more useful than a gap,
	// but it is not the same kind of number as the rows around it.
	SteadyState bool `json:"steady_state"`
}

// sweepReport is the machine-readable form, and the artifact a figure
// gets drawn from.
type sweepReport struct {
	Model          string       `json:"model"`
	Endpoint       string       `json:"endpoint"`
	MaxTokens      int          `json:"max_tokens"`
	Stream         bool         `json:"stream"`
	MeasureSeconds float64      `json:"measure_seconds"`
	Levels         []sweepLevel `json:"levels"`
}

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

	fmt.Fprintf(os.Stderr, "iplane load --sweep: levels %v, %s per level after steady state -> %s\n",
		levels, loadSweepDuration, base)

	report := sweepReport{
		Model:          loadModel,
		Endpoint:       base,
		MaxTokens:      loadMaxTokens,
		Stream:         loadStream,
		MeasureSeconds: loadSweepDuration.Seconds(),
	}
	for _, n := range levels {
		lvl, err := measureSweepLevel(ctx, client, &cfg, n)
		if err != nil {
			return err
		}
		report.Levels = append(report.Levels, lvl)
		if ctx.Err() != nil {
			fmt.Fprintf(os.Stderr, "interrupted after %d concurrent; reporting what completed\n", n)
			break
		}
	}

	if loadOutput == outputJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}
	writeSweepTable(os.Stderr, report)
	return nil
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
	var statsOn atomic.Bool
	recording := func() *loadStats {
		if statsOn.Load() {
			return stats
		}
		return nil
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
	discarded, steady := awaitSteadyState(levelCtx, &completed)
	warmup := time.Since(warmupStart)

	statsOn.Store(true)
	measureStart := time.Now()
	select {
	case <-time.After(loadSweepDuration):
	case <-levelCtx.Done():
	}
	elapsed := time.Since(measureStart)
	stop()
	wg.Wait()

	return summariseSweepLevel(n, stats, elapsed, warmup, discarded, steady), nil
}

// awaitSteadyState samples completion counts until the rate settles or
// --sweep-warmup-max runs out. Returns what completed during warm-up and
// whether it settled.
func awaitSteadyState(ctx context.Context, completed *atomic.Int64) (int64, bool) {
	det := newSteadyDetector(loadSweepTolerance, loadSweepStableRuns, loadSweepWindow)
	deadline := time.Now().Add(loadSweepWarmupMax)
	ticker := time.NewTicker(loadSweepWindow)
	defer ticker.Stop()

	last := completed.Load()
	for {
		select {
		case <-ctx.Done():
			return completed.Load(), false
		case <-ticker.C:
		}
		now := completed.Load()
		count := now - last
		last = now
		if det.observe(count) {
			return now, true
		}
		if time.Now().After(deadline) {
			return now, false
		}
	}
}

// summariseSweepLevel turns one level's samples into its row.
func summariseSweepLevel(n int, s *loadStats, elapsed, warmup time.Duration, discarded int64, steady bool) sweepLevel {
	s.mu.Lock()
	defer s.mu.Unlock()

	lvl := sweepLevel{
		Concurrency:       n,
		Successes:         s.successes,
		Errors:            s.errors,
		Tokens:            s.tokens,
		WarmupSec:         warmup.Seconds(),
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
		if !l.SteadyState {
			fmt.Fprintf(w, "level %d never settled within --sweep-warmup-max; its row was measured mid-ramp.\n", l.Concurrency)
		}
	}
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
