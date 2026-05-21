package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/grpc/metadata"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/gen/go/provisioner/v1/provisionerv1connect"
	"github.com/inference-book/inference-plane/internal/deployments/sshdocker"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/local"
	"github.com/inference-book/inference-plane/internal/provisioners/runpod"
	"github.com/inference-book/inference-plane/internal/provisioners/state"
	"github.com/inference-book/inference-plane/internal/sshkeys"
)

// Shared flags on the deployment group. The in-process state file is
// the same one `iplane instance` writes to (~/.iplane/state.json by
// default), so operators who mix the two verb groups in a session land
// on the same audit log without extra flag plumbing.
var (
	deploymentStateDir   string
	deploymentOperatorID string
	deploymentOutput     string
	deploymentServiceURL string
)

// deploymentClient is the gRPC-shaped surface every deployment
// subcommand calls. CreateDeployment / DescribeDeployment /
// ListDeployments / DestroyDeployment have the standard
// (ctx, *Req) → (*Resp, error) shape; *provisioners.Service satisfies
// these directly (it implements gen ProvisionerServiceServer +
// DeploymentServiceServer).
//
// WatchDeployment is server-streaming. To keep the interface single
// across both transports, it is exposed as a callback-driven method:
// the caller passes a handler invoked for each event; returning a
// non-nil error stops iteration (used by the `watch` and `wait`
// verbs to bail on terminal states).
type deploymentClient interface {
	CreateDeployment(context.Context, *provisionerv1.CreateDeploymentRequest) (*provisionerv1.CreateDeploymentResponse, error)
	DescribeDeployment(context.Context, *provisionerv1.DescribeDeploymentRequest) (*provisionerv1.DescribeDeploymentResponse, error)
	ListDeployments(context.Context, *provisionerv1.ListDeploymentsRequest) (*provisionerv1.ListDeploymentsResponse, error)
	DestroyDeployment(context.Context, *provisionerv1.DestroyDeploymentRequest) (*provisionerv1.DestroyDeploymentResponse, error)
	WatchDeployment(context.Context, *provisionerv1.WatchDeploymentRequest, func(*provisionerv1.DeploymentStateChangedEvent) error) error
}

// inProcessDeploymentClient bridges the gRPC server-stream signature
// the Service exposes to the callback-shape the CLI uses. The Service
// expects a grpc.ServerStream-shaped sender; we supply one that
// forwards each Send() into the caller's callback and returns early
// if the callback returns non-nil.
type inProcessDeploymentClient struct {
	svc *provisioners.Service
}

func (c *inProcessDeploymentClient) CreateDeployment(ctx context.Context, req *provisionerv1.CreateDeploymentRequest) (*provisionerv1.CreateDeploymentResponse, error) {
	return c.svc.CreateDeployment(ctx, req)
}

func (c *inProcessDeploymentClient) DescribeDeployment(ctx context.Context, req *provisionerv1.DescribeDeploymentRequest) (*provisionerv1.DescribeDeploymentResponse, error) {
	return c.svc.DescribeDeployment(ctx, req)
}

func (c *inProcessDeploymentClient) ListDeployments(ctx context.Context, req *provisionerv1.ListDeploymentsRequest) (*provisionerv1.ListDeploymentsResponse, error) {
	return c.svc.ListDeployments(ctx, req)
}

func (c *inProcessDeploymentClient) DestroyDeployment(ctx context.Context, req *provisionerv1.DestroyDeploymentRequest) (*provisionerv1.DestroyDeploymentResponse, error) {
	return c.svc.DestroyDeployment(ctx, req)
}

func (c *inProcessDeploymentClient) WatchDeployment(ctx context.Context, req *provisionerv1.WatchDeploymentRequest, onEvent func(*provisionerv1.DeploymentStateChangedEvent) error) error {
	stream := &callbackWatchStream{ctx: ctx, onEvent: onEvent}
	err := c.svc.WatchDeployment(req, stream)
	// callbackWatchStream uses stopIteration to break out of the
	// service loop without flagging an error to the caller.
	if errors.Is(err, errStopIteration) {
		return nil
	}
	return err
}

// connectDeploymentClient wraps the connect-rpc client so the CLI's
// remote mode satisfies the same deploymentClient interface as the
// in-process path. WatchDeployment iterates the server-stream and
// invokes the callback per event.
type connectDeploymentClient struct {
	c provisionerv1connect.DeploymentServiceClient
}

func (a *connectDeploymentClient) CreateDeployment(ctx context.Context, req *provisionerv1.CreateDeploymentRequest) (*provisionerv1.CreateDeploymentResponse, error) {
	resp, err := a.c.CreateDeployment(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (a *connectDeploymentClient) DescribeDeployment(ctx context.Context, req *provisionerv1.DescribeDeploymentRequest) (*provisionerv1.DescribeDeploymentResponse, error) {
	resp, err := a.c.DescribeDeployment(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (a *connectDeploymentClient) ListDeployments(ctx context.Context, req *provisionerv1.ListDeploymentsRequest) (*provisionerv1.ListDeploymentsResponse, error) {
	resp, err := a.c.ListDeployments(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (a *connectDeploymentClient) DestroyDeployment(ctx context.Context, req *provisionerv1.DestroyDeploymentRequest) (*provisionerv1.DestroyDeploymentResponse, error) {
	resp, err := a.c.DestroyDeployment(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (a *connectDeploymentClient) WatchDeployment(ctx context.Context, req *provisionerv1.WatchDeploymentRequest, onEvent func(*provisionerv1.DeploymentStateChangedEvent) error) error {
	stream, err := a.c.WatchDeployment(ctx, connect.NewRequest(req))
	if err != nil {
		return err
	}
	defer stream.Close()
	for stream.Receive() {
		if err := onEvent(stream.Msg()); err != nil {
			if errors.Is(err, errStopIteration) {
				return nil
			}
			return err
		}
	}
	return stream.Err()
}

// deploymentCmd is the `iplane deployment` cobra group. Subcommands
// attach via init() and inherit its persistent flags. Mirrors the
// transport-mode split documented on `instanceCmd`: in-process when
// --service-url is empty, remote connect-rpc when set.
var deploymentCmd = &cobra.Command{
	Use:     "deployment",
	Aliases: []string{"deploy"},
	Short:   "Create / list / describe / destroy iplane deployments",
	Long: `Push the engine container to a provisioned instance and operate it.

A deployment binds (instance, image, model) -> a long-running container
on the target instance, reachable via the engine endpoint surfaced
on DescribeDeployment. The control plane drives the state machine
(STARTING -> CONFIGURING -> RUNNING / FAILED) and exposes it via
the streaming WatchDeployment.

The default transport is in-process: this binary opens the state file
under flock, instantiates the SSH+docker executor in-process, and
calls Service methods directly. Pass --service-url to forward to a
running iplane serve instead.

State-changing subcommands accept --dry-run to preview the action.`,
}

func init() {
	rootCmd.AddCommand(deploymentCmd)

	pf := deploymentCmd.PersistentFlags()
	pf.StringVar(&deploymentStateDir, "state-dir", "",
		`directory holding state.json + .lock (default ~/.iplane; ignored when --service-url is set)`)
	pf.StringVar(&deploymentOperatorID, "operator", "default",
		`operator id stamped on deployments (v0.1 single-operator default)`)
	pf.StringVar(&deploymentOutput, "output", "table",
		`output format: table | json`)
	pf.StringVar(&deploymentServiceURL, "service-url", os.Getenv("IPLANE_SERVICE_URL"),
		`when set, forward to a running iplane serve at this URL (e.g. http://localhost:9091)`)
}

// resolveDeploymentStateDir returns the explicit --state-dir if set,
// else ~/.iplane (same default as instance verbs).
func resolveDeploymentStateDir() (string, error) {
	if deploymentStateDir != "" {
		return deploymentStateDir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir for default state path: %w", err)
	}
	return filepath.Join(home, ".iplane"), nil
}

// buildDeploymentClient returns the deploymentClient subcommands
// should call. Dispatch on --service-url matches buildClient() in
// instance.go: empty -> in-process Service with the same provider
// wiring + SSH+docker executor the instance verbs use; non-empty ->
// connect-rpc client dialing the URL.
//
// The in-process Service is constructed identically to instance.go's
// path so both verb groups see the same state file, key store, and
// executor configuration on a single iplane invocation.
func buildDeploymentClient() (deploymentClient, error) {
	if deploymentServiceURL != "" {
		return &connectDeploymentClient{
			c: provisionerv1connect.NewDeploymentServiceClient(http.DefaultClient, deploymentServiceURL),
		}, nil
	}

	dir, err := resolveDeploymentStateDir()
	if err != nil {
		return nil, err
	}
	store, err := state.Open(dir, deploymentOperatorID)
	if err != nil {
		return nil, fmt.Errorf("open state store: %w", err)
	}
	keyStore, err := sshkeys.New(sshkeys.WithDir(filepath.Join(dir, "keys")))
	if err != nil {
		return nil, fmt.Errorf("open ssh key store: %w", err)
	}

	providers := []provisioners.Provider{local.New()}
	if key := os.Getenv("RUNPOD_API_KEY"); key != "" {
		providers = append(providers, runpod.New(runpod.NewClient(key)))
	}

	svc := provisioners.New(providers, store, deploymentOperatorID,
		provisioners.WithKeyStore(keyStore),
		provisioners.WithDeploymentExecutor(sshdocker.NewExecutor()),
	)
	return &inProcessDeploymentClient{svc: svc}, nil
}

// errStopIteration is the sentinel onEvent callbacks return to break
// out of WatchDeployment cleanly (terminal state reached, e.g.). It is
// translated back to nil by the dispatchers above so the caller does
// not see a synthetic error on a normal completion.
var errStopIteration = errors.New("stop iteration")

// callbackWatchStream satisfies the gRPC ServerStreamingServer[T]
// interface the in-process Service expects, but forwards every Send()
// into a CLI-friendly callback. The bridge is deliberately minimal:
// no header / trailer / set-method calls -- the in-process path does
// not exercise them.
type callbackWatchStream struct {
	ctx     context.Context
	onEvent func(*provisionerv1.DeploymentStateChangedEvent) error
}

func (s *callbackWatchStream) Send(evt *provisionerv1.DeploymentStateChangedEvent) error {
	return s.onEvent(evt)
}

func (s *callbackWatchStream) Context() context.Context { return s.ctx }

// The methods below satisfy grpc.ServerStream interface requirements
// but are no-ops for the in-process bridge. The Service never calls
// them on the streaming path.
func (s *callbackWatchStream) SetHeader(_ metadata.MD) error  { return nil }
func (s *callbackWatchStream) SendHeader(_ metadata.MD) error { return nil }
func (s *callbackWatchStream) SetTrailer(_ metadata.MD)       {}
func (s *callbackWatchStream) SendMsg(_ any) error            { return nil }
func (s *callbackWatchStream) RecvMsg(_ any) error            { return io.EOF }
