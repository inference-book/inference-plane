package cmd

import (
	"encoding/json"
	"fmt"
	"io"
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

	o.plan = vrambudget.Plan{Weights: weights, KVCache: cache, MaxModelLen: budgetMaxModelLen, MaxBatch: budgetMaxBatch}
	o.cardBytes = int64(budgetVRAMGB * float64(vrambudget.GB))
	o.cardGB = budgetVRAMGB
	o.utilization = budgetUtilization
	o.maxCards = budgetMaxCards
	o.tp = budgetTP
	o.format = budgetOutput
	return o, nil
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
// modes cannot disagree about a row they share.
func budgetCandidates(a *provisionerv1.ModelArchitecture, o budgetOpts) ([]vrambudget.Candidate, error) {
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
	fmt.Fprintf(w, "plan   weights %s, cache %s, %d tokens x %d sequences\n",
		o.plan.Weights, o.plan.KVCache, o.plan.MaxModelLen, o.plan.MaxBatch)
	fmt.Fprintf(w, "card   %g GB at %.2f utilization = %s usable\n\n",
		o.cardGB, o.utilization, formatGB(vrambudget.UsableBytes(o.cardBytes, o.utilization)))

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
		Plan: budgetPlanJSON{
			Weights:     string(o.plan.Weights),
			KVCache:     string(o.plan.KVCache),
			MaxModelLen: o.plan.MaxModelLen,
			MaxBatch:    o.plan.MaxBatch,
		},
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
