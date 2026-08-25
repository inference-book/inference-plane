package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

func resetSweepFlags() {
	loadSweepLevels = ""
	loadSweepDuration = 30 * time.Second
	loadSweepWarmupMax = 90 * time.Second
	loadSweepWindow = 3 * time.Second
	loadSweepTolerance = 0.1
	loadSweepStableRuns = 3
	loadURL = ""
	loadServiceURL = ""
	loadTarget = ""
	loadModel = "mock/mock"
	loadMaxTokens = 8
	loadChatFraction = 1.0
	loadStream = false
	loadPriority = ""
	loadTenant = ""
	loadOutput = "text"
	sweepProgress = os.Stderr
}

func TestParseSweepLevelsSortsAndDeduplicates(t *testing.T) {
	got, err := parseSweepLevels(" 8, 2,4 , 2 ,1")
	if err != nil {
		t.Fatal(err)
	}
	want := []int{1, 2, 4, 8}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParseSweepLevelsRefusesWhatIsNotALadder(t *testing.T) {
	for _, in := range []string{"", "  ", "x", "0", "-4", "1,zero"} {
		if _, err := parseSweepLevels(in); err == nil {
			t.Errorf("accepted %q as a concurrency ladder", in)
		}
	}
}

// A level that is still climbing is not settled, however close any two
// consecutive windows happen to be. This is the case a
// previous-window comparison gets wrong and the running-mean comparison
// gets right, so it is the reason the detector is shaped the way it is.
func TestSteadyDetectorRejectsASlowClimb(t *testing.T) {
	d := newSteadyDetector(0.1, 3, time.Second)
	for _, count := range []int64{100, 115, 132, 152, 175} {
		if d.observe(count) {
			t.Fatalf("declared steady during a climb, at %d", count)
		}
	}
}

func TestSteadyDetectorSettlesOnAFlatRate(t *testing.T) {
	d := newSteadyDetector(0.1, 3, time.Second)
	if d.observe(100) || d.observe(103) {
		t.Fatal("settled before it had enough windows to compare")
	}
	if !d.observe(98) {
		t.Error("did not settle on three windows within tolerance")
	}
}

// Nothing completing is not stability. An engine that has stopped
// answering produces three identical zeroes, and reporting that level as
// measured would put a zero-throughput row in the curve looking like
// every other row.
func TestSteadyDetectorDoesNotSettleOnNothingHappening(t *testing.T) {
	d := newSteadyDetector(0.1, 3, time.Second)
	for range 5 {
		if d.observe(0) {
			t.Fatal("declared a level that completed nothing to be steady")
		}
	}
}

// At low counts the tolerance is dominated by the fact that requests
// arrive whole. Five per second against a one-second window alternates
// between four and five forever, which is a twenty percent swing that no
// amount of waiting removes.
func TestSteadyDetectorDoesNotMistakeCountingForInstability(t *testing.T) {
	d := newSteadyDetector(0.1, 3, time.Second)
	d.observe(5)
	d.observe(4)
	if !d.observe(5) {
		t.Error("a level alternating between four and five completions never settles")
	}

	// The floor must not swallow a real change at counts fine enough to
	// resolve one.
	big := newSteadyDetector(0.1, 3, time.Second)
	big.observe(500)
	big.observe(400)
	if big.observe(600) {
		t.Error("settled on a swing far outside tolerance at a resolvable count")
	}
}

func TestSteadyDetectorNeedsTheConfiguredNumberOfWindows(t *testing.T) {
	d := newSteadyDetector(0.1, 5, time.Second)
	for i := range 4 {
		if d.observe(100) {
			t.Fatalf("settled after %d windows, want 5", i+1)
		}
	}
	if !d.observe(100) {
		t.Error("did not settle on the fifth window")
	}
}

// A nil stats pointer is how the sweep fires warm-up traffic without
// letting it into the measurement, so every recorder has to tolerate one.
func TestNilStatsDiscardsEveryRecording(t *testing.T) {
	var s *loadStats
	s.recordSuccess(time.Second, 10)
	s.recordTTFT(time.Second)
	s.recordITLs([]time.Duration{time.Second})
	s.recordError()
}

func TestQuantilePinsTheExistingSemantics(t *testing.T) {
	in := []time.Duration{90, 10, 50, 30, 70}
	if got := quantile(in, 0.50); got != 50 {
		t.Errorf("p50 = %v, want 50", got)
	}
	if got := quantile(in, 0.99); got != 90 {
		t.Errorf("p99 = %v, want 90", got)
	}
	if got := quantile(nil, 0.5); got != 0 {
		t.Errorf("empty = %v, want 0", got)
	}
}

// Inter-token latency is the gap between content frames, so a stream
// paced at a known interval has to report roughly that interval and not
// the time to the first token, which is a different and much larger
// number here.
func TestInterTokenLatencyMeasuresTheGapBetweenFrames(t *testing.T) {
	const gap = 25 * time.Millisecond
	srv := sseServer(t, []string{
		`{"choices":[{"delta":{"role":"assistant"}}]}`,
		`{"choices":[{"delta":{"content":"one "}}]}`,
		`{"choices":[{"delta":{"content":"two "}}]}`,
		`{"choices":[{"delta":{"content":"three"}}]}`,
	}, gap)

	res := parseChatResponse(getStream(t, srv.URL), true, time.Now())
	if len(res.ITLs) != 2 {
		t.Fatalf("got %d gaps from four frames (one role-only, three content), want 2", len(res.ITLs))
	}
	for _, d := range res.ITLs {
		if d < gap/2 || d > gap*4 {
			t.Errorf("gap %v is nowhere near the %v the server paced at", d, gap)
		}
	}
	// That the role-only opening frame does not stop the TTFT clock is
	// pinned by TestTTFTIgnoresTheRoleOnlyOpeningFrame; what matters here
	// is only that the gaps after it are counted separately from it.
	if !res.HasTTFT {
		t.Error("no time-to-first-token alongside the gaps")
	}
}

func TestNonStreamedResponsesReportNoInterTokenGaps(t *testing.T) {
	srv := jsonServer(t, `{"choices":[{"message":{"content":"hi"}}],"usage":{"completion_tokens":2}}`)
	res := parseChatResponse(getStream(t, srv.URL), false, time.Now())
	if len(res.ITLs) != 0 {
		t.Errorf("got %d gaps from a non-streamed reply, want none", len(res.ITLs))
	}
}

// End to end against a server that answers after a known delay. The
// arithmetic the curve rests on is that a closed loop at N concurrent
// requests, each taking d, produces N/d per second.
func TestSweepHoldsTheConcurrencyItIsAskedFor(t *testing.T) {
	resetSweepFlags()
	t.Cleanup(resetSweepFlags)

	const serve = 20 * time.Millisecond
	var inFlight, peak atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := inFlight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		defer inFlight.Add(-1)
		time.Sleep(serve)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"completion_tokens":4}}`)
	}))
	t.Cleanup(srv.Close)

	loadURL = srv.URL
	loadSweepLevels = "1,4"
	loadSweepDuration = 400 * time.Millisecond
	loadSweepWindow = 100 * time.Millisecond
	loadSweepWarmupMax = 2 * time.Second
	loadOutput = "json"

	var buf bytes.Buffer
	if err := runLoadSweep(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	var got sweepReport
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("parse: %v\n%s", err, buf.String())
	}

	if len(got.Levels) != 2 {
		t.Fatalf("levels = %d, want 2", len(got.Levels))
	}
	if p := peak.Load(); p < 4 {
		t.Errorf("the server never saw more than %d concurrent requests; the loop is not closed", p)
	}
	for _, l := range got.Levels {
		if l.Errors != 0 {
			t.Errorf("level %d had %d errors", l.Concurrency, l.Errors)
		}
		if l.Successes == 0 {
			t.Fatalf("level %d recorded nothing", l.Concurrency)
		}
		// Little's law against the offered level. Generous bounds: the
		// point is that the row is describing the batch on its label, not
		// that the timing is precise under a loaded test runner.
		if b := l.AchievedBatch; b < float64(l.Concurrency)*0.5 || b > float64(l.Concurrency)*1.5 {
			t.Errorf("level %d achieved a batch of %.1f", l.Concurrency, b)
		}
	}
	if a, b := got.Levels[0].TokensPerSec, got.Levels[1].TokensPerSec; b <= a {
		t.Errorf("four concurrent produced %.0f tok/s against one's %.0f; the curve is not rising", b, a)
	}
}

// Warm-up traffic is fired and then excluded, and both halves of that are
// reported so a reader knows what the row is made of.
func TestSweepReportsWhatItDiscarded(t *testing.T) {
	resetSweepFlags()
	t.Cleanup(resetSweepFlags)

	var served atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served.Add(1)
		time.Sleep(10 * time.Millisecond)
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"completion_tokens":1}}`)
	}))
	t.Cleanup(srv.Close)

	loadURL = srv.URL
	loadSweepLevels = "2"
	loadSweepDuration = 300 * time.Millisecond
	loadSweepWindow = 100 * time.Millisecond
	loadSweepWarmupMax = 2 * time.Second
	loadOutput = "json"

	var buf bytes.Buffer
	if err := runLoadSweep(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	var got sweepReport
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	lvl := got.Levels[0]
	if lvl.DiscardedRequests == 0 {
		t.Error("reported no discarded requests, so the warm-up either did not fire or was not excluded")
	}
	if lvl.WarmupSec <= 0 {
		t.Error("reported no warm-up duration")
	}
	if total := served.Load(); total <= lvl.Successes {
		t.Errorf("server saw %d requests and the level counted %d successes; the discarded ones were counted too",
			total, lvl.Successes)
	}
}

// A level that will not settle inside --sweep-warmup-max is reported
// rather than omitted, and flagged so its row is not read as the same
// kind of number as the rest. A gap in the ladder reads as an oversight,
// which is the same reasoning the budget sweep's skipped rows follow.
//
// Driven by starving the detector of windows rather than by an unstable
// server: the deadline lands before three windows have elapsed, so it
// cannot have settled regardless of what the rate did. An unstable rate
// would be the more realistic trigger and a far flakier test.
func TestSweepReportsALevelThatNeverSettles(t *testing.T) {
	resetSweepFlags()
	t.Cleanup(resetSweepFlags)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"completion_tokens":1}}`)
	}))
	t.Cleanup(srv.Close)

	loadURL = srv.URL
	loadSweepLevels = "1"
	loadSweepDuration = 100 * time.Millisecond
	loadSweepWindow = 100 * time.Millisecond
	loadSweepStableRuns = 3
	loadSweepWarmupMax = 150 * time.Millisecond
	loadOutput = "json"

	var buf bytes.Buffer
	if err := runLoadSweep(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	var got sweepReport
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Levels) != 1 {
		t.Fatalf("levels = %d, want the unsettled one reported", len(got.Levels))
	}
	if got.Levels[0].SteadyState {
		t.Error("a level that ran out of warm-up before it had three windows was reported as steady")
	}
	if got.Levels[0].Successes == 0 {
		t.Error("the unsettled level was flagged and then not measured")
	}
}

func TestSweepRefusesConfigurationItCannotHonour(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func()
	}{
		{"no measurement window", func() { loadSweepDuration = 0 }},
		{"no sampling window", func() { loadSweepWindow = 0 }},
		{"tolerance of one", func() { loadSweepTolerance = 1 }},
		{"negative tolerance", func() { loadSweepTolerance = -0.1 }},
		{"one stable window", func() { loadSweepStableRuns = 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetSweepFlags()
			t.Cleanup(resetSweepFlags)
			loadSweepLevels = "1"
			loadURL = "http://127.0.0.1:1"
			tc.mutate()
			if err := runLoadSweep(context.Background(), &bytes.Buffer{}); err == nil {
				t.Error("accepted it")
			}
		})
	}
}
