package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

var (
	destroyForce  bool
	destroyDryRun bool
)

var instanceDestroyCmd = &cobra.Command{
	Use:   "destroy <id>",
	Short: "Terminate an instance and record it in the audit log",
	Args:  cobra.ExactArgs(1),
	Long: `Terminate the named instance via the provider and patch the local
state record to TERMINATED. Idempotent: a destroy against an already-
terminated record is a no-op, and an "instance not found" response
from the provider is treated as success (the desired end state is the
same).

--force skips the provider Terminate call. Use only when the provider
has confirmed the instance is gone and the local record is stuck in
TERMINATING.`,
	RunE: runInstanceDestroy,
}

func runInstanceDestroy(cmd *cobra.Command, args []string) error {
	id := args[0]
	client, err := buildClient()
	if err != nil {
		return err
	}

	if destroyDryRun {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return dryRunDestroy(ctx, cmd.OutOrStdout(), client, id)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	resp, err := client.DestroyInstance(ctx, &provisionerv1.DestroyInstanceRequest{
		Id:    id,
		Force: destroyForce,
	})
	if err != nil {
		return fmt.Errorf("destroy %q: %w", id, err)
	}
	out := cmd.OutOrStdout()
	if instanceOutput == outputJSON {
		return writeProtoJSON(out, resp)
	}
	inst := resp.GetInstance()
	fmt.Fprintf(out, "Destroyed instance %q (final state: %s)\n",
		inst.GetId(), instanceStateLabel(inst.GetState()))
	return nil
}

func init() {
	instanceCmd.AddCommand(instanceDestroyCmd)

	f := instanceDestroyCmd.Flags()
	f.BoolVar(&destroyForce, "force", false,
		`skip the provider call; mark TERMINATED locally only (recovery)`)
	f.BoolVar(&destroyDryRun, "dry-run", false,
		`print the planned action and exit without provider calls`)
}
