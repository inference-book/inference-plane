package router

// Context-length buckets for the token counter's context_bucket label.
//
// Bucketed rather than exact because the label lands on a per-request
// counter, and a distinct series per prompt length would be unbounded
// cardinality driven by whatever clients happen to send. Seven values
// bound it, and they are upper bounds in the histogram sense: a request
// falls in the first bucket it does not exceed.
//
// The edges are powers of two spanning the range the chapter actually
// compares, from a short chat turn to the million-token context the
// frontier models advertise. They are labelled with the round number an
// operator typed rather than the exact power of two, since --prompt-tokens
// 8000 and a 8192-token ceiling are the same experiment.
//
// Within one sweep every request carries the same value, so this label
// does not split a single run. It makes several runs comparable on one
// panel, which is the comparison the cost curve is read from.
var contextBuckets = []struct {
	max   int64
	label string
}{
	{512, "512"},
	{2048, "2k"},
	{8192, "8k"},
	{32768, "32k"},
	{131072, "128k"},
	{524288, "512k"},
}

// contextBucketUnknown is what a response with no usable prompt count
// reports.
//
// Not folded into the smallest bucket. A missing figure and a short
// prompt are different facts, and collapsing them would let an engine
// that omits prompt_tokens quietly populate the 512 series and drag a
// cost curve toward a context length nobody ran.
const contextBucketUnknown = "unknown"

// contextBucket maps a prompt length onto its bucket label.
func contextBucket(promptTokens int64) string {
	if promptTokens <= 0 {
		return contextBucketUnknown
	}
	for _, b := range contextBuckets {
		if promptTokens <= b.max {
			return b.label
		}
	}
	return "1M+"
}
