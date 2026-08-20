package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/vrambudget"
)

var (
	budgetQuantization   string
	budgetKVQuantization string
	budgetMaxModelLen    int32
	budgetMaxBatch       int32
	budgetTP             int32
	budgetVRAMGB         float64
	budgetUtilization    float64
	budgetMaxCards       int32
	budgetRevision       string
	budgetOutput         string
	budgetSessionsAt     string
	budgetExpertParallel bool
)

var modelBudgetCmd = &cobra.Command{
	Use:   "budget <model>",
	Short: "Work out whether a model fits a card, before renting one",
	Long: `Report what a model costs in VRAM under a deploy plan, and the
fewest cards that hold it.

Reads the model's published shape and does arithmetic. Rents nothing,
prices nothing and places nothing, so it is free to run and safe to run
often. ` + "`iplane capacity`" + ` is the other half of the question: this
command says what the model needs, that one says who has it.

Every card count is reported, not only the one that works, because a
verdict without its terms says an operator was wrong without saying
where. Exit status is 0 when some shape fits and 1 when none does, so it
works as a pre-flight gate in front of a deploy.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		opts, err := budgetOptionsFromFlags()
		if err != nil {
			return err
		}
		spec, err := budgetModelSpec(args[0], budgetRevision, cmd.Flags().Changed("revision"))
		if err != nil {
			return err
		}

		// Through the client, not the store directly, for the reason
		// `model describe` gives: reading a gated model's config needs
		// HF_TOKEN, which lives wherever the daemon lives.
		client, err := buildCapacityClient()
		if err != nil {
			return err
		}
		resp, err := client.DescribeModel(cmd.Context(), &provisionerv1.DescribeModelRequest{ModelSpec: spec})
		if err != nil {
			return err
		}

		// A checkpoint that already chose a precision cannot be sized at
		// another one. The parameter count is the hub's element
		// accounting, and packing changes what an element is, so every
		// row of the table would price a second quantization on top of
		// the first (#382).
		if q := resp.GetArchitecture().GetQuantization(); q != "" {
			return fmt.Errorf("%s is published at %s, so its parameter count is packed elements rather than parameters "+
				"and sizing it at another precision would quantize it twice. Budget the unquantized model and read the "+
				"result as an upper bound, or size this one from the repo's own file sizes", spec, q)
		}

		fits, err := renderModelBudget(cmd.OutOrStdout(), spec, resp.GetArchitecture(), opts)
		if err != nil {
			return err
		}
		if !fits {
			// Empty message on purpose: the table above already said
			// what overran and by how much, and a stderr line repeating
			// it would read as a second, separate failure.
			return exitWithCode(1)
		}
		return nil
	},
}

// budgetOpts is the operator's half of the budget: the plan they intend
// to deploy under and the card they intend to deploy onto. The model's
// half arrives separately, from the hub.
type budgetOpts struct {
	plan        vrambudget.Plan
	cardBytes   int64
	cardGB      float64 // as the operator typed it, for the header line
	utilization float64
	maxCards    int32
	tp          int32 // 0 means sweep every candidate count
	format      string
	// expertParallel switches what a row of the table is. Without it a
	// row is a tensor width and --tp narrows the sweep to one of them.
	// With it a row is the expert width, --tp is the tensor width inside
	// it, and the data-parallel width is what is left over.
	expertParallel bool
	// sessionsAt is the context ladder for the concurrency question.
	// Empty means the operator asked the default question, which is
	// whether one named batch fits.
	sessionsAt []int32
}

// budgetOptionsFromFlags validates the flags into a plan.
//
// Stricter than the underlying library on two points, deliberately.
// UsableBytes falls back to the default utilization for anything outside
// (0, 1] and Sweep rewrites a max-card count below 1, both silently. A
// library quietly correcting a caller is defensible; a command quietly
// answering a different question than the operator typed is not, since
// the answer looks exactly as authoritative either way.
func budgetOptionsFromFlags() (budgetOpts, error) {
	var o budgetOpts

	weights, err := vrambudget.ParsePrecision(budgetQuantization)
	if err != nil {
		return o, fmt.Errorf("--quantization: %w", err)
	}
	cache := weights
	if budgetKVQuantization != "" {
		cache, err = vrambudget.ParsePrecision(budgetKVQuantization)
		if err != nil {
			return o, fmt.Errorf("--kv-cache-quantization: %w", err)
		}
	}

	if budgetVRAMGB <= 0 {
		return o, fmt.Errorf("--vram-gb is required: there is no default card, and the whole answer turns on which one you are renting")
	}
	if budgetUtilization <= 0 || budgetUtilization > 1 {
		return o, fmt.Errorf("--gpu-memory-utilization must be in (0, 1], got %v (vLLM's own flag is a fraction, so 0.9 rather than 90)", budgetUtilization)
	}
	if budgetMaxCards < 1 {
		return o, fmt.Errorf("--max-cards must be at least 1, got %d", budgetMaxCards)
	}
	if budgetMaxBatch < 1 {
		return o, fmt.Errorf("--max-batch must be at least 1, got %d", budgetMaxBatch)
	}
	if budgetMaxModelLen < 0 {
		return o, fmt.Errorf("--max-model-len must be positive, got %d", budgetMaxModelLen)
	}
	if budgetTP < 0 {
		return o, fmt.Errorf("--tp must be at least 1, got %d", budgetTP)
	}
	if budgetTP > 0 && budgetMaxCards < budgetTP {
		return o, fmt.Errorf("--tp %d exceeds --max-cards %d", budgetTP, budgetMaxCards)
	}
	if budgetOutput != outputTable && budgetOutput != outputJSON {
		return o, fmt.Errorf("--output must be %q or %q, got %q", outputTable, outputJSON, budgetOutput)
	}

	if budgetExpertParallel && !powerOfTwo(budgetTP) {
		// Same answer the tensor sweep gives an operator asking about 3
		// or 6 cards, and for the same reason: engines shard a layer
		// across powers of two, and here the tensor width is a real
		// width rather than a row to look up.
		return o, fmt.Errorf("--tp %d is not a tensor-parallel width this budget plans for: engines shard across powers of two", budgetTP)
	}

	o.plan = vrambudget.Plan{Weights: weights, KVCache: cache, MaxModelLen: budgetMaxModelLen, MaxBatch: budgetMaxBatch}
	o.expertParallel = budgetExpertParallel
	if o.expertParallel {
		// The plan carries the tensor width from here on. Zero and one
		// both mean one card, which is what Plan already reads them as.
		o.plan.TPSize = budgetTP
	}
	// The label is a binary count, so "80 GB" is 80 GiB and holds 85.9
	// decimal GB. Reading a vendor's label as decimal removed seven
	// percent of every card before the arithmetic started, which is
	// enough to refuse a shape that fits (#323). The header prints both
	// figures rather than quietly reinterpreting the number typed.
	o.cardBytes = int64(budgetVRAMGB * float64(vrambudget.GiB))
	o.cardGB = budgetVRAMGB
	o.utilization = budgetUtilization
	o.maxCards = budgetMaxCards
	o.tp = budgetTP
	o.format = budgetOutput

	ladder, err := parseContextLadder(budgetSessionsAt)
	if err != nil {
		return o, err
	}
	o.sessionsAt = ladder

	return o, nil
}

// powerOfTwo accepts 0 alongside the powers, since an unset --tp is a
// tensor width of one rather than a width nobody named.
func powerOfTwo(n int32) bool {
	return n >= 0 && n&(n-1) == 0
}

// planAtCards sizes the plan for a row of n cards, on whichever axis the
// operator asked for.
//
// One helper rather than an assignment at each table, so the budget table
// and the sessions table cannot end up describing different shapes for
// the same row.
func planAtCards(o budgetOpts, n int32) vrambudget.Plan {
	plan := o.plan
	if o.expertParallel {
		plan.EPSize = n
		return plan
	}
	plan.TPSize = n
	return plan
}

// expertParallelPlanSuffix names the shape in the header, since the same
// card count means two different arrangements depending on the flag and
// the header is the only place that says which one was computed.
func expertParallelPlanSuffix(o budgetOpts) string {
	if !o.expertParallel {
		return ""
	}
	return fmt.Sprintf(", experts across every card, tensor width %d", o.plan.TensorWidth())
}

// expertParallelRowNote reads one row of the table out in the vocabulary
// the deploy flags use, because `--ep` there is a degree and the row here
// is a card count, and an operator carrying the answer to a deploy has to
// translate between them.
//
// The widest row rather than a general statement, since the sentence is
// only legible with real numbers in it and that is the row an operator
// sizing a machine is reading.
func expertParallelRowNote(o budgetOpts, cards int32) string {
	tp := o.plan.TensorWidth()
	return fmt.Sprintf("each row spreads the routed experts across all of its cards: %d cards is --ep %d --tp %d, "+
		"so %d data-parallel rank(s) each holding a whole copy of the attention, the embeddings and the dense layers.",
		cards, cards, tp, cards/tp)
}

// widestRow is the largest card count the table actually reported,
// skipped rows included. A note quoting --max-cards instead would name a
// row the sweep never walked whenever the ceiling is not a power of two.
func widestRow(candidates []vrambudget.Candidate) int32 {
	var n int32
	for _, c := range candidates {
		if c.Cards > n {
			n = c.Cards
		}
	}
	return n
}

// parseContextLadder reads the --sessions-at list.
//
// Accepts the units operators say rather than only the token counts they
// mean, since the whole list is powers of two and "1048576" is harder to
// check at a glance than "1M". k and M are binary here, matching how
// every context length is actually quoted: a 128k window is 131072
// tokens, not 128000.
func parseContextLadder(s string) ([]int32, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var out []int32
	for _, field := range strings.Split(s, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		mult := int64(1)
		switch {
		case strings.HasSuffix(field, "k"), strings.HasSuffix(field, "K"):
			mult, field = 1024, field[:len(field)-1]
		case strings.HasSuffix(field, "m"), strings.HasSuffix(field, "M"):
			mult, field = 1024*1024, field[:len(field)-1]
		}
		n, err := strconv.ParseInt(field, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("--sessions-at: %q is not a context length (want a token count, or one with a k or M suffix)", field)
		}
		if n <= 0 {
			return nil, fmt.Errorf("--sessions-at: context length must be positive, got %s", field)
		}
		total := n * mult
		if total > math.MaxInt32 {
			return nil, fmt.Errorf("--sessions-at: %s is longer than any engine addresses", field)
		}
		out = append(out, int32(total))
	}
	return out, nil
}

// writeSessionsTable answers the concurrency question across the context
// ladder and the card ladder at once.
//
// Two axes rather than one, because the interesting thing is not either
// number on its own. Concurrency collapsing as the context grows is half
// of it; the other half is how much of that a card count buys back, and
// for a model caching a compressed latent the answer is far less than an
// operator expects, since the latent is replicated on every card.
func writeSessionsTable(w io.Writer, spec string, a *provisionerv1.ModelArchitecture, o budgetOpts) (bool, error) {
	cardCounts, err := sessionCardLadder(a, o)
	if err != nil {
		return false, err
	}

	fmt.Fprintf(w, "%s  %s params  %d layers\n", spec, formatParamsShort(a.GetParams()), a.GetLayers())
	fmt.Fprintf(w, "plan   weights %s, cache %s%s\n", o.plan.Weights, o.plan.KVCache, expertParallelPlanSuffix(o))
	fmt.Fprintf(w, "card   %g GB (%g GiB = %s) at %.2f utilization = %s usable\n\n",
		o.cardGB, o.cardGB, formatGB(o.cardBytes), o.utilization,
		formatGB(vrambudget.UsableBytes(o.cardBytes, o.utilization)))

	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', tabwriter.AlignRight)
	fmt.Fprintf(tw, "context")
	for _, n := range cardCounts {
		fmt.Fprintf(tw, "\t%s", formatCards(n))
	}
	// Prefixed with spaces rather than tab-terminated: tabwriter leaves a
	// cell that is not tab-terminated unpadded, which is what keeps the
	// numeric columns right-aligned, and the gap has to come from
	// somewhere. Same trick the verdict column uses.
	fmt.Fprintf(tw, "\t   cache/session\n")

	any := false
	widest := cardCounts[len(cardCounts)-1]
	for _, ctx := range o.sessionsAt {
		fmt.Fprintf(tw, "%s", formatTokens(ctx))
		for _, cards := range cardCounts {
			plan := planAtCards(o, cards)
			plan.MaxModelLen = ctx
			n, err := vrambudget.MaxSessions(a, plan, o.cardBytes, o.utilization)
			if err != nil {
				return false, err
			}
			if n > 0 {
				any = true
				fmt.Fprintf(tw, "\t%d", n)
			} else {
				// A dash rather than a zero. Zero reads as a computed
				// answer, and what happened is that the shape never got
				// as far as costing a session.
				fmt.Fprintf(tw, "\t-")
			}
		}
		cache, _ := vrambudget.SessionCost(a, planAtCards(o, widest), ctx)
		fmt.Fprintf(tw, "\t   %s\n", formatGB(cache))
	}
	if err := tw.Flush(); err != nil {
		return false, err
	}

	fmt.Fprintln(w)
	if o.expertParallel {
		fmt.Fprintln(w, expertParallelRowNote(o, widest))
		// The cache divides by the tensor width and nothing else, so
		// under expert parallelism every row of this column is the same
		// number and naming the widest row would suggest otherwise.
		fmt.Fprintf(w, "cache/session is per card at tensor width %d, which every row shares.\n", o.plan.TensorWidth())
	} else {
		fmt.Fprintf(w, "cache/session is per card at %s.\n", formatCards(cardCounts[len(cardCounts)-1]))
	}
	if vrambudget.LatentCache(a) {
		fmt.Fprintln(w, "this model caches a compressed latent, which is replicated on every card rather than")
		fmt.Fprintln(w, "sharded, so cards buy room for weights and none for cache.")
	}
	if !any {
		fmt.Fprintln(w, "no shape on this ladder holds even one session.")
	}
	return any, nil
}

// sessionCardLadder is the card counts the sessions table reports.
//
// The operator's --tp when they named one, since they are asking about a
// shape they have in mind. Otherwise the same powers of two the budget
// sweep uses, so the two tables are read against each other.
//
// Under --expert-parallel --tp is not a row at all, so the ladder stays
// the card ladder and the counts a tensor width does not divide drop out
// of it, matching the rows the budget sweep skips for the same reason.
func sessionCardLadder(a *provisionerv1.ModelArchitecture, o budgetOpts) ([]int32, error) {
	if o.tp > 0 && !o.expertParallel {
		return []int32{o.tp}, nil
	}
	tp := o.plan.TensorWidth()
	var out []int32
	for n := int32(1); n <= o.maxCards; n *= 2 {
		if o.expertParallel && n%tp != 0 {
			continue
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("--max-cards %d leaves no card count to report", o.maxCards)
	}
	return out, nil
}

// formatCards labels a column the way the row it heads reads.
func formatCards(n int32) string {
	if n == 1 {
		return "1 card"
	}
	return fmt.Sprintf("%d cards", n)
}

// budgetModelSpec folds --revision into the spec grammar the hub read
// already parses (<org>/<name>:<revision>), so the flag needs no request
// field of its own.
//
// explicit distinguishes an operator who typed --revision from the flag's
// own default, which matters because a spec may pin its own revision. Two
// revisions that disagree is a contradiction rather than a precedence
// question, but only when a human chose both of them.
func budgetModelSpec(model, revision string, explicit bool) (string, error) {
	inline := ""
	if i := strings.IndexByte(model, ':'); i >= 0 {
		inline = model[i+1:]
	}
	switch {
	case inline != "" && explicit && revision != inline:
		return "", fmt.Errorf("model spec pins revision %q but --revision says %q; drop one", inline, revision)
	case inline != "" || !explicit:
		// Left bare when the flag was not typed. The hub defaults to main
		// on its own, so pinning it here would only put a revision in the
		// output that the operator did not ask about.
		return model, nil
	default:
		return model + ":" + revision, nil
	}
}

// renderModelBudget writes the budget and reports whether any shape fit.
//
// Fitting means Verdict Fits and nothing weaker. Tight clears only by
// eating the overhead band that stands in for the CUDA context and
// allocator fragmentation, which is the shape that starts and then dies
// under load, so counting it as a pass would make the exit code a worse
// signal than no exit code at all.
func renderModelBudget(w io.Writer, spec string, a *provisionerv1.ModelArchitecture, o budgetOpts) (bool, error) {
	if err := vrambudget.ValidateArch(a); err != nil {
		return false, err
	}
	// The concurrency question supplies its own context lengths, so it
	// does not need one defaulted and must not be refused for lacking
	// one. A model that publishes no trained window is exactly the case
	// where an operator names the ladder by hand.
	if len(o.sessionsAt) > 0 {
		if o.format == outputJSON {
			return writeSessionsJSON(w, spec, a, o)
		}
		return writeSessionsTable(w, spec, a, o)
	}

	if o.plan.MaxModelLen == 0 {
		if a.GetMaxPositionEmbeddings() <= 0 {
			return false, fmt.Errorf("%s publishes no max_position_embeddings, so there is no context length to default to; pass --max-model-len", spec)
		}
		o.plan.MaxModelLen = a.GetMaxPositionEmbeddings()
	}

	candidates, err := budgetCandidates(a, o)
	if err != nil {
		return false, err
	}

	fewest := int32(0)
	for _, c := range candidates {
		if c.SkipReason == "" && c.Verdict == vrambudget.Fits {
			fewest = c.Cards
			break
		}
	}

	if o.format == outputJSON {
		return fewest > 0, writeBudgetJSON(w, spec, a, o, candidates, fewest)
	}
	return fewest > 0, writeBudgetTable(w, spec, a, o, candidates, fewest)
}

// budgetCandidates is the sweep, or the single shape --tp pins it to.
//
// --tp narrows the sweep rather than computing separately, so the two
// modes cannot disagree about a row they share. Under --expert-parallel
// it does not narrow at all: there the row is the expert width and --tp
// is the tensor width every row is built around, so pinning a row to it
// would answer a question nobody asked.
func budgetCandidates(a *provisionerv1.ModelArchitecture, o budgetOpts) ([]vrambudget.Candidate, error) {
	if o.expertParallel {
		return vrambudget.SweepExpertParallel(a, o.plan, o.cardBytes, o.utilization, o.maxCards)
	}
	all, err := vrambudget.Sweep(a, o.plan, o.cardBytes, o.utilization, o.maxCards)
	if err != nil || o.tp == 0 {
		return all, err
	}
	for _, c := range all {
		if c.Cards == o.tp {
			return []vrambudget.Candidate{c}, nil
		}
	}
	// Sweep only walks powers of two, so an operator asking about 3 or 6
	// gets no row. That is a real answer about tensor parallelism rather
	// than a gap in the table.
	return nil, fmt.Errorf("--tp %d is not a tensor-parallel width this budget plans for: engines shard across powers of two", o.tp)
}

func writeBudgetTable(w io.Writer, spec string, a *provisionerv1.ModelArchitecture, o budgetOpts, candidates []vrambudget.Candidate, fewest int32) error {
	fmt.Fprintf(w, "%s  %s params  %d layers  %d kv-heads  head-dim %d\n",
		spec, formatParamsShort(a.GetParams()), a.GetLayers(), a.GetKvHeads(), a.GetHeadDim())
	fmt.Fprintf(w, "plan   weights %s, cache %s, %d tokens x %d sequences%s\n",
		o.plan.Weights, o.plan.KVCache, o.plan.MaxModelLen, o.plan.MaxBatch, expertParallelPlanSuffix(o))
	fmt.Fprintf(w, "card   %g GB (%g GiB = %s) at %.2f utilization = %s usable\n\n",
		o.cardGB, o.cardGB, formatGB(o.cardBytes),
		o.utilization, formatGB(vrambudget.UsableBytes(o.cardBytes, o.utilization)))

	// AlignRight so the columns compare down the page rather than across
	// it, which is what a reader scanning for the term that overran does.
	// The verdict is deliberately the last cell on the line: tabwriter
	// leaves a cell that is not tab-terminated alone, so the words stay
	// left-aligned while the numbers stay right-aligned.
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', tabwriter.AlignRight)
	fmt.Fprintf(tw, "cards\tweights\tcache\tactivation\toverhead\ttotal\t   verdict\n")
	var skipped []vrambudget.Candidate
	for _, c := range candidates {
		if c.SkipReason != "" {
			skipped = append(skipped, c)
			continue
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
			c.Cards,
			formatGB(c.Budget.WeightBytes), formatGB(c.Budget.KVBytes),
			formatGB(c.Budget.ActivationBytes), formatGB(c.Budget.OverheadBytes),
			formatGB(c.Budget.TotalBytes()), "   "+c.Verdict.String())
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	// Under the table rather than as blank rows in it. A gap in the
	// sequence reads as an oversight, and the reason a count is not a
	// candidate is longer than a column.
	if len(skipped) > 0 {
		fmt.Fprintln(w)
		for _, c := range skipped {
			fmt.Fprintf(w, "skipped %d cards: %s\n", c.Cards, c.SkipReason)
		}
	}

	fmt.Fprintln(w)
	if o.expertParallel {
		fmt.Fprintln(w, expertParallelRowNote(o, widestRow(candidates)))
		fmt.Fprintln(w)
	}
	if fewest > 0 {
		fmt.Fprintf(w, "fewest cards that fit: %d\n", fewest)
	} else {
		fmt.Fprintf(w, "fewest cards that fit: none within --max-cards %d\n", o.maxCards)
	}
	if term, frac, ok := largestTerm(candidates); ok {
		fmt.Fprintf(w, "largest term: %s, %.0f%% of the working set\n", term, frac*100)
	}
	return nil
}

// largestTerm names the term carrying most of the working set.
//
// This is the line the whole command exists to print. An operator who
// reaches for a smaller quantization is assuming the weights dominate,
// and past a few thousand tokens of context at any real batch size they
// do not, so quantizing moves the total barely at all and the context or
// the concurrency is what has to give. The table implies that; nobody
// reads it out of the table.
//
// Any row answers, because all three terms divide by the card count
// alike and the ratio is the same across the ladder. Overhead is
// excluded because it is a fraction of the other three by construction
// and could never be the largest.
func largestTerm(candidates []vrambudget.Candidate) (string, float64, bool) {
	for _, c := range candidates {
		if c.SkipReason != "" {
			continue
		}
		working := c.Budget.WorkingSetBytes()
		if working <= 0 {
			return "", 0, false
		}
		term, biggest := "weights", c.Budget.WeightBytes
		if c.Budget.KVBytes > biggest {
			term, biggest = "cache", c.Budget.KVBytes
		}
		if c.Budget.ActivationBytes > biggest {
			term, biggest = "activation", c.Budget.ActivationBytes
		}
		return term, float64(biggest) / float64(working), true
	}
	return "", 0, false
}

// budgetJSON is the machine-readable shape. Hand-rolled rather than
// protojson because a budget is not a wire message; the field names
// follow the same snake_case the proto renderers use so one jq idiom
// works across the CLI.
type budgetJSON struct {
	Model        string             `json:"model"`
	Architecture budgetArchJSON     `json:"architecture"`
	Plan         budgetPlanJSON     `json:"plan"`
	Card         budgetCardJSON     `json:"card"`
	Candidates   []budgetRowJSON    `json:"candidates"`
	Skipped      []budgetSkipJSON   `json:"skipped"`
	FewestCards  int32              `json:"fewest_cards_that_fit"`
	LargestTerm  *budgetLargestJSON `json:"largest_term,omitempty"`
}

type budgetArchJSON struct {
	Params                int64 `json:"params"`
	Layers                int32 `json:"layers"`
	KVHeads               int32 `json:"kv_heads"`
	HeadDim               int32 `json:"head_dim"`
	HiddenSize            int32 `json:"hidden_size"`
	MaxPositionEmbeddings int32 `json:"max_position_embeddings"`
}

type budgetPlanJSON struct {
	Weights     string `json:"weights"`
	KVCache     string `json:"kv_cache"`
	MaxModelLen int32  `json:"max_model_len"`
	MaxBatch    int32  `json:"max_batch"`
	// Both omitted unless --expert-parallel was asked for, because
	// without it a row's card count is the tensor width and stating it
	// twice invites a reader to believe the two could differ.
	TensorParallelSize int32 `json:"tensor_parallel_size,omitempty"`
	ExpertParallel     bool  `json:"expert_parallel,omitempty"`
}

// budgetPlanDoc renders the plan half of both JSON documents, so the two
// cannot describe the same flags differently.
func budgetPlanDoc(o budgetOpts) budgetPlanJSON {
	doc := budgetPlanJSON{
		Weights:     string(o.plan.Weights),
		KVCache:     string(o.plan.KVCache),
		MaxModelLen: o.plan.MaxModelLen,
		MaxBatch:    o.plan.MaxBatch,
	}
	if o.expertParallel {
		doc.TensorParallelSize = o.plan.TensorWidth()
		doc.ExpertParallel = true
	}
	return doc
}

type budgetCardJSON struct {
	VRAMBytes   int64   `json:"vram_bytes"`
	Utilization float64 `json:"utilization"`
	UsableBytes int64   `json:"usable_bytes"`
}

// budgetRowJSON quotes bytes rather than the table's rounded gigabytes.
// A caller piping this into a placement decision wants the figure the
// arithmetic produced, not the one that fit the column.
type budgetRowJSON struct {
	Cards           int32  `json:"cards"`
	WeightBytes     int64  `json:"weight_bytes"`
	KVBytes         int64  `json:"kv_bytes"`
	ActivationBytes int64  `json:"activation_bytes"`
	OverheadBytes   int64  `json:"overhead_bytes"`
	TotalBytes      int64  `json:"total_bytes"`
	Verdict         string `json:"verdict"`
}

type budgetSkipJSON struct {
	Cards  int32  `json:"cards"`
	Reason string `json:"reason"`
}

type budgetLargestJSON struct {
	Term                 string  `json:"term"`
	FractionOfWorkingSet float64 `json:"fraction_of_working_set"`
}

func writeBudgetJSON(w io.Writer, spec string, a *provisionerv1.ModelArchitecture, o budgetOpts, candidates []vrambudget.Candidate, fewest int32) error {
	doc := budgetJSON{
		Model: spec,
		Architecture: budgetArchJSON{
			Params:                a.GetParams(),
			Layers:                a.GetLayers(),
			KVHeads:               a.GetKvHeads(),
			HeadDim:               a.GetHeadDim(),
			HiddenSize:            a.GetHiddenSize(),
			MaxPositionEmbeddings: a.GetMaxPositionEmbeddings(),
		},
		Plan: budgetPlanDoc(o),
		Card: budgetCardJSON{
			VRAMBytes:   o.cardBytes,
			Utilization: o.utilization,
			UsableBytes: vrambudget.UsableBytes(o.cardBytes, o.utilization),
		},
		Candidates:  []budgetRowJSON{},
		Skipped:     []budgetSkipJSON{},
		FewestCards: fewest,
	}
	for _, c := range candidates {
		if c.SkipReason != "" {
			doc.Skipped = append(doc.Skipped, budgetSkipJSON{Cards: c.Cards, Reason: c.SkipReason})
			continue
		}
		doc.Candidates = append(doc.Candidates, budgetRowJSON{
			Cards:           c.Cards,
			WeightBytes:     c.Budget.WeightBytes,
			KVBytes:         c.Budget.KVBytes,
			ActivationBytes: c.Budget.ActivationBytes,
			OverheadBytes:   c.Budget.OverheadBytes,
			TotalBytes:      c.Budget.TotalBytes(),
			Verdict:         c.Verdict.String(),
		})
	}
	if term, frac, ok := largestTerm(candidates); ok {
		doc.LargestTerm = &budgetLargestJSON{Term: term, FractionOfWorkingSet: frac}
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

// formatParamsShort renders a parameter count the way a model's name
// does. formatParams is the precise form describe uses; a budget header
// is quoting the name back and wants the name's own rounding.
func formatParamsShort(n int64) string {
	return fmt.Sprintf("%.1fB", float64(n)/1e9)
}

// sessionsJSON is the machine-readable form of the sessions table.
//
// One row per context length with a count per card width, rather than a
// flat list of triples, because the figure this feeds is a curve per card
// width and a flat list would have to be re-grouped to draw it.
type sessionsJSON struct {
	Model       string            `json:"model"`
	Plan        budgetPlanJSON    `json:"plan"`
	Card        budgetCardJSON    `json:"card"`
	CardCounts  []int32           `json:"card_counts"`
	Rows        []sessionsRowJSON `json:"rows"`
	LatentCache bool              `json:"latent_cache"`
}

// sessionsRowJSON is one context length across the card ladder. Sessions
// is index-aligned with CardCounts; zero means no session fits.
type sessionsRowJSON struct {
	ContextLen           int32   `json:"context_len"`
	Sessions             []int64 `json:"sessions"`
	CacheBytesPerSession int64   `json:"cache_bytes_per_session"`
}

func writeSessionsJSON(w io.Writer, spec string, a *provisionerv1.ModelArchitecture, o budgetOpts) (bool, error) {
	cardCounts, err := sessionCardLadder(a, o)
	if err != nil {
		return false, err
	}
	// The sessions document states no context length or batch. The ladder
	// is the context axis and the batch is what the table solves for.
	planDoc := budgetPlanDoc(o)
	planDoc.MaxModelLen, planDoc.MaxBatch = 0, 0

	out := sessionsJSON{
		Model: spec,
		Plan:  planDoc,
		Card: budgetCardJSON{
			VRAMBytes:   o.cardBytes,
			Utilization: o.utilization,
			UsableBytes: vrambudget.UsableBytes(o.cardBytes, o.utilization),
		},
		CardCounts:  cardCounts,
		LatentCache: vrambudget.LatentCache(a),
	}

	any := false
	widest := cardCounts[len(cardCounts)-1]
	for _, ctx := range o.sessionsAt {
		row := sessionsRowJSON{ContextLen: ctx}
		for _, cards := range cardCounts {
			plan := planAtCards(o, cards)
			plan.MaxModelLen = ctx
			n, err := vrambudget.MaxSessions(a, plan, o.cardBytes, o.utilization)
			if err != nil {
				return false, err
			}
			if n > 0 {
				any = true
			}
			row.Sessions = append(row.Sessions, n)
		}
		row.CacheBytesPerSession, _ = vrambudget.SessionCost(a, planAtCards(o, widest), ctx)
		out.Rows = append(out.Rows, row)
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return any, enc.Encode(out)
}
