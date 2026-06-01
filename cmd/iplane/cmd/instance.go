package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/gen/go/provisioner/v1/provisionerv1connect"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/stores/file"
)

// Shared flags on the instance group. Each subcommand reads them via
// the package-level vars below; cobra populates them before Run fires.
var (
	instanceStateDir   string
	instanceOperatorID string
	instanceOutput     string // table | json
	instanceServiceURL string // when set, forward to a running iplane serve
)

// provisionerClient is the common surface every subcommand calls. The
// shape matches the generated gRPC server interface
// (provisionerv1.ProvisionerServiceServer) -- (ctx, *Req) → (*Resp,
// error) -- so *provisioners.Service satisfies it directly with no
// adapter. The remote path wraps a connect-rpc client to expose the
// same shape.
//
// Keeping the interface gRPC-shaped (not connect-shaped) matches the
// project convention: gRPC services own the API contract and return
// status.Error(codes.X, ...); transport bindings (Connect handler in
// the example, this CLI adapter) convert at their boundary. See
// internal/services/inference.go + internal/web/server/connect.go
// for the inference-plane equivalent.
type provisionerClient interface {
	CreateInstance(context.Context, *provisionerv1.CreateInstanceRequest) (*provisionerv1.CreateInstanceResponse, error)
	DestroyInstance(context.Context, *provisionerv1.DestroyInstanceRequest) (*provisionerv1.DestroyInstanceResponse, error)
	DescribeInstance(context.Context, *provisionerv1.DescribeInstanceRequest) (*provisionerv1.DescribeInstanceResponse, error)
	ListInstances(context.Context, *provisionerv1.ListInstancesRequest) (*provisionerv1.ListInstancesResponse, error)
	WaitForInstanceReady(context.Context, *provisionerv1.WaitForInstanceReadyRequest) (*provisionerv1.WaitForInstanceReadyResponse, error)
	GetInstanceSSHKey(context.Context, *provisionerv1.GetInstanceSSHKeyRequest) (*provisionerv1.GetInstanceSSHKeyResponse, error)
}

// connectProvisionerClient adapts the generated connect-rpc client to
// satisfy the gRPC-shape provisionerClient interface above. Wraps each
// call: pack the request into a connect.Request envelope, unpack the
// connect.Response.Msg on return. Errors flow through unchanged --
// connect-rpc surfaces gRPC status codes via *connect.Error which the
// caller can errors.As-extract if needed.
type connectProvisionerClient struct {
	c provisionerv1connect.ProvisionerServiceClient
}

func (a *connectProvisionerClient) CreateInstance(ctx context.Context, req *provisionerv1.CreateInstanceRequest) (*provisionerv1.CreateInstanceResponse, error) {
	resp, err := a.c.CreateInstance(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (a *connectProvisionerClient) DestroyInstance(ctx context.Context, req *provisionerv1.DestroyInstanceRequest) (*provisionerv1.DestroyInstanceResponse, error) {
	resp, err := a.c.DestroyInstance(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (a *connectProvisionerClient) DescribeInstance(ctx context.Context, req *provisionerv1.DescribeInstanceRequest) (*provisionerv1.DescribeInstanceResponse, error) {
	resp, err := a.c.DescribeInstance(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (a *connectProvisionerClient) ListInstances(ctx context.Context, req *provisionerv1.ListInstancesRequest) (*provisionerv1.ListInstancesResponse, error) {
	resp, err := a.c.ListInstances(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (a *connectProvisionerClient) WaitForInstanceReady(ctx context.Context, req *provisionerv1.WaitForInstanceReadyRequest) (*provisionerv1.WaitForInstanceReadyResponse, error) {
	resp, err := a.c.WaitForInstanceReady(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (a *connectProvisionerClient) GetInstanceSSHKey(ctx context.Context, req *provisionerv1.GetInstanceSSHKeyRequest) (*provisionerv1.GetInstanceSSHKeyResponse, error) {
	resp, err := a.c.GetInstanceSSHKey(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
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
// ~/.iplane (creating it implicitly via file.Open). Fails fast when
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
		return &connectProvisionerClient{
			c: provisionerv1connect.NewProvisionerServiceClient(http.DefaultClient, instanceServiceURL),
		}, nil
	}

	dir, err := resolveStateDir()
	if err != nil {
		return nil, err
	}
	store, err := file.Open(dir, instanceOperatorID)
	if err != nil {
		return nil, fmt.Errorf("open state store: %w", err)
	}
	// Acquire the lifetime lock so a running iplane serve daemon
	// surfaces as a clear error instead of a silent hang. The release
	// func is dropped on purpose: the CLI process exits within
	// seconds, and the kernel reclaims the FD (and therefore the
	// flock) on exit. The PID sidecar is left behind but is harmless
	// because LockForLifetime checks the flock itself, not the
	// sidecar; sidecar is informational only.
	if _, err := store.LockForLifetime(); err != nil {
		var held *file.ErrLockHeld
		if errors.As(err, &held) {
			if held.HolderPID != 0 {
				return nil, fmt.Errorf("iplane serve is running at PID %d (state %s); pass --service-url to route through it or stop the daemon", held.HolderPID, held.Path)
			}
			return nil, fmt.Errorf("state directory %q is locked by another process; pass --service-url to route through it or stop the holder", held.Path)
		}
		return nil, fmt.Errorf("acquire state lock: %w", err)
	}
	return buildLocalService(store, instanceOperatorID)
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
