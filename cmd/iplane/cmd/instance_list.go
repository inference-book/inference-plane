package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

var (
	listProvider string
	listRemote   bool
	listAll      bool
)

var instanceListCmd = &cobra.Command{
	Use:   "list",
	Short: "List iplane instances",
	Long: `List instances tracked by iplane.

Two sources, intentionally not unified -- the design doc treats them
as different questions worth asking separately:

  default              read from the local state file (~/.iplane/state.json).
                       Fast; the operator's own audit log.

  --remote             query the provider directly. Surfaces instances
                       under the operator's tag that the local state
                       file doesn't know about (the wiped-state-file
                       recovery path), and instances created outside
                       iplane that match the operator tag.

--remote requires --provider (we don't enumerate all configured
providers silently; see design doc line 99 -- v0.1 punts that to v0.3).

By default the list HIDES instances in terminal states (TERMINATED,
FAILED) so an operator's day-to-day view is just live resources. The
state file still has the records -- 'iplane instance describe <id>'
works on them. Pass --all to include terminal-state records in the
list output (useful for audit / debugging).`,
	RunE: runInstanceList,
}

func runInstanceList(cmd *cobra.Command, args []string) error {
	if listRemote && listProvider == "" {
		return fmt.Errorf("--remote requires --provider (we do not enumerate every configured adapter silently; see design doc line 99)")
	}
	if listProvider != "" {
		if err := checkProviderAvailable(listProvider); err != nil {
			return err
		}
	}

	client, err := buildClient()
	if err != nil {
		return err
	}

	source := provisionerv1.Source_SOURCE_LOCAL
	if listRemote {
		source = provisionerv1.Source_SOURCE_REMOTE
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := client.ListInstances(ctx, &provisionerv1.ListInstancesRequest{
		Source:   source,
		Provider: listProvider,
	})
	if err != nil {
		return fmt.Errorf("list: %w", err)
	}

	instances := resp.GetInstances()
	if !listAll {
		instances = filterLiveInstances(instances)
	}
	return renderInstances(cmd.OutOrStdout(), instanceOutput, instances)
}

// filterLiveInstances drops records in terminal states (TERMINATED,
// FAILED). Default behavior of `iplane instance list` so the day-to-day
// view is just live resources; the operator opts into the full audit
// view via --all. Matches `docker ps` (no `-a`, hides exited
// containers).
func filterLiveInstances(in []*provisionerv1.Instance) []*provisionerv1.Instance {
	out := make([]*provisionerv1.Instance, 0, len(in))
	for _, inst := range in {
		switch inst.GetState() {
		case provisionerv1.InstanceState_INSTANCE_STATE_TERMINATED,
			provisionerv1.InstanceState_INSTANCE_STATE_FAILED:
			continue
		}
		out = append(out, inst)
	}
	return out
}

func init() {
	instanceCmd.AddCommand(instanceListCmd)

	f := instanceListCmd.Flags()
	f.StringVar(&listProvider, "provider", "", `restrict to one provider (required with --remote)`)
	f.BoolVar(&listRemote, "remote", false, `query the provider directly rather than the local state file`)
	f.BoolVar(&listAll, "all", false, `include TERMINATED and FAILED instances (hidden by default)`)
}
