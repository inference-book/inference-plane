// Package dashboards checks the provisioned Grafana dashboards against
// metric-names.yaml.
//
// metric-names.yaml exists because drift between code and prose is
// silently corrosive: the chapter says one name, the code emits another,
// and the dashboard returns nothing. Code and the book are already
// generated from it. Dashboards were not checked against it at all, so a
// panel could reference a metric nobody emits and the only symptom would
// be an empty graph in a screenshot.
package dashboards

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	dashboardDir = "../../deploy/grafana/provisioning/dashboards"
	namesFile    = "../../metric-names.yaml"
)

type dashboard struct {
	Title  string  `json:"title"`
	UID    string  `json:"uid"`
	Panels []panel `json:"panels"`
}

type panel struct {
	Title      string  `json:"title"`
	Type       string  `json:"type"`
	Targets    []query `json:"targets"`
	Datasource *struct {
		UID string `json:"uid"`
	} `json:"datasource"`
}

type query struct {
	Expr string `json:"expr"`
}

// exportedNames reads metric-names.yaml and returns the Prometheus
// spelling of every declared metric.
//
// The OTel Prometheus exporter replaces dots with underscores and leaves
// an existing _total suffix alone, which is why the vocabulary declares
// `instance.uptime.seconds.total` and dashboards query
// `instance_uptime_seconds_total`.
func exportedNames(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(namesFile)
	if err != nil {
		t.Fatalf("read %s: %v", namesFile, err)
	}
	var doc struct {
		Metrics []struct {
			Value string `yaml:"value"`
		} `yaml:"metrics"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", namesFile, err)
	}
	if len(doc.Metrics) == 0 {
		t.Fatalf("%s declares no metrics", namesFile)
	}
	out := map[string]bool{}
	for _, m := range doc.Metrics {
		out[strings.ReplaceAll(m.Value, ".", "_")] = true
	}
	return out
}

// promQLFunctions and reserved words that look like metric names to the
// identifier regex but are not.
var notMetrics = map[string]bool{
	"histogram_quantile": true, "clamp_min": true, "clamp_max": true,
	"label_replace": true, "label_join": true,
	"sum": true, "rate": true, "irate": true, "increase": true, "avg": true,
	"min": true, "max": true, "count": true, "topk": true, "bottomk": true,
	"by": true, "without": true, "on": true, "ignoring": true,
	"group_left": true, "group_right": true, "offset": true, "bool": true,
	"le": true, "quantile": true, "delta": true, "idelta": true,
}

// histogramSuffixes are the series a histogram fans out into. A panel
// querying inference_request_duration_bucket is querying the declared
// inference.request.duration.
var histogramSuffixes = []string{"_bucket", "_sum", "_count"}

var identifier = regexp.MustCompile(`[a-zA-Z_][a-zA-Z0-9_]*`)

// metricsIn pulls candidate metric names out of a PromQL expression.
//
// Label names and label values would otherwise be picked up, so anything
// inside braces or after a `by (` / `without (` grouping is stripped
// first. What survives is the set of bare identifiers, which in these
// dashboards is metric names plus function names.
func metricsIn(expr string) []string {
	// Drop label matchers and legend interpolations.
	expr = regexp.MustCompile(`\{[^}]*\}`).ReplaceAllString(expr, " ")
	// Drop grouping clauses, whose contents are label names.
	expr = regexp.MustCompile(`(?i)\b(by|without|on|ignoring|group_left|group_right)\s*\([^)]*\)`).ReplaceAllString(expr, " ")
	// Drop duration literals like [5m] and $__rate_interval.
	expr = regexp.MustCompile(`\[[^\]]*\]`).ReplaceAllString(expr, " ")
	expr = regexp.MustCompile(`\$__[a-zA-Z_]+`).ReplaceAllString(expr, " ")

	seen := map[string]bool{}
	var out []string
	for _, id := range identifier.FindAllString(expr, -1) {
		if notMetrics[strings.ToLower(id)] || seen[id] {
			continue
		}
		// A bare word with no underscore is a function or keyword we did
		// not enumerate, never one of this project's metric names.
		if !strings.Contains(id, "_") {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func loadDashboards(t *testing.T) map[string]dashboard {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dashboardDir, "*.json"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(paths) == 0 {
		t.Fatalf("no dashboards found under %s", dashboardDir)
	}
	out := map[string]dashboard{}
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		var d dashboard
		if err := json.Unmarshal(raw, &d); err != nil {
			t.Fatalf("%s: invalid json: %v", filepath.Base(p), err)
		}
		out[filepath.Base(p)] = d
	}
	return out
}

// TestEveryPanelQueriesADeclaredMetric is the drift gate. A panel naming
// a metric nobody emits renders as an empty graph, which is invisible in
// review and obvious only in a screenshot somebody already published.
func TestEveryPanelQueriesADeclaredMetric(t *testing.T) {
	declared := exportedNames(t)

	for name, d := range loadDashboards(t) {
		for _, p := range d.Panels {
			for _, q := range p.Targets {
				if strings.TrimSpace(q.Expr) == "" {
					continue
				}
				for _, m := range metricsIn(q.Expr) {
					base := m
					for _, suf := range histogramSuffixes {
						base = strings.TrimSuffix(base, suf)
					}
					if declared[m] || declared[base] {
						continue
					}
					t.Errorf("%s: panel %q queries %q, which metric-names.yaml does not declare\n  expr: %s",
						name, p.Title, m, q.Expr)
				}
			}
		}
	}
}

func TestDashboardUIDsAreUnique(t *testing.T) {
	seen := map[string]string{}
	for name, d := range loadDashboards(t) {
		if d.UID == "" {
			t.Errorf("%s: no uid; Grafana would generate one per restart", name)
			continue
		}
		if prev, dup := seen[d.UID]; dup {
			t.Errorf("%s and %s share uid %q; the second silently overwrites the first", prev, name, d.UID)
		}
		seen[d.UID] = name
	}
}

// TestQueryPanelsNameADatasource catches the panel that renders against
// whatever Grafana's default happens to be, which is the kind of thing
// that works locally and shows nothing in a fresh stack.
func TestQueryPanelsNameADatasource(t *testing.T) {
	for name, d := range loadDashboards(t) {
		for _, p := range d.Panels {
			if len(p.Targets) == 0 {
				continue
			}
			if p.Datasource == nil || p.Datasource.UID == "" {
				t.Errorf("%s: panel %q has queries but names no datasource", name, p.Title)
			}
		}
	}
}
