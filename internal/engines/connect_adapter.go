package engines

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
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
	registry *Registry
}

// NewConnectAdapter wraps a Registry for the Connect mux.
func NewConnectAdapter(registry *Registry) *ConnectAdapter {
	return &ConnectAdapter{registry: registry}
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
	stored, err := a.registry.Register(req.Msg.GetEngine())
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
