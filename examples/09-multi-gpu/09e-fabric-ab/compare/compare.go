// Comparator for the Ch 10 fabric A/B: two arms, identical load, one verdict.
//
// This is deliberately more careful than "print both numbers and subtract".
// The A/B exists to replace the chapter's softest claim with a measured one,
// and the two ways to get that wrong are both quiet:
//
//   - Reporting a delta that is really run-to-run noise. A single run per arm
//     gives no variance estimate at all, so this refuses to call any delta
//     established from one run, no matter how large it looks.
//   - Comparing two runs that were not the same experiment. Different rps,
//     duration, or request count between arms produces a number that looks
//     like a fabric result and is not one.
//
// The interesting outcome for the chapter is a SMALL delta, because that
// would contradict the received wisdom the chapter currently repeats. A
// comparator that is eager to declare a winner would bury exactly the result
// worth publishing.
package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
)

// Summary mirrors the JSON that `iplane load --output json` emits. Field tags
// match cmd/iplane/cmd/load.go's loadSummary; only the fields the A/B reads
// are declared, so an added field upstream does not break decoding.
type Summary struct {
	DurationSec  float64 `json:"duration_sec"`
	TargetRPS    float64 `json:"target_rps"`
	ActualRPS    float64 `json:"actual_rps"`
	Successes    int64   `json:"successes"`
	Errors       int64   `json:"errors"`
	Tokens       int64   `json:"completion_tokens"`
	TokensPerSec float64 `json:"completion_tokens_per_sec"`
	LatencyP50Ms int64   `json:"latency_p50_ms"`
	LatencyP95Ms int64   `json:"latency_p95_ms"`
	TTFTSamples  int64   `json:"ttft_samples"`
	TTFTP50Ms    int64   `json:"ttft_p50_ms"`
	TTFTP95Ms    int64   `json:"ttft_p95_ms"`
}

// saturationFraction is how close an arm's achieved rate must stay to the
// offered rate before its latency figures are trusted.
//
// 0.8 rather than something tighter because a healthy arm still lands a little
// under target: the load generator's own scheduling and the tail of in-flight
// requests at the cutoff cost a few percent. Measured on a healthy arm:
// 3.93-3.96 of an offered 4.0, so 0.98. A saturated one read 0.31. The gap
// between those is wide enough that the exact threshold is not delicate.
const saturationFraction = 0.8

// Arm is one side of the A/B: a label and every run recorded for it.
type Arm struct {
	Label string
	Runs  []Summary
}

// Stat is the spread of one metric across an arm's runs. Median rather than
// mean because a single stalled run should not drag the centre, and the whole
// point of repeating runs is that outliers happen.
type Stat struct {
	Median float64
	Min    float64
	Max    float64
	N      int
}

func stat(vals []float64) Stat {
	if len(vals) == 0 {
		return Stat{}
	}
	s := append([]float64(nil), vals...)
	sort.Float64s(s)
	med := s[len(s)/2]
	if len(s)%2 == 0 {
		med = (s[len(s)/2-1] + s[len(s)/2]) / 2
	}
	return Stat{Median: med, Min: s[0], Max: s[len(s)-1], N: len(s)}
}

// Metric is one compared quantity.
type Metric struct {
	Name string
	Unit string
	// HigherIsBetter distinguishes throughput from latency so the verdict can
	// say which arm won rather than only which number is larger.
	HigherIsBetter bool
	Extract        func(Summary) float64
}

// Metrics is the compared set. Throughput is the chapter's headline, but TTFT
// is included because a fabric difference can show up in time-to-first-token
// (prefill is where tensor-parallel all-reduce traffic is heaviest) while
// steady-state throughput barely moves. Reporting only throughput could miss
// the effect entirely.
var Metrics = []Metric{
	{"throughput", "tok/s", true, func(s Summary) float64 { return s.TokensPerSec }},
	{"ttft p50", "ms", false, func(s Summary) float64 { return float64(s.TTFTP50Ms) }},
	{"ttft p95", "ms", false, func(s Summary) float64 { return float64(s.TTFTP95Ms) }},
	{"latency p50", "ms", false, func(s Summary) float64 { return float64(s.LatencyP50Ms) }},
	{"latency p95", "ms", false, func(s Summary) float64 { return float64(s.LatencyP95Ms) }},
}

// Comparison is one metric's result across both arms.
type Comparison struct {
	Metric Metric
	A, B   Stat
	// DeltaPct is B relative to A, signed, in percent.
	DeltaPct float64
	// Established is true only when the arms' observed ranges are disjoint AND
	// both arms have more than one run. Disjoint ranges from a single run each
	// mean nothing: two numbers are always disjoint.
	Established bool
	// Winner is the label that did better on this metric, or "" when the
	// difference is not established.
	Winner string
}

// Compare runs the metric set over both arms.
func Compare(a, b Arm) []Comparison {
	out := make([]Comparison, 0, len(Metrics))
	for _, m := range Metrics {
		av := make([]float64, 0, len(a.Runs))
		for _, r := range a.Runs {
			av = append(av, m.Extract(r))
		}
		bv := make([]float64, 0, len(b.Runs))
		for _, r := range b.Runs {
			bv = append(bv, m.Extract(r))
		}
		as, bs := stat(av), stat(bv)

		c := Comparison{Metric: m, A: as, B: bs}
		if as.Median != 0 {
			c.DeltaPct = (bs.Median - as.Median) / math.Abs(as.Median) * 100
		}
		// Disjoint ranges, and enough runs for "disjoint" to carry information.
		disjoint := as.Max < bs.Min || bs.Max < as.Min
		c.Established = disjoint && as.N > 1 && bs.N > 1
		if c.Established {
			bBetter := bs.Median > as.Median
			if !m.HigherIsBetter {
				bBetter = bs.Median < as.Median
			}
			if bBetter {
				c.Winner = b.Label
			} else {
				c.Winner = a.Label
			}
		}
		out = append(out, c)
	}
	return out
}

// Warnings returns everything that should make a reader distrust the run.
// These are not stylistic nits. Each one has produced a wrong A/B somewhere.
func Warnings(a, b Arm) []string {
	var w []string

	if len(a.Runs) == 0 || len(b.Runs) == 0 {
		w = append(w, "an arm has no runs; nothing can be concluded")
		return w
	}
	if len(a.Runs) < 2 || len(b.Runs) < 2 {
		w = append(w, fmt.Sprintf(
			"single run on at least one arm (%s n=%d, %s n=%d): no variance estimate, so no delta can be established. Re-run with --repeat 3 or more.",
			a.Label, len(a.Runs), b.Label, len(b.Runs)))
	}

	// The two arms must have been the same experiment. A mismatch here makes
	// every number below meaningless, so it is checked before anything else
	// is believed.
	for _, p := range []struct {
		name   string
		get    func(Summary) float64
		tolPct float64
	}{
		// Configuration only. Completed-request counts deliberately do NOT
		// belong here: they are an OUTCOME, and a slower arm finishing fewer
		// requests under identical settings is the result, not a setup error.
		// Conflating the two produced a misdiagnosis on the first real run --
		// it reported "the arms did not run the same experiment" when the
		// arms were configured identically and one simply could not keep up.
		{"target rps", func(s Summary) float64 { return s.TargetRPS }, 0},
		{"duration", func(s Summary) float64 { return s.DurationSec }, 5},
	} {
		av := stat(mapf(a.Runs, p.get)).Median
		bv := stat(mapf(b.Runs, p.get)).Median
		if av == 0 && bv == 0 {
			continue
		}
		den := math.Abs(av)
		if den == 0 {
			den = math.Abs(bv)
		}
		diff := math.Abs(av-bv) / den * 100
		if diff > p.tolPct {
			w = append(w, fmt.Sprintf(
				"%s differs between arms (%s=%.4g, %s=%.4g, %.1f%% apart): the arms did not run the same experiment",
				p.name, a.Label, av, b.Label, bv, diff))
		}
	}

	// Saturation, which is a different problem from misconfiguration and needs
	// saying differently.
	//
	// An arm that never reached the offered load was queueing, and queueing
	// delay is not a fabric cost -- it is unbounded in offered load and says
	// more about the headroom than the hardware. Measured on the first real
	// run: the PCIe arm sustained 1.2 of an offered 4.0 rps and its TTFT p50
	// read 6589ms against the other arm's 1520ms, which looks like a
	// spectacular fabric result and is mostly a queue.
	//
	// The throughput comparison survives this and the latency comparison does
	// not, so the warning says which, rather than casting doubt on the whole
	// table.
	for _, arm := range []Arm{a, b} {
		tgt := stat(mapf(arm.Runs, func(s Summary) float64 { return s.TargetRPS })).Median
		act := stat(mapf(arm.Runs, func(s Summary) float64 { return s.ActualRPS })).Median
		if tgt > 0 && act < saturationFraction*tgt {
			w = append(w, fmt.Sprintf(
				"%s never reached the offered load (%.2f of %.2f rps): it was SATURATED, so its "+
					"latency and ttft rows include queueing delay and are not a fabric measurement. "+
					"Throughput remains comparable -- both arms were offered the same load. "+
					"For clean latency, re-run below %.1f rps.",
				arm.Label, act, tgt, act))
		}
	}

	for _, arm := range []Arm{a, b} {
		var errs, ttft int64
		for _, r := range arm.Runs {
			errs += r.Errors
			ttft += r.TTFTSamples
		}
		if errs > 0 {
			w = append(w, fmt.Sprintf("%s recorded %d errors; a failing arm understates its own latency", arm.Label, errs))
		}
		if ttft == 0 {
			w = append(w, fmt.Sprintf("%s has zero TTFT samples: the load generator was not run with --stream, so the TTFT rows below are meaningless", arm.Label))
		}
	}
	return w
}

func mapf(runs []Summary, f func(Summary) float64) []float64 {
	out := make([]float64, 0, len(runs))
	for _, r := range runs {
		out = append(out, f(r))
	}
	return out
}

// Render formats the comparison as the block the walkthrough prints.
func Render(a, b Arm, cs []Comparison, warns []string) string {
	var sb strings.Builder
	line := strings.Repeat("=", 74)
	fmt.Fprintf(&sb, "%s\nfabric A/B: %s (n=%d) vs %s (n=%d)\n%s\n",
		line, a.Label, len(a.Runs), b.Label, len(b.Runs), line)

	fmt.Fprintf(&sb, "%-20s %-24s %-24s %10s  %s\n", "metric", a.Label, b.Label, "delta", "established")
	for _, c := range cs {
		fmt.Fprintf(&sb, "%-20s %-24s %-24s %+9.1f%%  %s\n",
			c.Metric.Name+" ("+c.Metric.Unit+")",
			fmtStat(c.A), fmtStat(c.B), c.DeltaPct, estab(c))
	}

	fmt.Fprintf(&sb, "\n%s\n", strings.Repeat("-", 74))
	if len(warns) > 0 {
		fmt.Fprintf(&sb, "WARNINGS -- read these before quoting any number above:\n")
		for _, x := range warns {
			fmt.Fprintf(&sb, "  ! %s\n", x)
		}
		fmt.Fprintf(&sb, "\n")
	}
	fmt.Fprint(&sb, verdict(cs))
	return sb.String()
}

func fmtStat(s Stat) string {
	if s.N == 0 {
		return "-"
	}
	if s.N == 1 {
		return fmt.Sprintf("%.4g", s.Median)
	}
	return fmt.Sprintf("%.4g [%.4g-%.4g]", s.Median, s.Min, s.Max)
}

func estab(c Comparison) string {
	if c.Established {
		return "yes, " + c.Winner + " better"
	}
	return "no"
}

// verdict states the conclusion in the terms the chapter needs, including the
// case the chapter most wants to hear about.
func verdict(cs []Comparison) string {
	var established []Comparison
	for _, c := range cs {
		if c.Established {
			established = append(established, c)
		}
	}
	if len(established) == 0 {
		return "VERDICT: no difference established on any metric.\n" +
			"  This is a real result, not a failed run. If the arms genuinely differ in\n" +
			"  fabric, it says the workload is not fabric-bound at this size and shape,\n" +
			"  which contradicts the usual claim and is worth reporting as such.\n" +
			"  Before concluding that, confirm the arms really did differ in fabric\n" +
			"  (check bw_nvlink on both) and that the run was long enough to matter.\n"
	}
	var sb strings.Builder
	sb.WriteString("VERDICT: difference established on ")
	names := make([]string, 0, len(established))
	for _, c := range established {
		names = append(names, fmt.Sprintf("%s (%+.1f%%, %s better)", c.Metric.Name, c.DeltaPct, c.Winner))
	}
	sb.WriteString(strings.Join(names, ", "))
	sb.WriteString(".\n  Established means the arms' observed ranges did not overlap across\n" +
		"  repeated runs. It does not by itself attribute the cause to fabric:\n" +
		"  the arms are different physical hosts, so host differences are\n" +
		"  confounded with interconnect. State that limitation alongside the number.\n")
	return sb.String()
}

// LoadArm reads an arm's run files.
func LoadArm(label string, paths []string) (Arm, error) {
	arm := Arm{Label: label}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return arm, fmt.Errorf("read %s: %w", p, err)
		}
		var s Summary
		if err := json.Unmarshal(b, &s); err != nil {
			return arm, fmt.Errorf("parse %s: %w", p, err)
		}
		arm.Runs = append(arm.Runs, s)
	}
	return arm, nil
}
