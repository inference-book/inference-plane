package cmd

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/vrambudget"
)

// contextLadder is the set of context lengths the cache line is quoted
// at. Chosen to bracket the decision rather than to be exhaustive: a
// short window most deployments start at, the native window of a current
// mid-size model, and the long window users increasingly expect. The
// point of showing three is that the term is linear, so a reader can
// interpolate anything in between and see immediately that the cache
// overtakes the weights somewhere along the row.
var contextLadder = []int32{8192, 32768, 131072}

var modelDescribeCmd = &cobra.Command{
	Use:   "describe <model>",
	Short: "Report a model's shape and what it costs in VRAM",
	Long: `Report the part of a model fixed at training time, and what that
costs on a card.

Reads the model's published config; rents nothing and writes nothing. The
weights row is the one term that is exact. The cache row is the one that
decides most deployments, because it grows with context and concurrency
while the weights do not.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		// Through the client, not the store directly. Reading a gated
		// model's config needs HF_TOKEN, which lives wherever the daemon
		// lives, so a laptop resolving it in-process would report a
		// gated model as unreadable while the daemon reads it fine.
		client, err := buildCapacityClient()
		if err != nil {
			return err
		}
		resp, err := client.DescribeModel(cmd.Context(), &provisionerv1.DescribeModelRequest{ModelSpec: args[0]})
		if err != nil {
			return err
		}
		return renderModelArchitecture(cmd.OutOrStdout(), args[0], resp.GetArchitecture(), resp.GetCheckpoint())
	},
}

// renderModelArchitecture prints the shape and the two derived rows that
// make it actionable.
func renderModelArchitecture(w interface{ Write([]byte) (int, error) }, spec string, a *provisionerv1.ModelArchitecture, cp *provisionerv1.ModelCheckpoint) error {
	if err := vrambudget.ValidateArch(a); err != nil {
		return err
	}

	fmt.Fprintf(w, "%s\n\n", spec)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "  parameters\t%s\n", formatParams(a.GetParams()))
	fmt.Fprintf(tw, "  layers\t%d\n", a.GetLayers())
	// How the model caches, rather than the per-head figures, when the
	// per-head figures describe nothing it stores. A latent-cache model
	// has no key and value per head to report, and printing a head count
	// beside a cache that ignores it invites the arithmetic the cache
	// term used to do.
	if vrambudget.LatentCache(a) {
		fmt.Fprintf(tw, "  kv latent\t%d + %d per token per layer\n", a.GetKvLoraRank(), a.GetQkRopeHeadDim())
	} else {
		fmt.Fprintf(tw, "  kv heads\t%d\n", a.GetKvHeads())
		fmt.Fprintf(tw, "  head dim\t%d\n", a.GetHeadDim())
	}
	if n := vrambudget.CachingLayers(a); n < a.GetLayers() {
		fmt.Fprintf(tw, "  caching layers\t%d of %d (the rest are linear attention)\n", n, a.GetLayers())
	}
	fmt.Fprintf(tw, "  hidden size\t%d\n", a.GetHiddenSize())
	if n := a.GetMaxPositionEmbeddings(); n > 0 {
		fmt.Fprintf(tw, "  context window\t%d tokens\n", n)
	}

	// The expert block, present only for a model that states experts.
	// Zero is a dense model rather than an unread config, so a dense
	// model gets no block at all and prints what it printed before these
	// fields existed.
	//
	// Each fact is printed only where the config states it, for the same
	// reason. A model that publishes a routed count and no activation
	// count gets the count alone, rather than "0 active per token" for a
	// model that certainly activates some.
	if experts := a.GetNumExperts(); experts > 0 {
		facts := []string{fmt.Sprintf("%d routed", experts)}
		if n := a.GetNumExpertsPerTok(); n > 0 {
			facts = append(facts, fmt.Sprintf("%d active per token", n))
		}
		if n := a.GetSharedExperts(); n > 0 {
			facts = append(facts, fmt.Sprintf("%d shared", n))
		}
		fmt.Fprintf(tw, "\n  experts\t%s\n", strings.Join(facts, ", "))
		if n := a.GetMoeIntermediateSize(); n > 0 {
			fmt.Fprintf(tw, "  expert width\t%d\n", n)
		}
		// Quoted against the layer count, because three dense layers
		// means something different in a 78-layer model than in a
		// 12-layer one.
		if n := a.GetDenseLayers(); n > 0 {
			fmt.Fprintf(tw, "  dense layers\t%d of %d\n", n, a.GetLayers())
		}
		// What the model reads to decode one token, against what it
		// holds. The gap is why a sparse model is billed like its total
		// and runs like its active share, and it is the one line here
		// that a dense model has no version of.
		if active := vrambudget.ActiveParams(a); active > 0 {
			fmt.Fprintf(tw, "  active per step\t%s of %s (%.0fx smaller)\n",
				formatParams(active), formatParams(a.GetParams()),
				float64(a.GetParams())/float64(active))
		}
	}

	// The weight ladder, for a checkpoint that still has a choice to make.
	// Each step down roughly halves the term, and seeing the three side by
	// side is the whole quantization decision.
	//
	// Suppressed for a checkpoint that already made it. The parameter
	// count is the hub's element accounting, so on a packed checkpoint a
	// ladder built from it prices a second quantization on top of the one
	// already applied, and every figure in the row is wrong by whatever
	// the packing did (#382).
	if q := a.GetQuantization(); q != "" {
		fmt.Fprintf(tw, "\n  weights\tpublished at %s; no ladder, this checkpoint already chose\n", q)
		if cp.GetFileCount() > 0 {
			// The repo's own files, which is what the advice used to send
			// an operator off to work out by hand.
			fmt.Fprintf(tw, "  \t%s on the hub is the real weight size\n", formatGB(cp.GetDownloadBytes()))
		} else {
			fmt.Fprintf(tw, "  \tsize it from the repo's own files, or from the unquantized model\n")
		}
	} else {
		var weights []string
		for _, p := range []struct {
			label string
			prec  vrambudget.Precision
		}{
			{"fp16", vrambudget.PrecisionFP16},
			{"fp8", vrambudget.PrecisionFP8},
			{"4-bit", vrambudget.PrecisionAWQ},
		} {
			b, err := vrambudget.Compute(a, vrambudget.Plan{Weights: p.prec, MaxModelLen: 1, MaxBatch: 1})
			if err != nil {
				return err
			}
			weights = append(weights, fmt.Sprintf("%s %s", formatGB(b.WeightBytes), p.label))
		}
		fmt.Fprintf(tw, "\n  weights\t%s\n", strings.Join(weights, "   "))
	}

	// What has to cross a network before any of the above matters. Absent
	// when the hub's file listing could not be read, and printed as nothing
	// rather than as zero, because a zero here reads as "no download".
	if cp.GetFileCount() > 0 {
		fmt.Fprintf(tw, "  download\t%s across %d safetensors, largest %s\n",
			formatGB(cp.GetDownloadBytes()), cp.GetFileCount(), formatGB(cp.GetLargestFileBytes()))
	}

	// The cache, per token and then at three context lengths. One
	// sequence throughout, because batch multiplies the same number and
	// showing both axes at once obscures which one moved.
	perToken, err := vrambudget.Compute(a, vrambudget.Plan{Weights: vrambudget.PrecisionFP16, MaxModelLen: 1, MaxBatch: 1})
	if err != nil {
		return err
	}
	fmt.Fprintf(tw, "  kv cache\t%s per token at fp16, %s at fp8\n",
		formatKiB(perToken.KVBytesPerToken), formatKiB(perToken.KVBytesPerToken/2))

	var ctxCosts []string
	for _, n := range contextLadder {
		b, err := vrambudget.Compute(a, vrambudget.Plan{Weights: vrambudget.PrecisionFP16, MaxModelLen: n, MaxBatch: 1})
		if err != nil {
			return err
		}
		ctxCosts = append(ctxCosts, fmt.Sprintf("%s %s", formatTokens(n), formatGB(b.KVBytes)))
	}
	fmt.Fprintf(tw, "  \t%s   (one sequence)\n", strings.Join(ctxCosts, "   "))
	return tw.Flush()
}

// formatParams renders a parameter count the way model names do, so the
// number can be checked against the name at a glance.
func formatParams(n int64) string {
	return fmt.Sprintf("%.2f B", float64(n)/1e9)
}

// formatGB renders bytes as decimal gigabytes, the unit vendors quote
// VRAM in. Engines report gibibytes, and conflating the two is what put
// a three-gigabyte error in a worked example.
func formatGB(b int64) string {
	return fmt.Sprintf("%.1f GB", float64(b)/float64(vrambudget.GB))
}

// formatKiB renders a per-token cost in kibibytes, which is the scale it
// lands at for every model worth deploying.
func formatKiB(b int64) string {
	return fmt.Sprintf("%.1f KiB", float64(b)/1024)
}

// formatTokens renders a context length as the "32k" operators say
// rather than the 32768 they mean.
func formatTokens(n int32) string {
	switch {
	case n%(1024*1024) == 0:
		return fmt.Sprintf("%dM:", n/(1024*1024))
	case n%1024 == 0:
		return fmt.Sprintf("%dk:", n/1024)
	}
	return fmt.Sprintf("%d:", n)
}
