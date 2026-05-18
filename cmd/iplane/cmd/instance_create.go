package cmd

import (
	"github.com/spf13/cobra"
)

// Flags scoped to `iplane instance create`. The constraints-first
// design (see docs/design/0001-provisioner.md "Resource requirements")
// exposes both the class shorthand and the underlying numeric knobs;
// operators reach for class when they want sane defaults and reach
// for the numeric flags when they know exactly what they need.
var (
	createProvider  string
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
	Use:   "create <id>",
	Short: "Create a GPU instance",
	Args:  cobra.ExactArgs(1),
	Long: `Create a new GPU instance under the given iplane id.

The id is operator-supplied and tenant-globally unique across providers.
Idempotent: re-running with the same id returns the existing record
without contacting the provider (see the failure-mode contract in the
design doc).

Resource shape can be expressed three ways, from most-convenient to
most-precise:

  class     small | medium | large | xlarge (typical constraint defaults)
  numeric   --min-vram-gb / --min-ram-gb / --min-disk-gb / --gpu-count
  sku       exact provider SKU id (skips the resolver)

class and explicit constraints compose -- class sets floors, the
numeric flags can raise them. sku is mutually exclusive with class
and bypasses constraints entirely.`,
	Example: `  # Class shorthand against local (zero-cost dev path)
  iplane instance create my-pod --provider local --class small

  # Class shorthand against RunPod (real pod, ~$0.36/hr)
  iplane instance create my-pod --provider runpod --class small --region US-WA-1

  # Constraints-first: need 80GB VRAM
  iplane instance create big-pod --provider runpod --min-vram-gb 80

  # Preview without provisioning
  iplane instance create my-pod --provider runpod --class small --dry-run`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return cmd.Help() // wired in commit 2
	},
}

func init() {
	instanceCmd.AddCommand(instanceCreateCmd)

	f := instanceCreateCmd.Flags()
	f.StringVar(&createProvider, "provider", "", `provider adapter (local | runpod)`)
	f.StringVar(&createRegion, "region", "", `provider region (optional; runpod schedules anywhere if empty)`)
	f.StringVar(&createClass, "class", "", `gpu class shorthand: small | medium | large | xlarge`)
	f.StringVar(&createSKU, "sku", "", `exact provider sku id (bypasses constraint resolver)`)
	f.Int32Var(&createGPUCount, "gpu-count", 0, `number of GPUs on the instance (default 1)`)
	f.Int32Var(&createMinVRAM, "min-vram-gb", 0, `minimum VRAM per GPU, in GB`)
	f.Int32Var(&createMinRAM, "min-ram-gb", 0, `minimum system RAM, in GB (per instance, not per GPU)`)
	f.Int32Var(&createMinDisk, "min-disk-gb", 0, `minimum container disk, in GB`)
	f.StringVar(&createBaseImage, "base-image", "", `docker-capable base image (provider default if empty)`)
	f.BoolVar(&createDryRun, "dry-run", false, `print the planned action and exit without provider calls`)

	_ = instanceCreateCmd.MarkFlagRequired("provider")
}
