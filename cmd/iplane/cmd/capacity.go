package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/stores/file"
)

// `iplane capacity` answers "what would this provider give me, and is it any
// good" without renting anything.
//
// It is a verb rather than a flag on `instance create --dry-run`, for two
// reasons. Dry-run's contract is that it makes zero provider calls and says so
// in its own output, and a live marketplace query would quietly break that.
// And capacity questions get asked far more often than instances get created,
// so making the answer a side effect of a create command is the wrong shape
// for how the question actually comes up.
//
// Read-only by contract, all the way down: it never rents, never writes
// state, and deliberately does not take the state-dir lock, because the moment
// an operator most wants to ask about capacity is while a daemon is running.
var (
	capacityProvider  string
	capacityAll       bool
	capacityClass     string
	capacitySKU       string
	capacityGPUCount  int32
	capacityMinVRAM   int32
	capacityMinRAM    int32
	capacityMinDisk   int32
	capacityFabric    string
	capacityFabricBW  int32
	capacityOutput    string
	capacityLimit     int
	capacityTimeoutSc int
)

var capacityCmd = &cobra.Command{
	Use:   "capacity",
	Short: "List what a provider would rent you, without renting it",
	Long: "Query a provider for the candidates matching a set of requirements " +
		"and print them cheapest first.\n\n" +
		"Read-only and free: no instance is rented, no state is written, and " +
		"the state directory is not locked, so this works while `iplane serve` " +
		"is running.\n\n" +
		"Not every provider can answer. A marketplace knows its live offers; a " +
		"fixed-catalog provider may only know its price list. Providers that " +
		"cannot answer say so rather than returning an empty list, because " +
		"\"nobody looked\" and \"no capacity\" are different answers.",
	RunE: func(cmd *cobra.Command, args []string) error {
		providers, err := capacityProviders()
		if err != nil {
			return err
		}

		reqs := &provisionerv1.ResourceRequirements{
			MinVramGb: capacityMinVRAM,
			MinDiskGb: capacityMinDisk,
			MinRamGb:  capacityMinRAM,
			GpuCount:  capacityGPUCount,
			Class:     capacityClass,
			Sku:       capacitySKU,
		}
		fabricScope, err := parseFabricScope(capacityFabric)
		if err != nil {
			return err
		}
		reqs.FabricScope = fabricScope
		reqs.MinFabricGbps = capacityFabricBW

		// Expand class shorthand into numeric constraints the same way the
		// create path does, so `--class large` here means what it means there.
		// Without this the listing would answer a different question than the
		// one a subsequent create would ask.
		// Provider is not read by the expansion (it only resolves class
		// shorthand into numbers), and with --all there is no single provider
		// to name, so it is deliberately left unset.
		spec := &provisionerv1.Spec{Id: "capacity-query", Requirements: reqs}
		if err := provisioners.ValidateAndExpandRequirements(spec); err != nil {
			return err
		}

		svc, err := buildReadOnlyService()
		if err != nil {
			return err
		}

		ctx, cancel := context.WithTimeout(cmd.Context(), time.Duration(capacityTimeoutSc)*time.Second)
		defer cancel()

		answers := svc.ListCandidatesAcross(ctx, providers, spec.GetRequirements())

		// A single named provider that could not answer is an error, because
		// the operator asked one question and got none. Across several it is
		// a reported outcome, because the other vendors still answered and a
		// search that dies on its weakest participant is not a search.
		if len(answers) == 1 && answers[0].Err != nil {
			return candidateQueryError(answers[0].Provider, answers[0].Err)
		}

		candidates := provisioners.MergeCandidates(answers)
		if capacityLimit > 0 && len(candidates) > capacityLimit {
			candidates = candidates[:capacityLimit]
		}
		return renderCandidates(cmd.OutOrStdout(), answers, candidates, capacityOutput)
	},
}

// capacityProviders resolves --provider / --all into the list to ask.
//
// An empty list means "every configured provider" and is passed through as
// such, because the Service is the thing that knows what is configured and
// duplicating that here would drift.
//
// The API-key precheck only runs for explicitly named providers. Asking for
// everything should not fail because one vendor's key is absent: that provider
// simply reports its own error alongside the others, which is the outcome the
// cross-provider view exists to show.
func capacityProviders() ([]string, error) {
	if capacityAll {
		if capacityProvider != "" {
			return nil, errors.New("--all and --provider are mutually exclusive")
		}
		return nil, nil
	}

	spec := capacityProvider
	if spec == "" {
		spec = defaultProvider(provisioners.ProviderRunPod)
	}
	var out []string
	for _, name := range strings.Split(spec, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if err := ensureProviderAPIKey(name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	if len(out) == 0 {
		return nil, errors.New("--provider was empty")
	}
	return out, nil
}

// buildReadOnlyService opens the state store WITHOUT taking the lifetime lock
// and wires the same provider set the daemon uses.
//
// The missing lock is the point rather than an oversight. `iplane model pin`
// takes it because it writes; this path only reads a provider's API and never
// touches state, so taking the lock would make the command fail exactly when a
// daemon is up, which is when an operator is most likely to be asking whether
// there is capacity to scale onto.
func buildReadOnlyService() (*provisioners.Service, error) {
	dir, err := resolveDeploymentStateDir()
	if err != nil {
		return nil, err
	}
	store, err := file.Open(dir, deploymentOperatorID)
	if err != nil {
		return nil, fmt.Errorf("open state store: %w", err)
	}
	return buildLocalService(store, deploymentOperatorID)
}

// candidateQueryError turns the Service's status codes into something an
// operator can act on.
//
// The Service is right to speak in codes, since it is also a gRPC surface. But
// "rpc error: code = Unimplemented" tells an operator nothing about what to do
// next, and this is precisely the moment they learn a provider cannot answer.
// Whether a provider can is a property of its API rather than a gap in iplane,
// and the message should say so, otherwise it reads as a missing feature and
// somebody files a ticket for it.
func candidateQueryError(provider string, err error) error {
	switch status.Code(err) {
	case codes.Unimplemented:
		return fmt.Errorf("%s cannot list candidates without renting one.\n"+
			"  this is a property of the provider's API, not a gap in iplane: a marketplace\n"+
			"  publishes live offers to choose among, while a fixed-catalog provider may only\n"+
			"  publish a price list. `iplane instance create --dry-run` still shows what the\n"+
			"  static catalog would resolve to on %s", provider, provider)
	case codes.NotFound:
		return fmt.Errorf("provider %q is not configured; set %s", provider, providerAPIKeyEnv(provider))
	}
	return err
}

func renderCandidates(w io.Writer, answers []provisioners.ProviderAnswer, candidates []provisioners.Candidate, format string) error {
	if format == "json" {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{
			"candidates":    candidates,
			"providers":     summarizeAnswers(answers),
			"comparability": provisioners.AnalyzeComparability(answers),
		})
	}

	if len(candidates) > 0 {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "PROVIDER\tHOST\tOFFER\tSKU\tREGION\tGPUS\tVRAM\tARCH\t$/HR\tFABRIC\tNOTES")
		for _, c := range candidates {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%dGB\t%s\t%.2f\t%s\t%s\n",
				c.Provider,
				dashIfEmpty(c.HostID), dashIfEmpty(c.OfferID), c.SKU,
				dashIfEmpty(c.Region), c.GPUCount, c.VRAMGbPerGPU,
				dashIfEmpty(c.Architecture), c.PriceUSDPerHour, candidateFabricLabel(c),
				renderAttrs(c.Attrs))
		}
		if err := tw.Flush(); err != nil {
			return err
		}
		fmt.Fprintln(w)
	}

	renderProviderOutcomes(w, answers, len(candidates))
	renderComparability(w, provisioners.AnalyzeComparability(answers))
	return nil
}

// renderProviderOutcomes reports what each vendor actually did, because the
// merged table cannot show it. A provider that could not answer contributes no
// rows and so does one with no capacity, and those are opposite findings.
func renderProviderOutcomes(w io.Writer, answers []provisioners.ProviderAnswer, total int) {
	if total > 0 {
		fmt.Fprintf(w, "%d candidate(s) across %d provider(s), cheapest first. Nothing was rented.\n",
			total, len(answers))
	} else {
		fmt.Fprintf(w, "no candidates from %d provider(s) for these requirements. Nothing was rented.\n",
			len(answers))
	}
	for _, a := range answers {
		switch {
		case a.Err != nil && !a.CanAnswer():
			fmt.Fprintf(w, "  %-12s cannot answer without renting one\n", a.Provider)
		case a.Err != nil:
			fmt.Fprintf(w, "  %-12s did not answer: %v\n", a.Provider, a.Err)
		case len(a.Candidates) == 0:
			fmt.Fprintf(w, "  %-12s answered, no capacity matching these requirements\n", a.Provider)
		default:
			fmt.Fprintf(w, "  %-12s %d candidate(s)\n", a.Provider, len(a.Candidates))
		}
	}
}

// renderComparability prints which columns mean the same thing on every row.
//
// Without it the table invites a comparison it cannot support: a blank cell
// reads as neutral, so the vendor that publishes least looks no worse than the
// one that publishes most. Naming the gaps is cheaper than pretending they are
// not there and far cheaper than silently ranking on them.
func renderComparability(w io.Writer, c provisioners.Comparability) {
	if len(c.Compared) == 0 && len(c.Gaps) == 0 {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Ranked on price only.")
	if len(c.Compared) > 0 {
		fmt.Fprintf(w, "  comparable across all answering providers: %s\n", strings.Join(c.Compared, ", "))
	}
	for _, g := range c.Gaps {
		fmt.Fprintf(w, "  %s: reported by %s, not by %s\n",
			g.Fact, strings.Join(g.ReportedBy, "/"), strings.Join(g.MissingFrom, "/"))
	}
	if len(c.Gaps) > 0 {
		fmt.Fprintln(w, "  a blank in those columns is unmeasured, not zero, and was not ranked on.")
	}
}

// summarizeAnswers renders the per-provider outcome for --output json, where
// the same three-way distinction has to survive without the prose.
func summarizeAnswers(answers []provisioners.ProviderAnswer) []map[string]any {
	out := make([]map[string]any, 0, len(answers))
	for _, a := range answers {
		row := map[string]any{"provider": a.Provider, "candidates": len(a.Candidates)}
		switch {
		case a.Err != nil && !a.CanAnswer():
			row["outcome"] = "cannot_answer"
		case a.Err != nil:
			row["outcome"] = "error"
			row["error"] = a.Err.Error()
		case len(a.Candidates) == 0:
			row["outcome"] = "no_capacity"
		default:
			row["outcome"] = "answered"
		}
		out = append(out, row)
	}
	return out
}

// candidateFabricLabel renders the fabric verdict WITH its provenance, never
// as a bare number.
//
// A candidate whose host reports no reading is not a candidate with zero
// bandwidth, and printing "0" would invite exactly the comparison that must
// not be made: a host that publishes nothing must not read as a host that
// published a bad number. This is the display half of the rule the fabric
// package enforces in code.
func candidateFabricLabel(c provisioners.Candidate) string {
	switch c.Fabric.Source {
	case provisionerv1.FabricSource_FABRIC_SOURCE_UNKNOWN:
		return "unknown (host reports no reading)"
	case provisionerv1.FabricSource_FABRIC_SOURCE_MEASURED:
		return fmt.Sprintf("%s (measured, %d Gb)", fabricScopeLabel(c.Fabric.Scope), c.Fabric.Gbps)
	default:
		if c.Fabric.Gbps == 0 {
			return fmt.Sprintf("%s (declared)", fabricScopeLabel(c.Fabric.Scope))
		}
		return fmt.Sprintf("%s (declared, %d Gb)", fabricScopeLabel(c.Fabric.Scope), c.Fabric.Gbps)
	}
}

// renderAttrs prints the provider-reported extras as sorted k=v pairs.
//
// Attrs is where per-provider facts live precisely because they cannot be
// compared across providers, and until now it was reachable only through
// --output json. Keeping it invisible is what lets an untyped bag quietly
// accumulate: RunPod's stock level is the single most useful thing it reports,
// and RunPod has no host, no offer and no region, so without this its rows are
// three dashes and a price. Sorted so the column is stable between runs.
func renderAttrs(attrs map[string]string) string {
	if len(attrs) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+attrs[k])
	}
	return strings.Join(parts, " ")
}

func dashIfEmpty(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func init() {
	rootCmd.AddCommand(capacityCmd)

	f := capacityCmd.Flags()
	f.StringVar(&capacityProvider, "provider", "", `provider(s) to ask, comma-separated (default $IPLANE_PROVIDER, else runpod)`)
	f.BoolVar(&capacityAll, "all", false, `ask every configured provider, including ones that cannot answer`)
	f.StringVar(&capacityClass, "class", "", `gpu class shorthand: small | medium | large | xlarge`)
	f.StringVar(&capacitySKU, "sku", "", `exact provider sku id (bypasses the constraint resolver)`)
	f.Int32Var(&capacityGPUCount, "gpu-count", 0, `number of GPUs on the instance (default 1)`)
	f.Int32Var(&capacityMinVRAM, "min-vram-gb", 0, `minimum VRAM per GPU, in GB`)
	f.Int32Var(&capacityMinRAM, "min-ram-gb", 0, `minimum system RAM, in GB (per instance, not per GPU)`)
	f.Int32Var(&capacityMinDisk, "min-disk-gb", 0, `minimum container disk, in GB`)
	f.StringVar(&capacityFabric, "fabric", "", fabricFlagUsage)
	f.Int32Var(&capacityFabricBW, "min-fabric-gbps", 0, minFabricGbpsUsage)
	f.StringVar(&capacityOutput, "output", "table", `output format: table | json`)
	f.IntVar(&capacityLimit, "limit", 0, `show at most N candidates (0 = all)`)
	f.IntVar(&capacityTimeoutSc, "timeout", 60, `seconds to wait for the provider to answer`)
}
