package main

import (
	"strings"
	"testing"
)

// run builds a Summary with the fields the A/B reads, so tests state only
// what they are about.
func run(tokPerSec float64, ttft50, ttft95, lat50 int64) Summary {
	return Summary{
		DurationSec:  60,
		TargetRPS:    5,
		ActualRPS:    5,
		Successes:    300,
		Errors:       0,
		TokensPerSec: tokPerSec,
		LatencyP50Ms: lat50,
		LatencyP95Ms: lat50 * 2,
		TTFTSamples:  300,
		TTFTP50Ms:    ttft50,
		TTFTP95Ms:    ttft95,
	}
}

// atRate builds a run that was OFFERED target rps and ACHIEVED actual rps,
// which is the distinction the saturation warning turns on.
func atRate(target, actual float64, tokPerSec float64, ttft50 int64) Summary {
	s := run(tokPerSec, ttft50, ttft50*2, ttft50)
	s.TargetRPS, s.ActualRPS = target, actual
	s.Successes = int64(actual * s.DurationSec)
	return s
}

func find(cs []Comparison, name string) Comparison {
	for _, c := range cs {
		if c.Metric.Name == name {
			return c
		}
	}
	panic("no metric " + name)
}

// The harness has to be able to see a difference that is really there. This
// is the injected-delta self-test the GPU-free mode relies on: give one arm
// visibly worse numbers across repeated non-overlapping runs and the
// comparator must call it.
func TestEstablishesARealDifference(t *testing.T) {
	a := Arm{"nvlink", []Summary{run(100, 40, 60, 200), run(102, 41, 62, 205), run(101, 40, 61, 202)}}
	b := Arm{"pcie", []Summary{run(70, 90, 130, 300), run(71, 92, 133, 305), run(69, 91, 131, 299)}}

	cs := Compare(a, b)

	tp := find(cs, "throughput")
	if !tp.Established {
		t.Errorf("throughput difference not established despite disjoint ranges: %+v", tp)
	}
	if tp.Winner != "nvlink" {
		t.Errorf("throughput winner = %q, want nvlink (higher tok/s is better)", tp.Winner)
	}
	if tp.DeltaPct > -25 {
		t.Errorf("throughput delta = %.1f%%, want a large negative (pcie slower)", tp.DeltaPct)
	}

	// Latency metrics invert: lower is better, so the winner must flip even
	// though B's number is larger.
	tt := find(cs, "ttft p50")
	if tt.Winner != "nvlink" {
		t.Errorf("ttft winner = %q, want nvlink (lower ms is better)", tt.Winner)
	}
}

// The failure this guards is the expensive one: quoting run-to-run noise as a
// fabric result. Two single runs are ALWAYS disjoint, so a naive
// range-overlap test would call every one-shot A/B established.
func TestSingleRunNeverEstablishes(t *testing.T) {
	a := Arm{"nvlink", []Summary{run(100, 40, 60, 200)}}
	b := Arm{"pcie", []Summary{run(50, 200, 300, 400)}} // huge apparent difference

	for _, c := range Compare(a, b) {
		if c.Established {
			t.Errorf("%s established from a single run per arm; two points are always disjoint", c.Metric.Name)
		}
	}
	if !hasWarning(Warnings(a, b), "single run") {
		t.Error("no warning that a single run cannot establish anything")
	}
}

// Overlapping ranges mean the arms are indistinguishable, however different
// their medians look.
func TestOverlappingRangesDoNotEstablish(t *testing.T) {
	a := Arm{"nvlink", []Summary{run(100, 40, 60, 200), run(80, 40, 60, 200), run(90, 40, 60, 200)}}
	b := Arm{"pcie", []Summary{run(95, 40, 60, 200), run(75, 40, 60, 200), run(85, 40, 60, 200)}}

	tp := find(Compare(a, b), "throughput")
	if tp.Established {
		t.Errorf("throughput established despite overlapping ranges (a %v-%v, b %v-%v)", tp.A.Min, tp.A.Max, tp.B.Min, tp.B.Max)
	}
}

// A "no difference" result is a publishable finding, so the verdict must say
// so rather than reading like the run failed.
func TestNoDifferenceReadsAsAResult(t *testing.T) {
	a := Arm{"nvlink", []Summary{run(100, 40, 60, 200), run(101, 41, 61, 201)}}
	b := Arm{"pcie", []Summary{run(100, 40, 60, 200), run(101, 41, 61, 201)}}

	out := Render(a, b, Compare(a, b), Warnings(a, b))
	if !strings.Contains(out, "no difference established") {
		t.Errorf("verdict does not state the null result plainly:\n%s", out)
	}
	if !strings.Contains(out, "real result") {
		t.Errorf("verdict does not frame the null result as a finding:\n%s", out)
	}
}

// Comparing two runs that were not the same experiment produces a number that
// looks like a fabric result and is not one.
func TestMismatchedLoadIsFlagged(t *testing.T) {
	a := Arm{"nvlink", []Summary{run(100, 40, 60, 200), run(100, 40, 60, 200)}}
	slower := run(70, 90, 130, 300)
	slower.TargetRPS = 2 // half the offered load
	b := Arm{"pcie", []Summary{slower, slower}}

	if !hasWarning(Warnings(a, b), "did not run the same experiment") {
		t.Errorf("mismatched target rps not flagged: %v", Warnings(a, b))
	}
}

// TTFT rows are silently zero when the load generator was not run with
// --stream, which would otherwise look like an impossibly fast arm.
func TestMissingTTFTSamplesFlagged(t *testing.T) {
	noStream := run(100, 0, 0, 200)
	noStream.TTFTSamples = 0
	a := Arm{"nvlink", []Summary{noStream, noStream}}
	b := Arm{"pcie", []Summary{run(70, 90, 130, 300), run(70, 90, 130, 300)}}

	if !hasWarning(Warnings(a, b), "zero TTFT samples") {
		t.Errorf("missing TTFT samples not flagged: %v", Warnings(a, b))
	}
}

// An arm that is erroring understates its own latency, because failed
// requests do not contribute slow samples.
func TestErrorsFlagged(t *testing.T) {
	bad := run(100, 40, 60, 200)
	bad.Errors = 12
	a := Arm{"nvlink", []Summary{bad, bad}}
	b := Arm{"pcie", []Summary{run(70, 90, 130, 300), run(70, 90, 130, 300)}}

	if !hasWarning(Warnings(a, b), "errors") {
		t.Errorf("errors not flagged: %v", Warnings(a, b))
	}
}

// Even when a difference is established, the arms are different physical
// hosts, so the write-up must not claim fabric caused it.
func TestEstablishedVerdictStatesTheConfound(t *testing.T) {
	a := Arm{"nvlink", []Summary{run(100, 40, 60, 200), run(101, 41, 61, 201)}}
	b := Arm{"pcie", []Summary{run(70, 90, 130, 300), run(71, 91, 131, 301)}}

	out := Render(a, b, Compare(a, b), Warnings(a, b))
	if !strings.Contains(out, "difference established") {
		t.Fatalf("expected an established verdict:\n%s", out)
	}
	if !strings.Contains(out, "confounded") {
		t.Errorf("established verdict does not state the host confound:\n%s", out)
	}
}

func TestMedianIsRobustToOneOutlier(t *testing.T) {
	// A single stalled run must not move the centre.
	s := stat([]float64{100, 101, 5})
	if s.Median != 100 {
		t.Errorf("median = %v, want 100 (outlier dragged the centre)", s.Median)
	}
	if s.Min != 5 || s.Max != 101 {
		t.Errorf("range = %v-%v, want the outlier still visible in the spread", s.Min, s.Max)
	}
}

func hasWarning(ws []string, substr string) bool {
	for _, w := range ws {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}


// Regression from the first real paid run, 2026-08-11. The PCIe arm sustained
// 1.2 of an offered 4.0 rps; its TTFT read 6589ms against the other arm's
// 1520ms, which looks like a spectacular fabric result and is mostly a queue.
//
// The comparator reported "the arms did not run the same experiment", which is
// false -- both were configured identically -- and it pointed at the wrong
// problem, so the real one had to be found by hand in the raw JSON.
func TestSaturatedArmIsReportedAsSaturationNotMisconfiguration(t *testing.T) {
	a := Arm{"nvlink", []Summary{
		atRate(4.0, 3.93, 157.0, 1520), atRate(4.0, 3.93, 148.7, 1478), atRate(4.0, 3.96, 163.7, 1545)}}
	b := Arm{"pcie", []Summary{
		atRate(4.0, 1.18, 47.2, 8041), atRate(4.0, 1.31, 42.6, 4320), atRate(4.0, 1.22, 43.0, 6589)}}

	ws := Warnings(a, b)

	if !hasWarning(ws, "SATURATED") {
		t.Errorf("saturation not reported; the latency rows would be quoted as a fabric result: %v", ws)
	}
	if !hasWarning(ws, "pcie never reached the offered load") {
		t.Errorf("saturation warning does not name the affected arm: %v", ws)
	}
	// The arms WERE configured identically. Saying otherwise sends the reader
	// looking for a setup bug that does not exist.
	if hasWarning(ws, "did not run the same experiment") {
		t.Errorf("identical configuration reported as a config mismatch: %v", ws)
	}
	// Throughput survives saturation; the warning must say so rather than
	// casting doubt on the whole table.
	if !hasWarning(ws, "Throughput remains comparable") {
		t.Errorf("warning does not preserve the valid half of the result: %v", ws)
	}
}

// A healthy arm lands a little under target. That must not read as saturation.
func TestHealthyArmsSlightlyUnderTargetAreNotSaturated(t *testing.T) {
	a := Arm{"nvlink", []Summary{atRate(4.0, 3.93, 157, 1520), atRate(4.0, 3.96, 158, 1520)}}
	b := Arm{"pcie", []Summary{atRate(4.0, 3.88, 150, 1600), atRate(4.0, 3.91, 151, 1600)}}

	if ws := Warnings(a, b); hasWarning(ws, "SATURATED") {
		t.Errorf("normal load-generator slack reported as saturation: %v", ws)
	}
}

// A genuine configuration difference must still be caught. Removing the
// completed-request check must not have removed the config guard with it.
func TestConfigMismatchStillCaught(t *testing.T) {
	a := Arm{"nvlink", []Summary{atRate(4.0, 3.9, 157, 1520), atRate(4.0, 3.9, 157, 1520)}}
	b := Arm{"pcie", []Summary{atRate(2.0, 2.0, 90, 1600), atRate(2.0, 2.0, 90, 1600)}}

	if ws := Warnings(a, b); !hasWarning(ws, "did not run the same experiment") {
		t.Errorf("halved target rps not caught as a config mismatch: %v", ws)
	}
}

// A slower arm that still keeps up completes fewer tokens but the same number
// of requests, and nothing should warn: that is just a slower engine.
func TestFewerCompletedRequestsAloneIsNotAWarning(t *testing.T) {
	a := Arm{"nvlink", []Summary{atRate(1.0, 1.0, 157, 900), atRate(1.0, 1.0, 157, 900)}}
	b := Arm{"pcie", []Summary{atRate(1.0, 0.99, 60, 1400), atRate(1.0, 0.99, 60, 1400)}}

	ws := Warnings(a, b)
	if hasWarning(ws, "did not run the same experiment") || hasWarning(ws, "SATURATED") {
		t.Errorf("an unsaturated slower arm triggered a warning: %v", ws)
	}
}
