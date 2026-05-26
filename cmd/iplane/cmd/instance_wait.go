package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

var instanceWaitTimeout time.Duration

var instanceWaitCmd = &cobra.Command{
	Use:   "wait <id>",
	Short: "Block until the instance's SSH endpoint is assigned",
	Args:  cobra.ExactArgs(1),
	Long: `Block until the provider has populated the instance's SSH endpoint
in the state file. The "Join" half of an asynchronous Spawn -- some
providers (RunPod) assign the public IP a few seconds after the pod
is scheduled, so 'iplane instance create' returns ACTIVE with an
empty SSH endpoint and the operator drives the wait separately when
they need it.

Providers without an SSH-readiness gap (local) return immediately
with the existing state -- the wait is a no-op.

Designed to slot between 'iplane instance create' and 'iplane
deployment deploy':

  iplane instance create runpod my-pod --class small
  iplane instance wait my-pod
  iplane deployment deploy my-llama --instance my-pod ...`,
	RunE: runInstanceWait,
}

func runInstanceWait(cmd *cobra.Command, args []string) error {
	id := args[0]
	client, err := buildClient()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), instanceWaitTimeout+10*time.Second)
	defer cancel()

	resp, err := client.WaitForInstanceReady(ctx, &provisionerv1.WaitForInstanceReadyRequest{
		Id:             id,
		TimeoutSeconds: int32(instanceWaitTimeout / time.Second),
	})
	if err != nil {
		return fmt.Errorf("wait %q: %w", id, err)
	}

	out := cmd.OutOrStdout()
	if instanceOutput == outputJSON {
		return writeProtoJSON(out, resp)
	}
	inst := resp.GetInstance()
	verb := "Ready"
	if resp.GetAlreadyReady() {
		verb = "Already ready"
	}
	fmt.Fprintf(out, "%s: instance %q\n", verb, inst.GetId())
	if ssh := inst.GetSsh(); ssh != nil && ssh.GetHost() != "" {
		fmt.Fprintf(out, "  ssh endpoint: %s@%s:%d\n", ssh.GetUser(), ssh.GetHost(), ssh.GetPort())
	} else {
		fmt.Fprintln(out, "  ssh endpoint: (none -- this provider has no SSH surface)")
	}
	return nil
}

func init() {
	instanceCmd.AddCommand(instanceWaitCmd)

	instanceWaitCmd.Flags().DurationVar(&instanceWaitTimeout, "timeout", 90*time.Second,
		`maximum time to wait for the SSH endpoint`)
}
