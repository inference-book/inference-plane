package cmd

import (
	"strings"
	"testing"
)

func TestSynthPromptLandsNearTheRequestedTokenCount(t *testing.T) {
	for _, want := range []int{100, 1000, 8192, 40000} {
		got := len(synthPrompt(want)) / charsPerToken
		if got < want-1 || got > want+1 {
			t.Errorf("asked for %d tokens, got about %d", want, got)
		}
	}
}

// Zero keeps the short built-in prompts, so every demo and every existing
// flag combination sends what it sent before.
func TestSynthPromptWithoutALengthKeepsTheShortPrompts(t *testing.T) {
	got := synthPrompt(0)
	if len(got) > 200 {
		t.Errorf("got %d characters for an unspecified length, want one of the short prompts", len(got))
	}
}

// Concurrent requests must not all send the same text. Identical prefixes
// would make a prefix-caching engine report a hit rate describing the
// load generator rather than the workload, which is the contamination a
// KV-pressure measurement cannot tolerate.
func TestSynthPromptRotatesSoRequestsDiffer(t *testing.T) {
	seen := map[string]bool{}
	for range 8 {
		seen[synthPrompt(500)] = true
	}
	if len(seen) < 8 {
		t.Errorf("8 prompts produced %d distinct texts", len(seen))
	}
}

// A prompt longer than the corpus tiles it rather than truncating, which
// is a real limitation at very long contexts and is documented as one.
func TestSynthPromptTilesBeyondTheCorpus(t *testing.T) {
	want := len(corpusText)/charsPerToken*3 + 1000
	got := synthPrompt(want)
	if len(got) < want*charsPerToken-charsPerToken {
		t.Errorf("got %d characters for %d tokens; it truncated at the end of the corpus", len(got), want)
	}
}

// The corpus has to be prose. A filler check rather than a content check:
// generated filler repeats a handful of words, so a low distinct-word
// ratio is the thing to catch.
func TestCorpusIsProseRatherThanFiller(t *testing.T) {
	words := strings.Fields(corpusText)
	if len(words) < 10_000 {
		t.Fatalf("corpus has %d words, too few to draw a long prompt from", len(words))
	}
	distinct := map[string]bool{}
	for _, w := range words {
		distinct[w] = true
	}
	if ratio := float64(len(distinct)) / float64(len(words)); ratio < 0.05 {
		t.Errorf("distinct-word ratio %.3f reads as filler rather than prose", ratio)
	}
}
