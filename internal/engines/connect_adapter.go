package engines

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"
	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
)

// ConnectAdapter exposes a Registry as the generated
// provisionerv1connect.EngineRegistryServiceHandler, so engines reach the
// registry over the public HTTP surface on :8080.
//
// It has to be the public surface rather than the gRPC server, which binds
// 127.0.0.1:9090 and is an in-process implementation detail. The engine runs
// on a rented pod on the open internet, and it dials out, so nothing needs an
// inbound path to the pod.
//
// **Unauthenticated, deliberately and consistently.** Anyone who can reach
// the daemon can register a fleet member. That is the same exposure the
// deployment API already carries on the same port, and protecting one RPC
// while DestroyDeployment sits open next to it would suggest a boundary that
// does not exist. Tracked as a whole-surface concern, not a per-RPC one.
type ConnectAdapter struct {
	registry        *Registry
	drainer         Drainer
	locator         Locator
	maxDrainTimeout time.Duration
}

// AdapterOption configures a ConnectAdapter.
type AdapterOption func(*ConnectAdapter)

// NewConnectAdapter wraps a Registry for the Connect mux.
func NewConnectAdapter(registry *Registry, opts ...AdapterOption) *ConnectAdapter {
	a := &ConnectAdapter{registry: registry, maxDrainTimeout: provisioners.DefaultDrainTimeout}
	for _, o := range opts {
		o(a)
	}
	return a
}

// RegisterEngine records a registration or renewal and returns the stored
// engine with the lease the caller should renew within.
//
// A missing id is InvalidArgument, not an internal error: the id is the
// caller's to choose and its absence is a client bug. An engine claiming to
// be LOST is likewise rejected, since that state is the control plane's
// conclusion from an expired lease and not something an engine can assert
// about itself.
func (a *ConnectAdapter) RegisterEngine(
	ctx context.Context,
	req *connect.Request[provisionerv1.RegisterEngineRequest],
) (*connect.Response[provisionerv1.RegisterEngineResponse], error) {
	// Fill the fields the box could not know before storing, so what the
	// registry persists is the complete record rather than one the fleet
	// view has to re-join on every read.
	incoming := req.Msg.GetEngine()
	a.enrich(ctx, incoming)

	stored, err := a.registry.Register(incoming)
	if err != nil {
		if errors.Is(err, ErrNoID) {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&provisionerv1.RegisterEngineResponse{
		Engine:       stored,
		LeaseSeconds: a.registry.LeaseSeconds(),
	}), nil
}

// ListEngines returns known engines, LOST included, ordered by id.
func (a *ConnectAdapter) ListEngines(
	ctx context.Context,
	req *connect.Request[provisionerv1.ListEnginesRequest],
) (*connect.Response[provisionerv1.ListEnginesResponse], error) {
	out, err := a.registry.List(req.Msg.GetDeploymentId(), req.Msg.GetState())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&provisionerv1.ListEnginesResponse{Engines: out}), nil
}

// Drainer is the release half of a drain, supplied by the daemon so this
// package does not import the provisioner service.
//
// Kept as an interface rather than a concrete dependency because the registry
// is otherwise self-contained: it knows what engines exist, not how to
// destroy the hardware under them. The daemon wires the two together.
type Drainer interface {
	// DrainAndDestroyDeployment quarantines every replica of the deployment,
	// waits out the grace period, then releases all of it. Returns the
	// released instance ids.
	DrainAndDestroyDeployment(ctx context.Context, deployID string, opts provisioners.DrainOptions) ([]string, error)
}

// WithDrainer supplies the release half. Without it, DrainEngine reports
// Unimplemented rather than pretending to drain, which matters because a
// drain that silently does nothing looks identical to one that worked.
func WithDrainer(d Drainer) AdapterOption {
	return func(a *ConnectAdapter) { a.drainer = d }
}

// WithMaxDrainTimeout bounds what a caller may ask for, normally derived from
// the server's write timeout.
func WithMaxDrainTimeout(d time.Duration) AdapterOption {
	return func(a *ConnectAdapter) {
		if d > 0 {
			a.maxDrainTimeout = d
		}
	}
}

// DrainEngine takes a member out of service and releases its hardware.
//
// Ordering matters and is worth reading in the code: the member is marked
// DRAINING *before* the wait begins, so `fleet status` shows the drain in
// progress rather than the member looking healthy for two minutes and then
// vanishing.
//
// An engine with no deployment is rejected rather than half-drained. Such an
// engine registered itself without iplane provisioning it, so there are no
// replicas to quarantine and no hardware to release; marking it DRAINING and
// returning success would report work that did not happen.
func (a *ConnectAdapter) DrainEngine(
	ctx context.Context,
	req *connect.Request[provisionerv1.DrainEngineRequest],
) (*connect.Response[provisionerv1.DrainEngineResponse], error) {
	id := req.Msg.GetEngineId()
	if id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("engine_id is required"))
	}
	if a.drainer == nil {
		return nil, connect.NewError(connect.CodeUnimplemented,
			errors.New("this daemon has no drainer wired; fleet drain is unavailable"))
	}

	timeout := time.Duration(req.Msg.GetTimeoutSeconds()) * time.Second
	if !req.Msg.GetForce() && timeout > a.maxDrainTimeout {
		// Reject rather than truncate. Silently draining for less than asked
		// would cut in-flight work the operator explicitly budgeted for.
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(
			"timeout %s exceeds this daemon's %s limit (server.write_timeout_sec); "+
				"raise that or drain with --force", timeout, a.maxDrainTimeout))
	}

	eng, err := a.registry.Get(id)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if eng == nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("no engine with id %q", id))
	}
	deployID := eng.GetDeploymentId()
	if deployID == "" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
			"engine %q was not provisioned by iplane (no deployment), so there is nothing to "+
				"quarantine and no hardware to release; stop it where it runs", id))
	}

	if _, err := a.registry.SetState(id, provisionerv1.EngineState_ENGINE_STATE_DRAINING); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	released, err := a.drainer.DrainAndDestroyDeployment(ctx, deployID, provisioners.DrainOptions{
		Timeout: timeout,
		Force:   req.Msg.GetForce(),
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	// Left in DRAINING rather than forced to LOST. The pod is gone, so the
	// agent stops renewing and the sweeper reaches the same conclusion within
	// one lease -- via the ordinary path, with no special case, and the
	// intervening state reads truthfully as "we drained this on purpose"
	// rather than "this disappeared".
	final, _ := a.registry.Get(id)
	return connect.NewResponse(&provisionerv1.DrainEngineResponse{
		Engine:              final,
		ReleasedInstanceIds: released,
	}), nil
}
