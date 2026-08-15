package cmd

import (
	"context"
	"encoding/json"
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
		provider := capacityProvider
		if provider == "" {
			provider = defaultProvider(provisioners.ProviderRunPod)
		}
		if err := ensureProviderAPIKey(provider); err != nil {
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
		spec := &provisionerv1.Spec{Id: "capacity-query", Provider: provider, Requirements: reqs}
		if err := provisioners.ValidateAndExpandRequirements(spec); err != nil {
			return err
		}

		svc, err := buildReadOnlyService()
		if err != nil {
			return err
		}

		ctx, cancel := context.WithTimeout(cmd.Context(), time.Duration(capacityTimeoutSc)*time.Second)
		defer cancel()

		candidates, err := svc.ListCandidates(ctx, provider, spec.GetRequirements())
		if err != nil {
			return candidateQueryError(provider, err)
		}
		if capacityLimit > 0 && len(candidates) > capacityLimit {
			candidates = candidates[:capacityLimit]
		}
		return renderCandidates(cmd.OutOrStdout(), provider, candidates, capacityOutput)
	},
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

func renderCandidates(w io.Writer, provider string, candidates []provisioners.Candidate, format string) error {
	if format == "json" {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(candidates)
	}

	if len(candidates) == 0 {
		fmt.Fprintf(w, "no candidates on %s for these requirements.\n", provider)
		fmt.Fprintln(w, "the provider was asked and had nothing to offer, which is not the same as the requirements being unsatisfiable elsewhere.")
		return nil
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "HOST\tOFFER\tSKU\tREGION\tGPUS\tVRAM\tARCH\t$/HR\tFABRIC\tNOTES")
	for _, c := range candidates {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%dGB\t%s\t%.2f\t%s\t%s\n",
			dashIfEmpty(c.HostID), dashIfEmpty(c.OfferID), c.SKU,
			dashIfEmpty(c.Region), c.GPUCount, c.VRAMGbPerGPU,
			dashIfEmpty(c.Architecture), c.PriceUSDPerHour, candidateFabricLabel(c),
			renderAttrs(c.Attrs))
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	fmt.Fprintf(w, "\n%d candidate(s) on %s, cheapest first. Nothing was rented.\n", len(candidates), provider)
	return nil
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
	f.StringVar(&capacityProvider, "provider", "", `provider to ask (default $IPLANE_PROVIDER, else runpod)`)
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
