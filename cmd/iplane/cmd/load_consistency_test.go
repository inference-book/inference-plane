package cmd

import (
	"strings"
	"testing"
)

// The row that motivated the check: the 120k level reported 6.2 tokens per
// request from the engine's usage block while its own frame count implied
// 103. Every published figure derived from the smaller number (#451).
func TestInconsistencyNoteCatchesTheTruncated120kRow(t *testing.T) {
	note := inconsistencyNote(sweepLevel{
		Concurrency: 32,
		Successes:   66,
		Tokens:      408,
		TTFTSamples: 19,
		ITLSamples:  1969,
	})

	if note == "" {
		t.Fatal("a 17x disagreement between the two token counts should be reported")
	}
	for _, want := range []string{"6.2", "103", "do not chart"} {
		if !strings.Contains(note, want) {
			t.Errorf("note should mention %q, got: %s", want, note)
		}
	}
}

// A healthy row's two counts agree, because tokens and frames measure the
// same thing. Real numbers from the 8k GLM sweep and the 0.5B rehearsal.
func TestInconsistencyNoteSilentOnAgreeingRows(t *testing.T) {
	for _, l := range []sweepLevel{
		{Concurrency: 1, Successes: 122, Tokens: 45815, TTFTSamples: 96, ITLSamples: 45798},
		{Concurrency: 8, Successes: 408, Tokens: 146612, TTFTSamples: 328, ITLSamples: 147826},
		{Concurrency: 1, Successes: 74, Tokens: 35701, TTFTSamples: 74, ITLSamples: 35685},
	} {
		if note := inconsistencyNote(l); note != "" {
			t.Errorf("level %d: agreeing counts should be silent, got: %s", l.Concurrency, note)
		}
	}
}

// Nothing to cross-check means no claim either way. A non-streamed run has no
// frames, and a row with no successes has no per-request figure at all.
func TestInconsistencyNoteSilentWhenUncomparable(t *testing.T) {
	for name, l := range map[string]sweepLevel{
		"no stream":    {Successes: 100, Tokens: 40000, TTFTSamples: 0, ITLSamples: 0},
		"no successes": {Successes: 0, Tokens: 0, TTFTSamples: 0, ITLSamples: 0},
		"tiny replies": {Successes: 100, Tokens: 200, TTFTSamples: 100, ITLSamples: 100},
	} {
		if note := inconsistencyNote(l); note != "" {
			t.Errorf("%s: should be silent, got: %s", name, note)
		}
	}
}
