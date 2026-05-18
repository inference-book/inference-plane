package cmd

import (
	"context"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

var describeRemote bool

var instanceDescribeCmd = &cobra.Command{
	Use:   "describe <id>",
	Short: "Show full details of one instance",
	Args:  cobra.ExactArgs(1),
	Long: `Look up one instance by iplane id and print every field iplane tracks.

By default reads from the local state file. With --remote, calls the
provider's describe endpoint and renders the live view -- useful when
the local record is stale or the operator is reconciling.`,
	RunE: runInstanceDescribe,
}

func runInstanceDescribe(cmd *cobra.Command, args []string) error {
	id := args[0]
	client, err := buildClient()
	if err != nil {
		return err
	}

	source := provisionerv1.Source_SOURCE_LOCAL
	if describeRemote {
		source = provisionerv1.Source_SOURCE_REMOTE
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := client.DescribeInstance(ctx, connect.NewRequest(&provisionerv1.DescribeInstanceRequest{
		Id:     id,
		Source: source,
	}))
	if err != nil {
		return fmt.Errorf("describe %q: %w", id, err)
	}
	return renderInstanceDetail(cmd, resp.Msg.GetInstance())
}

// renderInstanceDetail prints every operator-facing field on the
// Instance. Hidden by default in list (which keeps to the summary
// columns); describe is where the full record lives.
func renderInstanceDetail(cmd *cobra.Command, inst *provisionerv1.Instance) error {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "id:            %s\n", inst.GetId())
	fmt.Fprintf(out, "provider:      %s\n", inst.GetProvider())
	fmt.Fprintf(out, "provider id:   %s\n", inst.GetProviderId())
	fmt.Fprintf(out, "state:         %s\n", instanceStateLabel(inst.GetState()))
	fmt.Fprintf(out, "region:        %s\n", emptyAsDash(inst.GetRegion()))
	if gpu := inst.GetGpu(); gpu != nil {
		fmt.Fprintf(out, "gpu class:     %s\n", emptyAsDash(gpu.GetClass()))
		fmt.Fprintf(out, "gpu sku:       %s\n", emptyAsDash(gpu.GetSku()))
		fmt.Fprintf(out, "gpu count:     %d\n", gpu.GetCount())
		fmt.Fprintf(out, "vram (GB):     %d\n", gpu.GetVramGb())
	}
	fmt.Fprintf(out, "hourly rate:   $%.4f/hr\n", inst.GetHourlyRateUsd())
	if ts := inst.GetCreatedAt(); ts != nil {
		fmt.Fprintf(out, "created at:    %s\n", ts.AsTime().Format(time.RFC3339))
	}
	if ts := inst.GetActivatedAt(); ts != nil {
		fmt.Fprintf(out, "activated at:  %s\n", ts.AsTime().Format(time.RFC3339))
	}
	if ts := inst.GetTerminatedAt(); ts != nil {
		fmt.Fprintf(out, "terminated at: %s\n", ts.AsTime().Format(time.RFC3339))
	}
	if ssh := inst.GetSsh(); ssh != nil && ssh.GetHost() != "" {
		fmt.Fprintf(out, "ssh:           %s@%s:%d\n", ssh.GetUser(), ssh.GetHost(), ssh.GetPort())
	}
	if reason := inst.GetFailureReason(); reason != "" {
		fmt.Fprintf(out, "failure:       %s\n", reason)
	}
	return nil
}

func emptyAsDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func init() {
	instanceCmd.AddCommand(instanceDescribeCmd)

	instanceDescribeCmd.Flags().BoolVar(&describeRemote, "remote", false,
		`query the provider directly rather than the local state file`)
}
