package cmd

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/gen/go/provisioner/v1/provisionerv1connect"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/local"
	"github.com/inference-book/inference-plane/internal/provisioners/runpod"
	"github.com/inference-book/inference-plane/internal/provisioners/state"
)

// Shared flags on the instance group. Each subcommand reads them via
// the package-level vars below; cobra populates them before Run fires.
var (
	instanceStateDir   string
	instanceOperatorID string
	instanceOutput     string // table | json
	instanceServiceURL string // when set, forward to a running iplane serve
)

// provisionerClient is the common surface between in-process Service
// and a gRPC ProvisionerServiceClient. The Service struct already
// implements every method on this set (see internal/provisioners/
// service.go's package doc lines 23-32), so the in-process path is a
// direct assignment, not an adapter. Both modes return
// *connect.Response[T] -- the calling code never branches on transport.
type provisionerClient interface {
	CreateInstance(context.Context, *connect.Request[provisionerv1.CreateInstanceRequest]) (*connect.Response[provisionerv1.CreateInstanceResponse], error)
	DestroyInstance(context.Context, *connect.Request[provisionerv1.DestroyInstanceRequest]) (*connect.Response[provisionerv1.DestroyInstanceResponse], error)
	DescribeInstance(context.Context, *connect.Request[provisionerv1.DescribeInstanceRequest]) (*connect.Response[provisionerv1.DescribeInstanceResponse], error)
	ListInstances(context.Context, *connect.Request[provisionerv1.ListInstancesRequest]) (*connect.Response[provisionerv1.ListInstancesResponse], error)
}

// instanceCmd is the `iplane instance` cobra group. Subcommands attach
// to it via init() and inherit its persistent flags.
//
// Two transport modes (see the provisionerClient interface above):
//
//   - In-process (default): the binary opens ~/.iplane/state.json under
//     flock, instantiates Provisioner adapters in-process, and calls
//     Service methods directly. No iplane serve required.
//   - Remote: when --service-url (or IPLANE_SERVICE_URL) is set, the
//     CLI dials a running iplane serve via the generated gRPC client.
//     The local state file is NOT touched; the server's state file
//     is the single source of truth. Useful when multiple operators
//     share a control plane, or when CI wants to drive a long-running
//     service rather than redo flock dances per invocation.
//
// Subcommands speak the same call API in both modes (provisionerClient
// interface). Tests pick a mode per case.
var instanceCmd = &cobra.Command{
	Use:   "instance",
	Short: "Create / list / describe / destroy iplane instances",
	Long: `Provision and operate GPU instances against the configured providers.

Two providers ship in v0.1:

  local    Provision against this laptop. Zero cost, no API key needed.
  runpod   Provision a real RunPod pod. Requires RUNPOD_API_KEY.

By default the CLI is self-contained: it reads ~/.iplane/state.json
directly and instantiates provider adapters in-process. Pass
--service-url to forward to a running iplane serve instead.

Every state-changing subcommand supports --dry-run to preview the
action.`,
}

func init() {
	rootCmd.AddCommand(instanceCmd)

	pf := instanceCmd.PersistentFlags()
	pf.StringVar(&instanceStateDir, "state-dir", "",
		`directory holding state.json + .lock (default ~/.iplane; ignored when --service-url is set)`)
	pf.StringVar(&instanceOperatorID, "operator", "default",
		`operator id stamped on instances (v0.1 single-operator default)`)
	pf.StringVar(&instanceOutput, "output", "table",
		`output format: table | json`)
	pf.StringVar(&instanceServiceURL, "service-url", os.Getenv("IPLANE_SERVICE_URL"),
		`when set, forward to a running iplane serve at this URL (e.g. http://localhost:9091)`)
}

// resolveStateDir returns the explicit --state-dir if set, else
// ~/.iplane (creating it implicitly via state.Open). Fails fast when
// the home dir is unavailable -- the alternative would be silently
// landing state in $CWD, which is a worse surprise.
func resolveStateDir() (string, error) {
	if instanceStateDir != "" {
		return instanceStateDir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir for default state path: %w", err)
	}
	return filepath.Join(home, ".iplane"), nil
}

// buildClient returns the provisionerClient subcommands should call.
// Dispatches on --service-url:
//
//   - empty   -> in-process Service. Opens state file under flock,
//                wires local + runpod adapters. The "self-contained
//                one-shot CLI" path the design doc describes.
//   - non-empty -> gRPC client dialing the URL. Local state file is
//                not opened; the server owns state.
//
// Both branches return the same provisionerClient interface so every
// subcommand calls client.CreateInstance(ctx, ...) with no transport
// branching at the call site.
func buildClient() (provisionerClient, error) {
	if instanceServiceURL != "" {
		return provisionerv1connect.NewProvisionerServiceClient(http.DefaultClient, instanceServiceURL), nil
	}

	dir, err := resolveStateDir()
	if err != nil {
		return nil, err
	}
	store, err := state.Open(dir, instanceOperatorID)
	if err != nil {
		return nil, fmt.Errorf("open state store: %w", err)
	}

	providers := []provisioners.Provider{local.New()}
	if key := os.Getenv("RUNPOD_API_KEY"); key != "" {
		providers = append(providers, runpod.New(runpod.NewClient(key)))
	}

	return provisioners.New(providers, store, instanceOperatorID), nil
}

// checkProviderAvailable surfaces the "you asked for runpod but
// RUNPOD_API_KEY isn't set" case before the Service does, with an
// actionable message instead of "unknown provider runpod". Only
// relevant in in-process mode -- in remote mode the running server
// owns provider availability, so we let its error message stand.
func checkProviderAvailable(name string) error {
	if instanceServiceURL != "" {
		return nil // remote server decides
	}
	switch name {
	case provisioners.ProviderLocal:
		return nil
	case provisioners.ProviderRunPod:
		if os.Getenv("RUNPOD_API_KEY") == "" {
			return fmt.Errorf("provider runpod requires RUNPOD_API_KEY in env (or pass --service-url to forward to a running iplane serve)")
		}
		return nil
	default:
		return fmt.Errorf("unknown provider %q (supported: local, runpod)", name)
	}
}
