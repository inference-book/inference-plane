package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

// modelSuggestion is one entry in the curated list `iplane deployment
// models` prints. Static for v0.1 (the chapter focuses on the deploy
// loop, not model curation); a live HuggingFace-backed catalog is
// tracked as a separate followup.
type modelSuggestion struct {
	ID      string // HF model id; pass as --model
	Size    string // shorthand
	VRAM    string // approximate, headroom for KV cache included
	Class   string // smallest iplane GPU class that fits
	Caveats string // gated, requires HF token, etc -- empty if open-weights
}

// curatedModels is the v0.1 starter list. All open-weights so no HF
// gated-access dance. Spans the realistic small-class GPU range so
// operators see what fits a 24 GB pod. Live discovery (filtered HF
// search) lives in a separate ticket.
var curatedModels = []modelSuggestion{
	{ID: "Qwen/Qwen2.5-1.5B-Instruct", Size: "1.5B", VRAM: "~3 GB", Class: "small"},
	{ID: "Qwen/Qwen2.5-3B-Instruct", Size: "3B", VRAM: "~6 GB", Class: "small"},
	{ID: "Qwen/Qwen2.5-7B-Instruct", Size: "7B", VRAM: "~14 GB", Class: "small"},
	{ID: "microsoft/Phi-3-mini-4k-instruct", Size: "3.8B", VRAM: "~7.6 GB", Class: "small"},
	{ID: "meta-llama/Llama-3.2-1B-Instruct", Size: "1B", VRAM: "~2.5 GB", Class: "small", Caveats: "gated -- requires HF token + license acceptance"},
}

// HF Hub search URL the verb points operators to for browsing beyond
// the curated list. The `library=vllm` filter restricts to models
// vLLM's loader knows how to serve; `sort=trending` puts the
// popular-this-week models first.
const hfHubSearchURL = "https://huggingface.co/models?library=vllm&sort=trending"

var deploymentModelsCmd = &cobra.Command{
	Use:   "models",
	Short: "Show a starter list of model ids that work with `deployment deploy --model`",
	Long: `Prints a small curated list of well-known vLLM-compatible model ids
plus a link to HuggingFace's vLLM-filtered model search.

The --model flag on 'iplane deployment deploy' accepts any HuggingFace
model id; vLLM downloads the weights on first run. This list is just a
starting point -- copy an id from here or browse the HF page for more.

A live HF-backed catalog (filtered by your instance's GPU class) is
tracked as a separate followup; v0.1 ships a static list because the
chapter narrative focuses on the deploy loop, not model curation.`,
	RunE: runDeploymentModels,
}

func runDeploymentModels(cmd *cobra.Command, args []string) error {
	out := cmd.OutOrStdout()
	fmt.Fprintln(out, "Starter list of vLLM-compatible models (--model accepts any HF model id):")
	fmt.Fprintln(out)
	for _, m := range curatedModels {
		fmt.Fprintf(out, "  %-40s %s  (%s, fits %s class)", m.ID, m.Size, m.VRAM, m.Class)
		if m.Caveats != "" {
			fmt.Fprintf(out, "  [%s]", m.Caveats)
		}
		fmt.Fprintln(out)
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Browse more (vLLM-compatible, trending this week):")
	fmt.Fprintln(out, "  "+hfHubSearchURL)
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Example:")
	fmt.Fprintln(out, "  iplane deployment deploy my-llama --instance my-pod \\")
	fmt.Fprintln(out, "    --image vllm/vllm-openai:v0.7.0 \\")
	fmt.Fprintln(out, "    --model Qwen/Qwen2.5-1.5B-Instruct")
	return nil
}

func init() {
	deploymentCmd.AddCommand(deploymentModelsCmd)
}
