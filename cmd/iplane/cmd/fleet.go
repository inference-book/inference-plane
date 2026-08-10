package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/spf13/cobra"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/gen/go/provisioner/v1/provisionerv1connect"
	"github.com/inference-book/inference-plane/internal/engines"
	"github.com/inference-book/inference-plane/internal/provisioners/stores/file"

	"connectrpc.com/connect"
)

var (
	fleetServiceURL string
	fleetStateDir   string
	fleetOperatorID string
	fleetOutput     string
	fleetDeployment string
	fleetShowLost   bool
)

// fleetCmd groups the verbs over registered data planes.
//
// Deliberately NOT a parallel object type next to `deployment`. A distributed
// engine is the same kind of thing as a single-card one from the deployment's
// side, one endpoint serving one model, which is the promise Ch 8 made and
// Ch 10 is careful not to break. So a fleet member gains a span column and
// everything the earlier chapters taught keeps working; there is no
// `iplane pool` or `iplane group` surface and there should not be one.
var fleetCmd = &cobra.Command{
	Use:   "fleet",
	Short: "Inspect and manage registered data planes",
	Long: `Verbs over the engines that have registered with the control plane.

A fleet member is one engine: one endpoint, one model, over a span of one or
more cards on one or more nodes. Single-card and distributed engines appear in
the same list, distinguished only by the span column.

Reads the local state file by default, or the remote daemon when
--service-url is set.`,
}

var fleetStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "List every registered data plane",
	Long: `Show each engine that has registered, with its span and state.

States are wider than the router's healthy/quarantined pair, because the
registration channel carries facts a health probe cannot phrase a question
about:

  assembling          some workers joined, the group has not formed. There is
                      no endpoint to probe yet, so this is invisible to a
                      health poller. It is NOT a failure.
  serving             group formed, endpoint answering.
  serving, link down  assembled, returning correct tokens, and running well
                      below expected throughput. Reads as neither healthy nor
                      failed, which is the point.
  draining            taken out of service on purpose; finishing in-flight work.
  lost                stopped renewing its lease.`,
	RunE: runFleetStatus,
}

func init() {
	rootCmd.AddCommand(fleetCmd)
	fleetCmd.AddCommand(fleetStatusCmd)

	pf := fleetCmd.PersistentFlags()
	// IPLANE_SERVICE_URL default, matching `deployment`, `instance` and
	// `load`. Without it `fleet` was the one remote-transport verb that
	// silently read the local state file while every sibling verb honoured
	// the exported variable, which reads as "no engines have registered"
	// against a daemon that has several.
	pf.StringVar(&fleetServiceURL, "service-url", os.Getenv("IPLANE_SERVICE_URL"),
		"daemon base URL (e.g. http://localhost:8080); reads the local state file when unset")
	pf.StringVar(&fleetStateDir, "state-dir", "",
		"directory holding state.json + .lock (default ~/.iplane; ignored when --service-url is set)")
	pf.StringVar(&fleetOperatorID, "operator", "default", "operator id namespacing the state file")

	f := fleetStatusCmd.Flags()
	f.StringVarP(&fleetOutput, "output", "o", "table", "output format: table | json")
	f.StringVar(&fleetDeployment, "deployment", "", "only members belonging to this deployment id")
	f.BoolVar(&fleetShowLost, "show-lost", false,
		"include members whose lease expired (hidden by default, like terminal-state deployments)")
}

// engineLister is the slice of the registry surface `fleet status` needs.
// Narrow so the in-process and remote transports are interchangeable and
// tests can substitute a fake.
type engineLister interface {
	ListEngines(ctx context.Context, deploymentID string) ([]*provisionerv1.Engine, error)
}

type connectEngineLister struct {
	c provisionerv1connect.EngineRegistryServiceClient
}

func (l *connectEngineLister) ListEngines(ctx context.Context, deploymentID string) ([]*provisionerv1.Engine, error) {
	resp, err := l.c.ListEngines(ctx, connect.NewRequest(&provisionerv1.ListEnginesRequest{
		DeploymentId: deploymentID,
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetEngines(), nil
}

type localEngineLister struct {
	registry *engines.Registry
}

func (l *localEngineLister) ListEngines(_ context.Context, deploymentID string) ([]*provisionerv1.Engine, error) {
	return l.registry.List(deploymentID, provisionerv1.EngineState_ENGINE_STATE_UNSPECIFIED)
}

// buildEngineLister mirrors buildDeploymentClient's transport split: remote
// when --service-url is set, otherwise the local state file.
//
// The local path takes a read-only view and does NOT take the lifetime lock.
// `fleet status` is the verb an operator reaches for while the daemon is
// running, and the daemon holds that lock for its lifetime; failing here
// would make the useful case the broken one. A torn read is not a concern
// because the store writes by atomic rename.
func buildEngineLister() (engineLister, error) {
	if fleetServiceURL != "" {
		return &connectEngineLister{
			c: provisionerv1connect.NewEngineRegistryServiceClient(http.DefaultClient, fleetServiceURL),
		}, nil
	}
	dir := fleetStateDir
	if dir == "" {
		resolved, err := resolveDeploymentStateDir()
		if err != nil {
			return nil, err
		}
		dir = resolved
	}
	store, err := file.Open(dir, fleetOperatorID)
	if err != nil {
		return nil, fmt.Errorf("open state store: %w", err)
	}
	return &localEngineLister{registry: engines.New(engines.NewStateStore(store))}, nil
}

func runFleetStatus(cmd *cobra.Command, _ []string) error {
	switch fleetOutput {
	case "table", "json":
	default:
		return fmt.Errorf("--output: unknown format %q (want table | json)", fleetOutput)
	}

	lister, err := buildEngineLister()
	if err != nil {
		return err
	}
	all, err := lister.ListEngines(cmd.Context(), fleetDeployment)
	if err != nil {
		return fmt.Errorf("fleet status: %w", err)
	}
	if !fleetShowLost {
		all = filterLostEngines(all)
	}
	return renderFleet(cmd.OutOrStdout(), fleetOutput, all)
}

// filterLostEngines drops members whose lease expired, matching the
// hide-terminal-records default of `deployment list` and `instance list`.
// --show-lost brings them back, which is what an operator wants when asking
// why something disappeared.
func filterLostEngines(in []*provisionerv1.Engine) []*provisionerv1.Engine {
	out := make([]*provisionerv1.Engine, 0, len(in))
	for _, e := range in {
		if e.GetState() == provisionerv1.EngineState_ENGINE_STATE_LOST {
			continue
		}
		out = append(out, e)
	}
	return out
}

// errUnknownFleetOutput is returned for an unsupported --output value.
var errUnknownFleetOutput = errors.New("unknown fleet output format")
