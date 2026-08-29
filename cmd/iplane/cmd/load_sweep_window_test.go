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

// The projected window is narration only, but it is the number an operator
// decides on mid-run, so its clamps are the same as the real window's.
func TestProjectedMeasureWindow(t *testing.T) {
	resetSweepFlags()
	t.Cleanup(resetSweepFlags)

	loadSweepDuration = 30 * time.Second
	loadSweepDurationMax = 5 * time.Minute

	for name, tc := range map[string]struct {
		minSamples int
		rate       float64
		want       time.Duration
	}{
		// Off by default: the window is whatever was asked for.
		"disabled": {minSamples: 0, rate: 100, want: 30 * time.Second},
		// No reading is not a slow rate. A level that completed nothing
		// earns nothing by being held longer.
		"no rate":  {minSamples: 200, rate: 0, want: 30 * time.Second},
		"negative": {minSamples: 200, rate: -1, want: 30 * time.Second},
		// Fast enough already: --sweep-duration stays the floor, so turning
		// this on can never shorten a level below what was asked for.
		"already earns it": {minSamples: 200, rate: 100, want: 30 * time.Second},
		// The case it exists for: half a request a second needs over three
		// minutes to earn 100 samples, where the flat window collects 15.
		"extends": {minSamples: 100, rate: 0.5, want: 200 * time.Second},
		// The ceiling wins over the sample target. A level that cannot earn
		// its samples comes back starved rather than open-ended.
		"capped": {minSamples: 200, rate: 0.05, want: 5 * time.Minute},
	} {
		t.Run(name, func(t *testing.T) {
			loadSweepMinSamples = tc.minSamples
			if got := projectedMeasureWindow(tc.rate); got != tc.want {
				t.Errorf("projectedMeasureWindow(%g) = %s, want %s", tc.rate, got, tc.want)
			}
		})
	}
}

// An unbounded window on rented hardware is a bill nobody agreed to, so the
// ceiling is required rather than defaulted to some multiple.
func TestSweepMinSamplesRequiresACeiling(t *testing.T) {
	resetSweepFlags()
	t.Cleanup(resetSweepFlags)

	loadSweepLevels = "1"
	loadSweepMinSamples = 200

	err := runLoadSweep(context.Background(), &bytes.Buffer{})
	if err == nil {
		t.Fatal("a sample target with no ceiling should be refused")
	}
	if !strings.Contains(err.Error(), "--sweep-duration-max") {
		t.Errorf("the error should name the missing flag, got: %v", err)
	}
}

func TestSweepCeilingMustExceedTheFloor(t *testing.T) {
	resetSweepFlags()
	t.Cleanup(resetSweepFlags)

	loadSweepLevels = "1"
	loadSweepDuration = 60 * time.Second
	loadSweepMinSamples = 200
	loadSweepDurationMax = 10 * time.Second

	err := runLoadSweep(context.Background(), &bytes.Buffer{})
	if err == nil {
		t.Fatal("a ceiling below the floor should be refused")
	}
}

// End to end. A slow server earns few samples in a flat window; with a sample
// target the level holds open until it has them, and the row says how long it
// actually ran.
func TestSweepExtendsASlowLevelToEarnItsSamples(t *testing.T) {
	const serve = 50 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(serve)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"completion_tokens":4}}`)
	}))
	t.Cleanup(srv.Close)

	run := func(minSamples int) sweepLevel {
		t.Helper()
		resetSweepFlags()
		t.Cleanup(resetSweepFlags)

		loadURL = srv.URL
		loadSweepLevels = "1"
		loadSweepDuration = 200 * time.Millisecond
		loadSweepWindow = 100 * time.Millisecond
		loadSweepWarmupMax = 3 * time.Second
		loadSweepMinSamples = minSamples
		loadSweepDurationMax = 5 * time.Second
		loadOutput = "json"

		var buf bytes.Buffer
		if err := runLoadSweep(context.Background(), &buf); err != nil {
			t.Fatal(err)
		}
		var got sweepReport
		if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
			t.Fatalf("parse: %v\n%s", err, buf.String())
		}
		if len(got.Levels) != 1 {
			t.Fatalf("levels = %d, want 1", len(got.Levels))
		}
		return got.Levels[0]
	}

	flat := run(0)
	if flat.MeasureSec > 0.6 {
		t.Errorf("the flat window ran %.2fs against the 0.2s asked for", flat.MeasureSec)
	}

	// At one in flight and 50ms a request, twenty samples need about a
	// second, which is five times the flat window.
	extended := run(20)
	if extended.MeasureSec <= flat.MeasureSec*1.5 {
		t.Errorf("the extended window ran %.2fs against the flat %.2fs; it did not extend",
			extended.MeasureSec, flat.MeasureSec)
	}
	// The target is counted rather than projected, so it is actually met
	// whenever the ceiling allows rather than approached.
	if extended.Successes < 20 {
		t.Errorf("the extended level earned %d samples against the 20 asked for, with the cap not reached",
			extended.Successes)
	}
	if extended.Successes <= flat.Successes {
		t.Errorf("extending earned %d samples against the flat window's %d",
			extended.Successes, flat.Successes)
	}
}

// The ceiling wins. A level that cannot earn its samples inside
// --sweep-duration-max comes back short rather than running on, because the
// alternative is an open-ended hold on rented hardware.
func TestSweepCeilingStopsALevelThatCannotEarnItsSamples(t *testing.T) {
	resetSweepFlags()
	t.Cleanup(resetSweepFlags)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"completion_tokens":4}}`)
	}))
	t.Cleanup(srv.Close)

	loadURL = srv.URL
	loadSweepLevels = "1"
	loadSweepDuration = 200 * time.Millisecond
	loadSweepWindow = 100 * time.Millisecond
	loadSweepWarmupMax = 3 * time.Second
	loadSweepMinSamples = 100000
	loadSweepDurationMax = time.Second
	loadOutput = "json"

	var buf bytes.Buffer
	if err := runLoadSweep(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	var got sweepReport
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("parse: %v\n%s", err, buf.String())
	}
	l := got.Levels[0]
	if l.MeasureSec > 2 {
		t.Errorf("the level ran %.2fs past a 1s ceiling", l.MeasureSec)
	}
	if l.Successes >= 100000 {
		t.Fatal("the server was too fast for this test to mean anything")
	}
}
