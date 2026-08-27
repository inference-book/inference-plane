// Package series loads and validates the Part IV measurement registry.
//
// docs/data/series.yaml is where every figure chapters 14 and 15 cite
// resolves, and each entry carries a status saying what kind of number it
// is. The status is the point of the file, and it is only worth anything if
// it is checkable: this package re-derives every figure marked `measured`
// from the artifact its entry names, so the word means an artifact agrees
// rather than that somebody typed it.
//
// The failure this guards against is specific. A chapter drafted against
// placeholder numbers reads exactly like a chapter drafted against measured
// ones, and the difference is invisible at proofreading time. Flipping a
// placeholder to `measured` therefore has to require an artifact whose
// numbers match, or the flip is just a word change.
package series

import (
	"encoding/csv"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Status values. Only Measured may be re-derived; the others exist so a
// figure without an artifact can still be named, cited and later promoted.
const (
	// StatusMeasured is backed by a committed artifact.
	StatusMeasured = "measured"
	// StatusPredicted comes from a stated model. Publishable when the prose
	// labels it a prediction, since that is a falsifiable claim rather than
	// a reported observation.
	StatusPredicted = "predicted"
	// StatusSimulated was invented so prose could be drafted. Never prints.
	StatusSimulated = "simulated"
	// StatusPending is planned and has no numbers yet.
	StatusPending = "pending"
)

// tolerance is how far a re-derived figure may sit from the recorded one.
//
// Half a percent, which is loose enough for the two-decimal rounding the
// file stores and tight enough that a changed artifact or a changed rate
// moves a figure past it. A tolerance wide enough to absorb a real edit
// would make the check decorative.
const tolerance = 0.005

type Hardware struct {
	Provider string `yaml:"provider"`
	SKU      string `yaml:"sku"`
	GPUs     int    `yaml:"gpus"`
	Plan     string `yaml:"plan"`
}

type Workload struct {
	PromptTokens int  `yaml:"prompt_tokens"`
	MaxTokens    int  `yaml:"max_tokens"`
	Stream       bool `yaml:"stream"`
}

// Series is one measured or placeholder curve.
type Series struct {
	ID       string `yaml:"id"`
	Status   string `yaml:"status"`
	Artifact string `yaml:"artifact"`

	IplaneVersion string `yaml:"iplane_version"`
	CapturedAt    string `yaml:"captured_at"`
	Model         string `yaml:"model"`

	Hardware Hardware `yaml:"hardware"`
	Workload Workload `yaml:"workload"`

	// RateUSDHr is the denominator of every cost figure drawn from this
	// series, and RateSource records whether anyone wrote it down. The 120k
	// run's rate was not recorded and had to be back-solved, which is
	// exactly the provenance gap this field exists to make visible rather
	// than to hide.
	RateUSDHr  float64 `yaml:"rate_usd_hr"`
	RateSource string  `yaml:"rate_source"`
	RateBasis  string  `yaml:"rate_basis"`

	Ladder   []int    `yaml:"ladder"`
	Grade    string   `yaml:"grade"`
	Caveats  []string `yaml:"caveats"`
	Basis    string   `yaml:"basis"`
	Validate string   `yaml:"validates_by"`
}

// Derived is a figure computed from a series, with the check that proves it.
type Derived struct {
	ID     string `yaml:"id"`
	Status string `yaml:"status"`
	From   string `yaml:"from"`
	// Check names the re-derivation to run. "literal" means none is
	// possible, which a measured entry may only use when its Label carries
	// the derivation in prose (a fit, say).
	Check        string  `yaml:"check"`
	Value        float64 `yaml:"value"`
	Label        string  `yaml:"label"`
	Basis        string  `yaml:"basis"`
	Concurrency  int     `yaml:"concurrency"`
	ConcurrencyA int     `yaml:"concurrency_a"`
	ConcurrencyB int     `yaml:"concurrency_b"`
}

// Run is one rental, including the ones that produced nothing.
type Run struct {
	ID        string  `yaml:"id"`
	Status    string  `yaml:"status"`
	Outcome   string  `yaml:"outcome"`
	Date      string  `yaml:"date"`
	RateUSDHr float64 `yaml:"rate_usd_hr"`
	Minutes   float64 `yaml:"minutes"`
	USD       float64 `yaml:"usd"`
	Produced  string  `yaml:"produced"`
	Note      string  `yaml:"note"`
}

type Registry struct {
	SchemaVersion int       `yaml:"schema_version"`
	Series        []Series  `yaml:"series"`
	Derived       []Derived `yaml:"derived"`
	Runs          []Run     `yaml:"runs"`
}

// Load reads the registry. root is the repo root, so artifact paths in the
// file stay repo-relative and mean the same thing from any caller.
func Load(root string) (*Registry, error) {
	raw, err := os.ReadFile(filepath.Join(root, "docs", "data", "series.yaml"))
	if err != nil {
		return nil, fmt.Errorf("read registry: %w", err)
	}
	var r Registry
	if err := yaml.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("parse registry: %w", err)
	}
	return &r, nil
}

// SeriesByID indexes the series so Derived.From can be resolved.
func (r *Registry) SeriesByID() map[string]Series {
	out := make(map[string]Series, len(r.Series))
	for _, s := range r.Series {
		out[s.ID] = s
	}
	return out
}

// Level is one concurrency point read back out of an artifact.
type Level struct {
	Concurrency  int
	TokensPerSec float64
	ITLP95Ms     float64
	Successes    int
}

// ReadArtifact parses a committed sweep CSV, skipping the '#' provenance
// header the sweep writes above the column row.
func ReadArtifact(path string) ([]Level, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var kept []string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
			continue
		}
		kept = append(kept, line)
	}
	rows, err := csv.NewReader(strings.NewReader(strings.Join(kept, "\n"))).ReadAll()
	if err != nil {
		return nil, err
	}
	if len(rows) < 2 {
		return nil, fmt.Errorf("%s: no data rows", path)
	}
	col := map[string]int{}
	for i, name := range rows[0] {
		col[name] = i
	}
	need := []string{"concurrency", "tokens_per_sec", "itl_p95_ms", "successes"}
	for _, c := range need {
		if _, ok := col[c]; !ok {
			return nil, fmt.Errorf("%s: artifact has no %q column", path, c)
		}
	}
	var out []Level
	for _, row := range rows[1:] {
		n, _ := strconv.Atoi(row[col["concurrency"]])
		tps, _ := strconv.ParseFloat(row[col["tokens_per_sec"]], 64)
		itl, _ := strconv.ParseFloat(row[col["itl_p95_ms"]], 64)
		s, _ := strconv.Atoi(row[col["successes"]])
		out = append(out, Level{Concurrency: n, TokensPerSec: tps, ITLP95Ms: itl, Successes: s})
	}
	return out, nil
}

// CostPerM is the rented hour divided by the tokens that hour produced.
// Zero rate or zero throughput returns 0 and ok=false, because a fabricated
// zero here would read as free inference.
func CostPerM(rateUSDHr, tokensPerSec float64) (float64, bool) {
	if rateUSDHr <= 0 || tokensPerSec <= 0 {
		return 0, false
	}
	return rateUSDHr / (tokensPerSec * 3600) * 1e6, true
}

// Recompute re-derives a Derived's value from its series' artifact and
// returns it. A check it does not implement is an error rather than a pass,
// since an unrecognised check name silently approving the figure is the
// failure mode this whole file exists to prevent.
func Recompute(d Derived, s Series, levels []Level) (float64, error) {
	find := func(n int) (Level, error) {
		for _, l := range levels {
			if l.Concurrency == n {
				return l, nil
			}
		}
		return Level{}, fmt.Errorf("%s: no level at concurrency %d", s.ID, n)
	}

	switch d.Check {
	case "literal":
		return d.Value, nil

	case "min_cost_per_m", "argmin_cost_concurrency":
		best, bestN, found := math.Inf(1), 0, false
		for _, l := range levels {
			c, ok := CostPerM(s.RateUSDHr, l.TokensPerSec)
			if !ok {
				continue
			}
			if c < best {
				best, bestN, found = c, l.Concurrency, true
			}
		}
		if !found {
			return 0, fmt.Errorf("%s: no level yielded a cost", s.ID)
		}
		if d.Check == "argmin_cost_concurrency" {
			return float64(bestN), nil
		}
		return best, nil

	case "cost_per_m_at":
		l, err := find(d.Concurrency)
		if err != nil {
			return 0, err
		}
		c, ok := CostPerM(s.RateUSDHr, l.TokensPerSec)
		if !ok {
			return 0, fmt.Errorf("%s: no cost at concurrency %d", s.ID, d.Concurrency)
		}
		return c, nil

	case "ratio_itl_p95":
		a, err := find(d.ConcurrencyA)
		if err != nil {
			return 0, err
		}
		b, err := find(d.ConcurrencyB)
		if err != nil {
			return 0, err
		}
		if a.ITLP95Ms <= 0 {
			return 0, fmt.Errorf("%s: no itl_p95 at concurrency %d", s.ID, d.ConcurrencyA)
		}
		return b.ITLP95Ms / a.ITLP95Ms, nil

	default:
		return 0, fmt.Errorf("%s: unknown check %q", d.ID, d.Check)
	}
}

// Agrees reports whether a re-derived figure matches the recorded one.
func Agrees(recorded, recomputed float64) bool {
	if recorded == 0 {
		return recomputed == 0
	}
	return math.Abs(recomputed-recorded)/math.Abs(recorded) <= tolerance
}
