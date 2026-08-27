package series

import (
	"path/filepath"
	"testing"
)

// repoRoot locates the checkout from this package's directory, so the test
// reads the same registry a caller at the root would.
func repoRoot() string { return filepath.Join("..", "..") }

func load(t *testing.T) *Registry {
	t.Helper()
	r, err := Load(repoRoot())
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}
	if r.SchemaVersion != 1 {
		t.Fatalf("schema_version = %d, want 1", r.SchemaVersion)
	}
	return r
}

// The check the whole file exists for. Every figure claiming to be measured
// is re-derived from the artifact its entry names, and a disagreement fails
// the build rather than reaching a chapter.
func TestMeasuredDerivedFiguresMatchTheirArtifacts(t *testing.T) {
	r := load(t)
	byID := r.SeriesByID()

	for _, d := range r.Derived {
		if d.Status != StatusMeasured {
			continue
		}
		s, ok := byID[d.From]
		if !ok {
			// A fit can derive from another derived entry rather than from a
			// series; that is only legitimate when nothing is recomputable.
			if d.Check == "literal" {
				continue
			}
			t.Errorf("%s: from %q which is not a series", d.ID, d.From)
			continue
		}
		if d.Check == "literal" {
			if d.Label == "" && d.Basis == "" {
				t.Errorf("%s is measured with an unrunnable check and no basis, so nothing supports it", d.ID)
			}
			continue
		}
		levels, err := ReadArtifact(filepath.Join(repoRoot(), s.Artifact))
		if err != nil {
			t.Errorf("%s: %v", d.ID, err)
			continue
		}
		got, err := Recompute(d, s, levels)
		if err != nil {
			t.Errorf("%s: %v", d.ID, err)
			continue
		}
		if !Agrees(d.Value, got) {
			t.Errorf("%s: recorded %.4f, artifact says %.4f (check %s on %s)",
				d.ID, d.Value, got, d.Check, s.ID)
		}
	}
}

// A measured series names a file that exists and parses. Without this the
// previous test passes vacuously for any entry whose artifact was deleted.
func TestMeasuredSeriesHaveReadableArtifacts(t *testing.T) {
	r := load(t)
	for _, s := range r.Series {
		if s.Status != StatusMeasured {
			continue
		}
		if s.Artifact == "" {
			t.Errorf("%s is measured with no artifact", s.ID)
			continue
		}
		levels, err := ReadArtifact(filepath.Join(repoRoot(), s.Artifact))
		if err != nil {
			t.Errorf("%s: %v", s.ID, err)
			continue
		}
		if len(levels) == 0 {
			t.Errorf("%s: artifact has no levels", s.ID)
		}
		for _, want := range s.Ladder {
			found := false
			for _, l := range levels {
				if l.Concurrency == want {
					found = true
				}
			}
			if !found {
				t.Errorf("%s: ladder claims concurrency %d, artifact has no such row", s.ID, want)
			}
		}
	}
}

// Status hygiene. The statuses only mean anything if the shape of an entry
// has to match the status it claims: a measured entry without an artifact,
// or a simulated one carrying an artifact path, is a mislabel rather than a
// stylistic choice.
func TestStatusesMatchTheShapeOfTheirEntries(t *testing.T) {
	r := load(t)
	valid := map[string]bool{
		StatusMeasured: true, StatusPredicted: true,
		StatusSimulated: true, StatusPending: true,
	}
	for _, s := range r.Series {
		if !valid[s.Status] {
			t.Errorf("%s: unknown status %q", s.ID, s.Status)
		}
		switch s.Status {
		case StatusMeasured:
			if s.RateSource == "" {
				t.Errorf("%s is measured without a rate_source, so its cost figures have an unattributed denominator", s.ID)
			}
		case StatusSimulated, StatusPredicted:
			if s.Artifact != "" {
				t.Errorf("%s is %s but names an artifact; if an artifact exists it should be measured", s.ID, s.Status)
			}
			if s.Basis == "" {
				t.Errorf("%s is %s with no basis, which makes it an invented number nobody can audit", s.ID, s.Status)
			}
		}
		if s.Status == StatusSimulated && s.Validate == "" {
			t.Errorf("%s is simulated with no validates_by, so nothing says what run would promote it", s.ID)
		}
	}
	for _, d := range r.Derived {
		if !valid[d.Status] {
			t.Errorf("%s: unknown status %q", d.ID, d.Status)
		}
		if d.Status != StatusMeasured && d.Basis == "" {
			t.Errorf("%s is %s with no basis", d.ID, d.Status)
		}
	}
}

// The rate is the denominator of every cost figure, and one of ours was
// never written down. Inferring it is allowed; inferring it silently is not.
func TestInferredRatesSayHowTheyWereInferred(t *testing.T) {
	r := load(t)
	for _, s := range r.Series {
		if s.RateSource == "inferred" && s.RateBasis == "" {
			t.Errorf("%s has an inferred rate with no rate_basis", s.ID)
		}
	}
}
