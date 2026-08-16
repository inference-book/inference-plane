package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/gen/go/provisioner/v1/provisionerv1connect"
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
	capacityReclaim   string
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
		rp, err := parseReclaimPolicy(capacityReclaim)
		if err != nil {
			return err
		}
		reqs.ReclaimPolicy = rp

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

		client, err := buildCapacityClient()
		if err != nil {
			return err
		}

		ctx, cancel := context.WithTimeout(cmd.Context(), time.Duration(capacityTimeoutSc)*time.Second)
		defer cancel()

		resp, err := client.ListCandidates(ctx, &provisionerv1.ListCandidatesRequest{
			Providers:    providers,
			Requirements: spec.GetRequirements(),
		})
		if err != nil {
			return err
		}
		answers := resp.GetAnswers()

		// A single named provider that could not answer is an error, because
		// the operator asked one question and got none. Across several it is
		// a reported outcome, because the other vendors still answered and a
		// search that dies on its weakest participant is not a search.
		if len(answers) == 1 {
			switch answers[0].GetOutcome() {
			case provisionerv1.AnswerOutcome_ANSWER_OUTCOME_CANNOT_ANSWER:
				return cannotAnswerError(answers[0].GetProvider())
			case provisionerv1.AnswerOutcome_ANSWER_OUTCOME_FAILED:
				return fmt.Errorf("%s: %s", answers[0].GetProvider(), answers[0].GetError())
			}
		}

		candidates := resp.GetCandidates()
		if capacityLimit > 0 && len(candidates) > capacityLimit {
			candidates = candidates[:capacityLimit]
		}
		return renderCandidates(cmd.OutOrStdout(), answers, candidates, resp.GetComparability(), capacityOutput)
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

// buildCapacityClient resolves who answers a capacity question.
//
// Remote when --service-url is set, and this is the whole of #304. The first
// version of these verbs always built a local service, so pointing them at a
// control plane produced a confident answer from the wrong host: provider
// credentials live in the daemon's environment, so a CLI without keys reported
// "no capacity" for vendors the daemon could see perfectly well.
//
// The reasoning that got this wrong is worth keeping. The test applied was
// "does it mutate state", which is why the write verbs got RPCs and the read
// verbs did not. The test that matters is where the INPUTS live. A read that
// depends on privileged inputs belongs where those inputs are, exactly as much
// as a write does.
func buildCapacityClient() (provisionerClient, error) {
	if instanceServiceURL != "" {
		return &connectProvisionerClient{
			c: provisionerv1connect.NewProvisionerServiceClient(http.DefaultClient, instanceServiceURL),
		}, nil
	}
	return buildReadOnlyService()
}

// buildReadOnlyService opens the state store WITHOUT taking the lifetime lock
// and wires the same provider set the daemon uses.
//
// The missing lock is the point rather than an oversight. `iplane model pin`
// takes it because it writes; this path only reads a provider's API and never
// touches state, so taking the lock would make the command fail exactly when a
// daemon is up, which is when an operator is most likely to be asking whether
// there is capacity to scale onto.
//
// Only the local branch of buildCapacityClient uses this. In remote mode there
// is no store to open, because the daemon holds it.
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

// cannotAnswerError explains a provider that has no way to answer.
//
// Says it is a property of the provider's API rather than a gap in iplane,
// because otherwise it reads as a missing feature and somebody files a ticket
// for it.
func cannotAnswerError(provider string) error {
	return fmt.Errorf("%s cannot list candidates without renting one.\n"+
		"  this is a property of the provider's API, not a gap in iplane: a marketplace\n"+
		"  publishes live offers to choose among, while a fixed-catalog provider may only\n"+
		"  publish a price list. `iplane instance create --dry-run` still shows what the\n"+
		"  static catalog would resolve to on %s", provider, provider)
}

func renderCandidates(w io.Writer, answers []*provisioners.ProviderAnswer, candidates []*provisioners.Candidate, comp *provisioners.Comparability, format string) error {
	if format == "json" {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{
			"candidates":    candidates,
			"providers":     summarizeAnswers(answers),
			"comparability": comp,
		})
	}

	if len(candidates) > 0 {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "PROVIDER\tHOST\tOFFER\tSKU\tREGION\tGPUS\tVRAM\tARCH\t$/HR\tTIER\tFABRIC\tNOTES")
		for _, c := range candidates {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%dGB\t%s\t%.2f\t%s\t%s\t%s\n",
				c.GetProvider(),
				dashIfEmpty(c.GetHostId()), dashIfEmpty(c.GetOfferId()), c.GetSku(),
				dashIfEmpty(c.GetRegion()), c.GetGpuCount(), c.GetVramGbPerGpu(),
				dashIfEmpty(c.GetArchitecture()), c.GetPriceUsdPerHour(), reclaimLabel(c.GetReclaimable()),
				candidateFabricLabel(c), renderAttrs(c.GetAttrs()))
		}
		if err := tw.Flush(); err != nil {
			return err
		}
		fmt.Fprintln(w)
	}

	renderProviderOutcomes(w, answers, len(candidates))
	renderComparability(w, comp)
	return nil
}

// renderProviderOutcomes reports what each vendor actually did, because the
// merged table cannot show it. A provider that could not answer contributes no
// rows and so does one with no capacity, and those are opposite findings.
func renderProviderOutcomes(w io.Writer, answers []*provisioners.ProviderAnswer, total int) {
	if total > 0 {
		fmt.Fprintf(w, "%d candidate(s) across %d provider(s), cheapest first. Nothing was rented.\n",
			total, len(answers))
	} else {
		fmt.Fprintf(w, "no candidates from %d provider(s) for these requirements. Nothing was rented.\n",
			len(answers))
	}
	for _, a := range answers {
		switch a.GetOutcome() {
		case provisionerv1.AnswerOutcome_ANSWER_OUTCOME_CANNOT_ANSWER:
			fmt.Fprintf(w, "  %-12s cannot answer without renting one\n", a.GetProvider())
		case provisionerv1.AnswerOutcome_ANSWER_OUTCOME_FAILED:
			fmt.Fprintf(w, "  %-12s did not answer: %s\n", a.GetProvider(), a.GetError())
		case provisionerv1.AnswerOutcome_ANSWER_OUTCOME_NO_CAPACITY:
			fmt.Fprintf(w, "  %-12s answered, no capacity matching these requirements\n", a.GetProvider())
		default:
			fmt.Fprintf(w, "  %-12s %d candidate(s)\n", a.GetProvider(), len(a.GetCandidates()))
		}
	}
}

// renderComparability prints which columns mean the same thing on every row.
//
// Without it the table invites a comparison it cannot support: a blank cell
// reads as neutral, so the vendor that publishes least looks no worse than the
// one that publishes most. Naming the gaps is cheaper than pretending they are
// not there and far cheaper than silently ranking on them.
func renderComparability(w io.Writer, c *provisioners.Comparability) {
	if len(c.GetCompared()) == 0 && len(c.GetGaps()) == 0 {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Ranked on price only.")
	if len(c.GetCompared()) > 0 {
		fmt.Fprintf(w, "  comparable across all answering providers: %s\n", strings.Join(c.GetCompared(), ", "))
	}
	for _, g := range c.GetGaps() {
		fmt.Fprintf(w, "  %s: reported by %s, not by %s\n",
			g.GetFact(), strings.Join(g.GetReportedBy(), "/"), strings.Join(g.GetMissingFrom(), "/"))
	}
	if len(c.GetGaps()) > 0 {
		fmt.Fprintln(w, "  a blank in those columns is unmeasured, not zero, and was not ranked on.")
	}
}

// summarizeAnswers renders the per-provider outcome for --output json, where
// the same three-way distinction has to survive without the prose.
func summarizeAnswers(answers []*provisioners.ProviderAnswer) []map[string]any {
	out := make([]map[string]any, 0, len(answers))
	for _, a := range answers {
		row := map[string]any{"provider": a.GetProvider(), "candidates": len(a.GetCandidates())}
		switch a.GetOutcome() {
		case provisionerv1.AnswerOutcome_ANSWER_OUTCOME_CANNOT_ANSWER:
			row["outcome"] = "cannot_answer"
		case provisionerv1.AnswerOutcome_ANSWER_OUTCOME_FAILED:
			row["outcome"] = "error"
			row["error"] = a.GetError()
		case provisionerv1.AnswerOutcome_ANSWER_OUTCOME_NO_CAPACITY:
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
func candidateFabricLabel(c *provisioners.Candidate) string {
	switch c.GetFabricSource() {
	case provisionerv1.FabricSource_FABRIC_SOURCE_UNKNOWN:
		return "unknown (host reports no reading)"
	case provisionerv1.FabricSource_FABRIC_SOURCE_MEASURED:
		return fmt.Sprintf("%s (measured, %d Gb)", fabricScopeLabel(c.GetFabricScope()), c.GetFabricGbps())
	default:
		if c.GetFabricGbps() == 0 {
			return fmt.Sprintf("%s (declared)", fabricScopeLabel(c.GetFabricScope()))
		}
		return fmt.Sprintf("%s (declared, %d Gb)", fabricScopeLabel(c.GetFabricScope()), c.GetFabricGbps())
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

// reclaimLabel names the tier a price belongs to.
//
// It is a column rather than a footnote because it changes what the number
// next to it means. An hourly rate on capacity that can be taken back is not
// comparable with one that cannot, and a list sorted on price alone puts the
// two side by side as though they were the same kind of number.
func reclaimLabel(reclaimable bool) string {
	if reclaimable {
		return "reclaimable"
	}
	return "on-demand"
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
	// capacity is a top-level verb, so it does not inherit the instance
	// group's persistent --service-url and needs its own bound to the same
	// variable. Defaulting from IPLANE_SERVICE_URL matches every other verb;
	// without it, an operator with the env var exported would have this one
	// command quietly answer from a different place than the rest (#304).
	f.StringVar(&instanceServiceURL, "service-url", os.Getenv("IPLANE_SERVICE_URL"),
		`forward to a running iplane serve rather than answering in-process (default $IPLANE_SERVICE_URL)`)
	f.StringVar(&capacityProvider, "provider", "", `provider(s) to ask, comma-separated (default $IPLANE_PROVIDER, else runpod)`)
	f.BoolVar(&capacityAll, "all", false, `ask every configured provider, including ones that cannot answer`)
	f.StringVar(&capacityClass, "class", "", `gpu class shorthand: small | medium | large | xlarge`)
	f.StringVar(&capacitySKU, "sku", "", `exact provider sku id (bypasses the constraint resolver)`)
	f.Int32Var(&capacityGPUCount, "gpu-count", 0, `number of GPUs on the instance (default 1)`)
	f.Int32Var(&capacityMinVRAM, "min-vram-gb", 0, `minimum VRAM per GPU, in GB`)
	f.Int32Var(&capacityMinRAM, "min-ram-gb", 0, `minimum system RAM, in GB (per instance, not per GPU)`)
	f.Int32Var(&capacityMinDisk, "min-disk-gb", 0, `minimum container disk, in GB`)
	f.StringVar(&capacityFabric, "fabric", "", fabricFlagUsage)
	f.StringVar(&capacityReclaim, "reclaim", "", reclaimFlagUsage)
	f.Int32Var(&capacityFabricBW, "min-fabric-gbps", 0, minFabricGbpsUsage)
	f.StringVar(&capacityOutput, "output", "table", `output format: table | json`)
	f.IntVar(&capacityLimit, "limit", 0, `show at most N candidates (0 = all)`)
	f.IntVar(&capacityTimeoutSc, "timeout", 60, `seconds to wait for the provider to answer`)
}
