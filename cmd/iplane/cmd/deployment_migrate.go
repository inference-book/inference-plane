package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

var (
	migrateTo       string
	migrateRegion   string
	migrateClass    string
	migrateSKU      string
	migrateDrainSec int32
	migrateForce    bool
	migrateDryRun   bool
)

var deploymentMigrateCmd = &cobra.Command{
	Use:   "migrate <id> --to <provider>",
	Short: "Move a running deployment to another provider",
	Args:  cobra.ExactArgs(1),
	Long: `Move a running deployment to another provider without changing its id.

Grows the deployment onto the destination, waits for the new replicas to
be genuinely serving, then drains and releases the originals. Traffic
moves because the endpoint set changed, so nothing is dropped in the
handover.

The requirements default to whatever the source replicas were asked for,
so "same shape, different vendor" needs only --to. Pass --class or --sku
to change the hardware at the same time.

Weights are staged per provider and per region. Migrating somewhere the
model is not pinned pays a full cold start, which on a large model is
minutes. --dry-run says so before anything is provisioned.`,
	RunE: runDeploymentMigrate,
}

func runDeploymentMigrate(cmd *cobra.Command, args []string) error {
	id := args[0]
	if migrateTo == "" {
		return fmt.Errorf("migrate requires --to <provider>")
	}
	if migrateClass != "" && migrateSKU != "" {
		return fmt.Errorf("--class and --sku are mutually exclusive")
	}

	client, err := buildDeploymentClient()
	if err != nil {
		return err
	}

	to := &provisionerv1.ReplicaSpec{Provider: migrateTo, Region: migrateRegion}
	if migrateClass != "" || migrateSKU != "" {
		to.Requirements = &provisionerv1.ResourceRequirements{Class: migrateClass, Sku: migrateSKU}
	}

	// A migration provisions the destination and then waits out a drain, so
	// the budget is a scale-up plus a grace period rather than either alone.
	timeout := 30 * time.Second
	if !migrateDryRun {
		timeout = 20 * time.Minute
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
	defer cancel()

	resp, err := client.MigrateDeployment(ctx, &provisionerv1.MigrateDeploymentRequest{
		Id:              id,
		To:              to,
		DrainTimeoutSec: migrateDrainSec,
		Force:           migrateForce,
		DryRun:          migrateDryRun,
	})
	if err != nil {
		return fmt.Errorf("migrate %q to %s: %w", id, migrateTo, err)
	}

	out := cmd.OutOrStdout()
	if deploymentOutput == outputJSON {
		return writeProtoJSON(out, resp)
	}

	prefix := ""
	if migrateDryRun {
		prefix = "[dry-run] would "
	}
	fmt.Fprintf(out, "%smigrate %q: %s -> %s (%d replica(s))\n",
		prefix, resp.GetId(), dashIfEmpty(resp.GetFromProvider()), resp.GetToProvider(),
		len(resp.GetSourceInstanceIds()))

	// The warnings are the reason this verb prints anything at all on a dry
	// run. Cold start dominates the cost of most migrations and an operator
	// who did not expect it reads a stalled deploy as a hang.
	for _, w := range resp.GetWarnings() {
		fmt.Fprintf(out, "  warning: %s\n", w)
	}
	if resp.GetWarmCacheFollows() {
		fmt.Fprintln(out, "  the model is already pinned at the destination, so this is a warm move")
	}
	if len(resp.GetAddedInstanceIds()) > 0 {
		fmt.Fprintf(out, "  added:   %v\n", resp.GetAddedInstanceIds())
		fmt.Fprintf(out, "  drained: %v\n", resp.GetSourceInstanceIds())
	}
	return nil
}

func init() {
	deploymentCmd.AddCommand(deploymentMigrateCmd)

	f := deploymentMigrateCmd.Flags()
	f.StringVar(&migrateTo, "to", "", `destination provider (required)`)
	f.StringVar(&migrateRegion, "region", "", `destination region, when the provider pins one`)
	f.StringVar(&migrateClass, "class", "", `change the gpu class while migrating (default: keep the source shape)`)
	f.StringVar(&migrateSKU, "sku", "", `change to an exact provider sku while migrating`)
	f.Int32Var(&migrateDrainSec, "drain-timeout", 0, `seconds to let in-flight work finish on the source (0 = default)`)
	f.BoolVar(&migrateForce, "force", false, `release the source immediately, cutting in-flight requests`)
	f.BoolVar(&migrateDryRun, "dry-run", false, `plan the move and print it without provisioning anything`)
}
