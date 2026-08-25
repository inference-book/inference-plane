package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// shortSweepServer answers fast enough that a level settles inside a test.
func shortSweepServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(5 * time.Millisecond)
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"completion_tokens":3}}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// configureShortSweep points the sweep at srv and shrinks every duration so
// a full ladder runs in about a second.
func configureShortSweep(t *testing.T, srv *httptest.Server, levels string) *bytes.Buffer {
	t.Helper()
	resetSweepFlags()
	t.Cleanup(resetSweepFlags)

	loadURL = srv.URL
	loadSweepLevels = levels
	loadSweepDuration = 200 * time.Millisecond
	loadSweepWindow = 50 * time.Millisecond
	loadSweepWarmupMax = 2 * time.Second
	loadOutput = "json"

	var progress bytes.Buffer
	sweepProgress = &progress
	return &progress
}

// A sweep that has finished a level should say so. Before this, the header
// printed and then nothing until every level was done: on the GLM-5.2 run
// that was 48 identical minutes while an 8x H200 billed at $32.88/hr, and a
// sweep firing at a closed port looked exactly the same as a healthy one.
func TestSweepPrintsALineWhenALevelCompletes(t *testing.T) {
	progress := configureShortSweep(t, shortSweepServer(t), "1,2")

	var artifact bytes.Buffer
	if err := runLoadSweep(context.Background(), &artifact); err != nil {
		t.Fatal(err)
	}

	got := progress.String()
	for _, want := range []string{"level 1/2", "level 2/2", "req/s", "tok/s"} {
		if !strings.Contains(got, want) {
			t.Errorf("progress output does not mention %q:\n%s", want, got)
		}
	}
}

// A level can legitimately run for --sweep-warmup-max plus --sweep-duration,
// which is up to eighteen minutes on the real settings. Silence for that long
// is indistinguishable from a level that is doing nothing at all, and telling
// those apart is the distinction that actually cost money.
func TestSweepReportsProgressWithinALevel(t *testing.T) {
	progress := configureShortSweep(t, shortSweepServer(t), "1")
	// Long enough that several windows elapse inside the one level.
	loadSweepDuration = 400 * time.Millisecond
	loadSweepWindow = 40 * time.Millisecond

	var artifact bytes.Buffer
	if err := runLoadSweep(context.Background(), &artifact); err != nil {
		t.Fatal(err)
	}

	got := progress.String()
	summaryAt := strings.Index(got, "level 1/1")
	if summaryAt < 0 {
		t.Fatalf("no level summary at all:\n%s", got)
	}
	before := got[:summaryAt]
	if strings.TrimSpace(before) == "" {
		t.Fatalf("nothing was reported before the level finished; a running level is still silent:\n%s", got)
	}
	if !strings.Contains(before, "n=1") {
		t.Errorf("in-level progress does not say which level it is about:\n%s", before)
	}
}

// The artifact is what a Part IV figure is drawn from, and a redirect is how
// it gets captured. One stray progress line on stdout corrupts a file that
// costs thousands of dollars of rented hardware to reproduce.
func TestSweepProgressNeverReachesTheArtifact(t *testing.T) {
	progress := configureShortSweep(t, shortSweepServer(t), "1,2")

	var artifact bytes.Buffer
	if err := runLoadSweep(context.Background(), &artifact); err != nil {
		t.Fatal(err)
	}

	var report sweepReport
	if err := json.Unmarshal(artifact.Bytes(), &report); err != nil {
		t.Fatalf("stdout is not the artifact any more: %v\n%s", err, artifact.String())
	}
	if len(report.Levels) != 2 {
		t.Errorf("artifact carries %d levels, want 2", len(report.Levels))
	}
	if strings.Contains(artifact.String(), "level 1/2") {
		t.Error("a progress line reached stdout")
	}
	if progress.Len() == 0 {
		t.Error("nothing was written to the progress stream, so this test proves nothing")
	}
}

// csv is the other artifact format and has the same contract.
func TestSweepProgressNeverReachesTheCSVArtifact(t *testing.T) {
	progress := configureShortSweep(t, shortSweepServer(t), "1")
	loadOutput = "csv"

	var artifact bytes.Buffer
	if err := runLoadSweep(context.Background(), &artifact); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(artifact.String(), "level 1/1") {
		t.Errorf("a progress line reached the csv:\n%s", artifact.String())
	}
	if progress.Len() == 0 {
		t.Error("nothing was written to the progress stream")
	}
}

// A level that never settles is the case the operator most needs told about,
// since it is the one whose row is measured mid-ramp.
func TestSweepSaysWhenALevelNeverSettled(t *testing.T) {
	progress := configureShortSweep(t, shortSweepServer(t), "1")
	// Starve the detector: the deadline lands before enough windows pass.
	loadSweepWarmupMax = 60 * time.Millisecond
	loadSweepWindow = 50 * time.Millisecond
	loadSweepStableRuns = 5

	var artifact bytes.Buffer
	if err := runLoadSweep(context.Background(), &artifact); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(progress.String(), "never settled") {
		t.Errorf("a level measured mid-ramp was not flagged in the progress output:\n%s", progress.String())
	}
}

// The incident, in a test. A sweep fired at a closed port produces no traffic
// whatsoever and looked exactly like a level still warming up for thirteen
// minutes before anyone checked the daemon log.
//
// The completion counter cannot tell the difference and neither can the
// steady-state detector: a refused connection returns instantly, so attempts
// spin at tens of thousands per window and the rate is beautifully stable.
// Narrating that count alone would report "23287 requests in the last 2s",
// which reads healthier than a working engine. Successes and errors are the
// pair that separates them.
func TestSweepProgressDistinguishesAClosedPortFromASlowLevel(t *testing.T) {
	// A listener that is bound and then closed, so connections are refused
	// rather than timing out: the same shape as pointing at the wrong port.
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := closed.URL
	closed.Close()

	progress := configureShortSweep(t, shortSweepServer(t), "1")
	loadURL = url
	loadSweepDuration = 100 * time.Millisecond
	loadSweepWindow = 40 * time.Millisecond
	loadSweepWarmupMax = 300 * time.Millisecond

	var artifact bytes.Buffer
	if err := runLoadSweep(context.Background(), &artifact); err != nil {
		t.Fatal(err)
	}

	// Assert on the warm-up lines specifically. The measuring phase reports
	// errors too, so checking the whole stream would pass even if warm-up
	// were still narrating the raw completion count, and warm-up is the
	// phase that ran for thirteen minutes.
	var warmup []string
	for _, line := range strings.Split(progress.String(), "\n") {
		if strings.Contains(line, "warming up") {
			warmup = append(warmup, line)
		}
	}
	if len(warmup) == 0 {
		t.Fatalf("no warm-up progress at all:\n%s", progress.String())
	}
	for _, line := range warmup {
		if !strings.Contains(line, "0 ok") {
			t.Errorf("warm-up line does not say that nothing succeeded: %q", line)
		}
		if !strings.Contains(line, "errors") {
			t.Errorf("warm-up line does not mention errors, so a closed port reads as a healthy level: %q", line)
		}
	}
}

// And the contrast, so the previous test is not passing because every level
// looks broken. A working engine reports successes and never mentions errors.
func TestSweepProgressSaysNothingAboutErrorsWhenThereAreNone(t *testing.T) {
	progress := configureShortSweep(t, shortSweepServer(t), "1")

	var artifact bytes.Buffer
	if err := runLoadSweep(context.Background(), &artifact); err != nil {
		t.Fatal(err)
	}

	got := progress.String()
	if strings.Contains(got, "errors") {
		t.Errorf("a healthy level reported errors:\n%s", got)
	}
	if !strings.Contains(got, " ok") {
		t.Errorf("a healthy level reported no successes:\n%s", got)
	}
}
