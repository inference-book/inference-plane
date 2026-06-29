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
	deploymentScaleWait        bool
	deploymentScaleDryRun      bool
	deploymentScaleAddReplicas []string
)

var deploymentScaleCmd = &cobra.Command{
	Use:   "scale <id> [<N>]",
	Short: "Change a deployment's replica count",
	Args:  cobra.RangeArgs(1, 2),
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

	addSpecs, err := parseReplicaSpecs(deploymentScaleAddReplicas)
	if err != nil {
		return err
	}

	// Two forms:
	//   - homogeneous: positional <N> (absolute target)
	//   - heterogeneous: --add-replica '<provider>:<class>' repeatable
	// Mutually exclusive.
	var target int32
	switch {
	case len(addSpecs) > 0 && len(args) >= 2:
		return fmt.Errorf("--add-replica is mutually exclusive with the positional target count <N> -- use one form per call")
	case len(addSpecs) > 0:
		// heterogeneous: count comes from len(--add-replica)
	case len(args) >= 2:
		t, perr := strconv.Atoi(args[1])
		if perr != nil {
			return fmt.Errorf("invalid replica count %q: %w", args[1], perr)
		}
		if t <= 0 {
			return fmt.Errorf("replica count must be > 0 (got %d)", t)
		}
		target = int32(t)
	default:
		return fmt.Errorf("scale requires either a positional <N> or one or more --add-replica '<provider>:<class>' flags")
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

	req := &provisionerv1.ScaleDeploymentRequest{
		Id:     id,
		Wait:   deploymentScaleWait && !deploymentScaleDryRun,
		DryRun: deploymentScaleDryRun,
	}
	if len(addSpecs) > 0 {
		req.AddReplicas = addSpecs
	} else {
		req.TargetReplicas = target
	}
	resp, err := client.ScaleDeployment(ctx, req)
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
		if len(addSpecs) > 0 {
			fmt.Fprintf(out, "[dry-run] adding %d heterogeneous replicas:\n", len(addSpecs))
			for i, spec := range addSpecs {
				fmt.Fprintf(out, "[dry-run]   r%d: provider=%s class=%s\n",
					current+i, spec.GetProvider(), spec.GetRequirements().GetClass())
			}
		} else {
			fmt.Fprintf(out, "[dry-run] target replicas:  %d\n", target)
		}
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
	f.StringSliceVar(&deploymentScaleAddReplicas, "add-replica", nil,
		`per-replica spec in 'provider:class' form (e.g., runpod:small). Repeatable. Each --add-replica appends one replica with that (provider, class) shape. Mutually exclusive with the positional <N> count -- use this form for heterogeneous scale-up (1 runpod + 1 vast + 1 lambda).`)
}
