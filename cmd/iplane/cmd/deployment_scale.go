package cmd

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

var (
	deploymentScaleWait   bool
	deploymentScaleDryRun bool
)

var deploymentScaleCmd = &cobra.Command{
	Use:   "scale <id> <N>",
	Short: "Change a deployment's replica count",
	Args:  cobra.ExactArgs(2),
	Long: `Change a deployment's replica count to N.

v0.2 ch7-beat3.8 ships scale-up only: N greater than the current
replica count appends new replicas by extending the slot numbering
(deploy_id-rK, deploy_id-r(K+1), ...). Existing slots are not
touched -- a DEGRADED tombstone from a prior fan-out partial failure
stays as a tombstone until #93's reconciliation refills it.

N equal to current is a no-op. N less than current returns
Unimplemented; scale-down's drain-and-destroy semantics are
designed in #145.

--dry-run prints the planned new instance ids without provisioning.`,
	RunE: runDeploymentScale,
}

func runDeploymentScale(cmd *cobra.Command, args []string) error {
	id := args[0]
	target, err := strconv.Atoi(args[1])
	if err != nil {
		return fmt.Errorf("invalid replica count %q: %w", args[1], err)
	}
	if target <= 0 {
		return fmt.Errorf("replica count must be > 0 (got %d)", target)
	}

	client, err := buildDeploymentClient()
	if err != nil {
		return err
	}

	// Generous timeout: each new replica may take ~1-2 minutes to
	// provision (rent + boot + engine ready). 10 min budget for a
	// scale-up of a few replicas under reasonable provider latency.
	timeout := 30 * time.Second
	if !deploymentScaleDryRun && deploymentScaleWait {
		timeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
	defer cancel()

	resp, err := client.ScaleDeployment(ctx, &provisionerv1.ScaleDeploymentRequest{
		Id:             id,
		TargetReplicas: int32(target),
		Wait:           deploymentScaleWait && !deploymentScaleDryRun,
		DryRun:         deploymentScaleDryRun,
	})
	if err != nil {
		return fmt.Errorf("scale %q to %d: %w", id, target, err)
	}

	out := cmd.OutOrStdout()
	if deploymentOutput == outputJSON {
		return writeProtoJSON(out, resp)
	}

	dep := resp.GetDeployment()
	current := len(dep.GetInstanceIds())
	if current == 0 && dep.GetInstanceId() != "" {
		current = 1
	}
	if deploymentScaleDryRun {
		fmt.Fprintf(out, "[dry-run] deployment %q (state: %s)\n",
			dep.GetId(), deploymentStateLabel(dep.GetState()))
		fmt.Fprintf(out, "[dry-run] current replicas: %d\n", current)
		fmt.Fprintf(out, "[dry-run] target replicas:  %d\n", target)
		planned := resp.GetPlannedInstanceIds()
		if len(planned) == 0 {
			fmt.Fprintln(out, "[dry-run] no-op (already at target)")
			return nil
		}
		fmt.Fprintf(out, "[dry-run] would provision %d new replicas:\n", len(planned))
		for _, pid := range planned {
			fmt.Fprintf(out, "[dry-run]   - %s\n", pid)
		}
		return nil
	}

	fmt.Fprintf(out, "Scaled deployment %q to %d replicas (state: %s)\n",
		dep.GetId(), current, deploymentStateLabel(dep.GetState()))
	if reason := dep.GetFailureReason(); reason != "" {
		fmt.Fprintf(out, "  failure_reason: %s\n", reason)
	}
	return nil
}

func init() {
	deploymentCmd.AddCommand(deploymentScaleCmd)

	f := deploymentScaleCmd.Flags()
	f.BoolVar(&deploymentScaleWait, "wait", true,
		`block until new replicas reach a terminal aggregate state`)
	f.BoolVar(&deploymentScaleDryRun, "dry-run", false,
		`print the planned new instance ids without provisioning`)
}
