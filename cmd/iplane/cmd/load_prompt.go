package cmd

import (
	_ "embed"
	"strings"
	"sync/atomic"
)

// corpusText is the prompt source for --prompt-tokens.
//
// Real prose rather than repeated filler, for two reasons that both bear
// on the measurement. Filler tokenizes unrealistically, so a prompt built
// from it costs a different number of tokens than its length suggests.
// And filler makes every request's prefix identical, so an engine with
// prefix caching reports a hit rate describing the load generator rather
// than the workload, which is exactly the thing a KV-pressure measurement
// must not be contaminated by.
//
// See corpus/README.md for the provenance and the licence position.
//
//go:embed corpus/alice.txt
var corpusText string

// charsPerToken is the ratio used to turn a requested token count into a
// slice of the corpus.
//
// Four is the usual rule of thumb for English prose in a byte-pair
// vocabulary, and it is what the mock backend already uses to report
// prompt_tokens, so a run against the mock has the requested and the
// reported counts agree. Against a real engine they will not agree
// exactly; expect the tokenizer to land within ten or twenty percent, and
// read the engine's own reported prompt_tokens as the truth when the
// difference matters.
const charsPerToken = 4

// promptOffset rotates the window each request draws from, so a run does
// not send the same prompt every time.
//
// A shared atomic rather than a per-worker counter, because the point is
// that concurrent requests differ from each other, not that each worker's
// stream differs from its own history.
var promptOffset atomic.Int64

// synthPrompt returns a prompt of approximately the requested token
// count, drawn from the corpus.
//
// The window rotates, so the requests in a run overlap the way real
// traffic against a shared document set does rather than being identical
// or being disjoint. A request longer than the corpus tiles it, which is
// a real limitation at very long contexts and is called out in the
// corpus README: a million-token prompt is the same book about
// twenty-seven times, and a prefix-caching engine will notice.
func synthPrompt(tokens int) string {
	if tokens <= 0 {
		return pickLoadPrompt()
	}
	want := tokens * charsPerToken
	start := int(promptOffset.Add(int64(want/8))) % len(corpusText)

	var b strings.Builder
	b.Grow(want + 1)
	for b.Len() < want {
		end := start + (want - b.Len())
		if end > len(corpusText) {
			end = len(corpusText)
		}
		b.WriteString(corpusText[start:end])
		start = 0
	}
	return b.String()
}
