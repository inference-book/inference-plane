package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// Flags scoped to `iplane instance create`. The constraints-first
// design (see docs/design/0001-provisioner.md "Resource requirements")
// exposes both the class shorthand and the underlying numeric knobs;
// operators reach for class when they want sane defaults and reach
// for the numeric flags when they know exactly what they need.
//
// provider and id are POSITIONAL, not flags. provider is the
// fundamental axis of creation -- where to spawn -- and ids are
// tenant-globally unique so describe / destroy / list look up by id
// alone. Mirrors `docker run <image>`: the most load-bearing argument
// is positional, refinements are flags.
var (
	createRegion    string
	createClass     string
	createSKU       string
	createGPUCount  int32
	createMinVRAM   int32
	createMinRAM    int32
	createMinDisk   int32
	createBaseImage string
	createDryRun    bool
)

var instanceCreateCmd = &cobra.Command{
	Use:   "create <provider> <id>",
	Short: "Create a GPU instance",
	Args:  cobra.ExactArgs(2),
	Long: `Create a new GPU instance on the named provider under the given iplane id.

provider is positional and required. v0.1 supports:

  local    The operator's laptop. Zero cost, no API key.
  runpod   A real RunPod pod. Requires RUNPOD_API_KEY.

id is operator-supplied and tenant-globally unique across providers.
Idempotent: re-running with the same id returns the existing record
without contacting the provider (see the failure-mode contract in the
design doc). describe / destroy / list look up by id alone -- the
provider does not need to be respecified after create.

Resource shape can be expressed three ways, from most-convenient to
most-precise:

  class     small | medium | large | xlarge (typical constraint defaults)
  numeric   --min-vram-gb / --min-ram-gb / --min-disk-gb / --gpu-count
  sku       exact provider SKU id (skips the resolver)

class and explicit constraints compose -- class sets floors, the
numeric flags can raise them. sku is mutually exclusive with class
and bypasses constraints entirely.`,
	Example: `  # Class shorthand against local (zero-cost dev path)
  iplane instance create local my-pod --class small

  # Class shorthand against RunPod (real pod, ~$0.36/hr)
  iplane instance create runpod my-pod --class small --region US-WA-1

  # Constraints-first: need 80GB VRAM
  iplane instance create runpod big-pod --min-vram-gb 80

  # Preview without provisioning
  iplane instance create runpod my-pod --class small --dry-run`,
	RunE: runInstanceCreate,
}

// runInstanceCreate is the createCmd's RunE. Builds a Spec proto from
// the parsed args + flags, dispatches to the configured client
// (in-process Service or remote gRPC), and prints the resulting
// Instance. Args order is <provider> <id>.
func runInstanceCreate(cmd *cobra.Command, args []string) error {
	provider := args[0]
	id := args[1]
	if err := checkProviderAvailable(provider); err != nil {
		return err
	}

	client, err := buildClient()
	if err != nil {
		return err
	}

	spec := &provisionerv1.Spec{
		Id:        id,
		Provider:  provider,
		Region:    createRegion,
		BaseImage: createBaseImage,
		Requirements: &provisionerv1.ResourceRequirements{
			MinVramGb: createMinVRAM,
			MinDiskGb: createMinDisk,
			MinRamGb:  createMinRAM,
			GpuCount:  createGPUCount,
			Class:     createClass,
			Sku:       createSKU,
		},
	}

	if createDryRun {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return dryRunCreate(ctx, cmd.OutOrStdout(), client, spec)
	}

	// 3-minute timeout covers RunPod's slowest p99 spawn and a generous
	// buffer for local. Operators with stranger needs can wrap in their
	// own shell-level timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	resp, err := client.CreateInstance(ctx, &provisionerv1.CreateInstanceRequest{Spec: spec})
	if err != nil {
		return fmt.Errorf("create %q: %w", id, err)
	}

	return renderCreateResult(cmd, resp)
}

// renderCreateResult prints the create response. JSON mode emits the
// full CreateInstanceResponse (including already_existed); table mode
// prints a "Created" / "Found existing" header plus the standard
// instance block from output.go.
func renderCreateResult(cmd *cobra.Command, resp *provisionerv1.CreateInstanceResponse) error {
	out := cmd.OutOrStdout()
	if instanceOutput == outputJSON {
		return writeProtoJSON(out, resp)
	}
	inst := resp.GetInstance()
	verb := "Created"
	if resp.GetAlreadyExisted() {
		verb = "Found existing"
	}
	fmt.Fprintf(out, "%s instance %q\n", verb, inst.GetId())
	fmt.Fprintf(out, "  provider:     %s\n", inst.GetProvider())
	fmt.Fprintf(out, "  provider id:  %s\n", inst.GetProviderId())
	fmt.Fprintf(out, "  state:        %s\n", instanceStateLabel(inst.GetState()))
	if sku := inst.GetGpu().GetSku(); sku != "" {
		fmt.Fprintf(out, "  sku:          %s\n", sku)
	}
	if rate := inst.GetHourlyRateUsd(); rate > 0 {
		fmt.Fprintf(out, "  hourly rate:  $%.4f/hr\n", rate)
	}
	if region := inst.GetRegion(); region != "" {
		fmt.Fprintf(out, "  region:       %s\n", region)
	}
	return nil
}

func init() {
	instanceCmd.AddCommand(instanceCreateCmd)

	f := instanceCreateCmd.Flags()
	f.StringVar(&createRegion, "region", "", `provider region (optional; runpod schedules anywhere if empty)`)
	f.StringVar(&createClass, "class", "", `gpu class shorthand: small | medium | large | xlarge`)
	f.StringVar(&createSKU, "sku", "", `exact provider sku id (bypasses constraint resolver)`)
	f.Int32Var(&createGPUCount, "gpu-count", 0, `number of GPUs on the instance (default 1)`)
	f.Int32Var(&createMinVRAM, "min-vram-gb", 0, `minimum VRAM per GPU, in GB`)
	f.Int32Var(&createMinRAM, "min-ram-gb", 0, `minimum system RAM, in GB (per instance, not per GPU)`)
	f.Int32Var(&createMinDisk, "min-disk-gb", 0, `minimum container disk, in GB`)
	f.StringVar(&createBaseImage, "base-image", "", `docker-capable base image (provider default if empty)`)
	f.BoolVar(&createDryRun, "dry-run", false, `print the planned action and exit without provider calls`)
}
