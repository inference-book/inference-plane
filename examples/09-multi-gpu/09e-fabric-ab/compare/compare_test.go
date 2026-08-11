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
