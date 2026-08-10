package cmd

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/gen/go/provisioner/v1/provisionerv1connect"
)

var (
	fleetDrainTimeout time.Duration
	fleetDrainForce   bool
)

var fleetDrainCmd = &cobra.Command{
	Use:   "drain <member>",
	Short: "Take a member out of service and release its hardware",
	Long: `Stop new work landing on a member, let in-flight requests finish, then
release every node it spans.

The release half is why this exists as one verb. A distributed member holds
several nodes, and reclaiming them one at a time is a sequence the operator
has to get right; a partial teardown leaves nodes billing. Draining the member
releases the whole span in one action.

The wait is a fixed grace period, not a poll for in-flight reaching zero. The
control plane has no trustworthy count to poll -- in-flight lives in the
router, and the channel that carries it upward is deliberately lagging, so
draining early on a stale zero would cut live requests.

--force skips the wait entirely. In-flight requests see their connections cut.

Requires --service-url: draining releases hardware, so it runs against the
daemon that owns the state rather than against the state file directly.`,
	Args: cobra.ExactArgs(1),
	RunE: runFleetDrain,
}

func init() {
	fleetCmd.AddCommand(fleetDrainCmd)
	f := fleetDrainCmd.Flags()
	f.DurationVar(&fleetDrainTimeout, "timeout", 0,
		"how long to let in-flight work finish (0 = server default, 2m)")
	f.BoolVar(&fleetDrainForce, "force", false,
		"skip the wait; in-flight requests on this member are cut")
}

func runFleetDrain(cmd *cobra.Command, args []string) error {
	member := args[0]
	if fleetServiceURL == "" {
		return fmt.Errorf("fleet drain requires --service-url: it releases hardware, so it runs " +
			"against the daemon that owns the state rather than the state file")
	}
	if fleetDrainForce && fleetDrainTimeout > 0 {
		return fmt.Errorf("--force and --timeout are mutually exclusive: --force is the no-wait path")
	}

	client := provisionerv1connect.NewEngineRegistryServiceClient(http.DefaultClient, fleetServiceURL)

	// The client deadline has to outlast the server's wait, or the CLI gives
	// up on a drain that is proceeding correctly and the operator is left
	// unsure whether the hardware was released. Same three-timeouts-aligned
	// lesson the Ch 9 deploy path learned.
	wait := fleetDrainTimeout
	if wait <= 0 {
		wait = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), wait+5*time.Minute)
	defer cancel()

	if !fleetDrainForce {
		fmt.Fprintf(cmd.OutOrStdout(), "draining %s (waiting up to %s for in-flight work)\n", member, wait)
	} else {
		fmt.Fprintf(cmd.OutOrStdout(), "draining %s (--force: not waiting)\n", member)
	}

	resp, err := client.DrainEngine(ctx, connect.NewRequest(&provisionerv1.DrainEngineRequest{
		EngineId:       member,
		TimeoutSeconds: int32(fleetDrainTimeout / time.Second),
		Force:          fleetDrainForce,
	}))
	if err != nil {
		return fmt.Errorf("drain %s: %w", member, err)
	}

	released := resp.Msg.GetReleasedInstanceIds()
	fmt.Fprintf(cmd.OutOrStdout(), "drained %s; released %d node(s)\n", member, len(released))
	for _, id := range released {
		fmt.Fprintf(cmd.OutOrStdout(), "  %s\n", id)
	}
	return nil
}
