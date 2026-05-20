package provisioners

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// ConnectProvisionerAdapter wraps a gRPC ProvisionerServiceServer
// implementation (typically *Service) and exposes the
// provisionerv1connect.ProvisionerServiceHandler interface. Each
// method unwraps the connect.Request envelope, calls the gRPC server
// directly, and wraps the response back.
//
// Errors flow through unchanged. status.Error(codes.X, ...) returned by
// the gRPC server is recognized by connect-rpc's automatic translation
// (the codes are 1:1 with connect.Code values), so callers on the wire
// see the correct gRPC / Connect error code without manual mapping.
//
// This adapter lives in the provisioners package (rather than under
// internal/web/server like the inference adapter) because the
// in-process examples + tests need to mount the gRPC service via a
// Connect handler without a dependency on the web server layer.
type ConnectProvisionerAdapter struct {
	svc provisionerv1.ProvisionerServiceServer
}

// NewConnectProvisionerAdapter constructs the adapter. Pass a
// *Service or any other gRPC-shape impl.
func NewConnectProvisionerAdapter(svc provisionerv1.ProvisionerServiceServer) *ConnectProvisionerAdapter {
	return &ConnectProvisionerAdapter{svc: svc}
}

func (a *ConnectProvisionerAdapter) CreateInstance(ctx context.Context, req *connect.Request[provisionerv1.CreateInstanceRequest]) (*connect.Response[provisionerv1.CreateInstanceResponse], error) {
	resp, err := a.svc.CreateInstance(ctx, req.Msg)
	if err != nil {
		return nil, statusToConnectErr(err)
	}
	return connect.NewResponse(resp), nil
}

func (a *ConnectProvisionerAdapter) DestroyInstance(ctx context.Context, req *connect.Request[provisionerv1.DestroyInstanceRequest]) (*connect.Response[provisionerv1.DestroyInstanceResponse], error) {
	resp, err := a.svc.DestroyInstance(ctx, req.Msg)
	if err != nil {
		return nil, statusToConnectErr(err)
	}
	return connect.NewResponse(resp), nil
}

func (a *ConnectProvisionerAdapter) DescribeInstance(ctx context.Context, req *connect.Request[provisionerv1.DescribeInstanceRequest]) (*connect.Response[provisionerv1.DescribeInstanceResponse], error) {
	resp, err := a.svc.DescribeInstance(ctx, req.Msg)
	if err != nil {
		return nil, statusToConnectErr(err)
	}
	return connect.NewResponse(resp), nil
}

func (a *ConnectProvisionerAdapter) ListInstances(ctx context.Context, req *connect.Request[provisionerv1.ListInstancesRequest]) (*connect.Response[provisionerv1.ListInstancesResponse], error) {
	resp, err := a.svc.ListInstances(ctx, req.Msg)
	if err != nil {
		return nil, statusToConnectErr(err)
	}
	return connect.NewResponse(resp), nil
}

// ConnectDeploymentAdapter wraps a gRPC DeploymentServiceServer
// implementation (typically *Service) and exposes the
// provisionerv1connect.DeploymentServiceHandler interface. Mirrors
// ConnectProvisionerAdapter for the deployment surface.
//
// All five methods are wired even though the underlying Service stubs
// return Unimplemented in this PR -- when the Phase 2 executor PR
// lands, the adapter needs no changes.
type ConnectDeploymentAdapter struct {
	svc provisionerv1.DeploymentServiceServer
}

// NewConnectDeploymentAdapter constructs the adapter.
func NewConnectDeploymentAdapter(svc provisionerv1.DeploymentServiceServer) *ConnectDeploymentAdapter {
	return &ConnectDeploymentAdapter{svc: svc}
}

func (a *ConnectDeploymentAdapter) CreateDeployment(ctx context.Context, req *connect.Request[provisionerv1.CreateDeploymentRequest]) (*connect.Response[provisionerv1.CreateDeploymentResponse], error) {
	resp, err := a.svc.CreateDeployment(ctx, req.Msg)
	if err != nil {
		return nil, statusToConnectErr(err)
	}
	return connect.NewResponse(resp), nil
}

func (a *ConnectDeploymentAdapter) DescribeDeployment(ctx context.Context, req *connect.Request[provisionerv1.DescribeDeploymentRequest]) (*connect.Response[provisionerv1.DescribeDeploymentResponse], error) {
	resp, err := a.svc.DescribeDeployment(ctx, req.Msg)
	if err != nil {
		return nil, statusToConnectErr(err)
	}
	return connect.NewResponse(resp), nil
}

func (a *ConnectDeploymentAdapter) ListDeployments(ctx context.Context, req *connect.Request[provisionerv1.ListDeploymentsRequest]) (*connect.Response[provisionerv1.ListDeploymentsResponse], error) {
	resp, err := a.svc.ListDeployments(ctx, req.Msg)
	if err != nil {
		return nil, statusToConnectErr(err)
	}
	return connect.NewResponse(resp), nil
}

func (a *ConnectDeploymentAdapter) DestroyDeployment(ctx context.Context, req *connect.Request[provisionerv1.DestroyDeploymentRequest]) (*connect.Response[provisionerv1.DestroyDeploymentResponse], error) {
	resp, err := a.svc.DestroyDeployment(ctx, req.Msg)
	if err != nil {
		return nil, statusToConnectErr(err)
	}
	return connect.NewResponse(resp), nil
}

// WatchDeployment routes the Connect server-streaming call to the
// gRPC server-streaming method via connectStreamBridge -- a generic
// adapter that satisfies grpc.ServerStreamingServer[T] on top of a
// *connect.ServerStream[T]. The underlying gRPC stub returns
// Unimplemented in this PR; when the Phase 2 executor lands and emits
// real events, no changes to this method or the bridge are needed.
//
// TODO(servicekit#30): connectStreamBridge is copied from
// lilbattle/web/server/connectbridge.go and is a candidate for stack
// pushdown into servicekit. See https://github.com/panyam/servicekit/issues/30;
// the migration of both iplane and lilbattle to a shared servicekit
// primitive is tracked there. Until then this is acknowledged
// duplication.
func (a *ConnectDeploymentAdapter) WatchDeployment(ctx context.Context, req *connect.Request[provisionerv1.WatchDeploymentRequest], stream *connect.ServerStream[provisionerv1.DeploymentStateChangedEvent]) error {
	return a.svc.WatchDeployment(req.Msg, &connectStreamBridge[provisionerv1.DeploymentStateChangedEvent]{
		ctx:           ctx,
		connectStream: stream,
	})
}

// connectStreamBridge converts a *connect.ServerStream[T] into something
// that satisfies grpc.ServerStreamingServer[T] (which is what
// generated gRPC server-streaming methods expect). Generic over the
// message type so one bridge handles every streaming RPC.
//
// Lifted from lilbattle/web/server/connectbridge.go pending pushdown
// to servicekit. Header / trailer metadata are no-op in v0.1 (the
// stubs never set them); when the executor needs real metadata
// propagation, that goes in alongside the servicekit pushdown.
type connectStreamBridge[T any] struct {
	ctx           context.Context
	connectStream *connect.ServerStream[T]
}

func (b *connectStreamBridge[T]) Send(msg *T) error {
	return b.connectStream.Send(msg)
}

func (b *connectStreamBridge[T]) Context() context.Context { return b.ctx }

func (b *connectStreamBridge[T]) SendMsg(m any) error {
	if msg, ok := m.(*T); ok {
		return b.connectStream.Send(msg)
	}
	return fmt.Errorf("connectStreamBridge: invalid message type %T", m)
}

func (b *connectStreamBridge[T]) RecvMsg(m any) error {
	return fmt.Errorf("connectStreamBridge: RecvMsg not supported for server streaming")
}

func (b *connectStreamBridge[T]) SetHeader(metadata.MD) error  { return nil }
func (b *connectStreamBridge[T]) SendHeader(metadata.MD) error { return nil }
func (b *connectStreamBridge[T]) SetTrailer(metadata.MD)       {}

// statusToConnectErr translates a gRPC status.Error (which is what the
// gRPC service returns) into a *connect.Error with the matching code.
// Without this translation, connect-rpc receives a non-*connect.Error
// from the handler and wraps it as CodeUnknown -- callers downstream
// see "unknown" instead of "not_found" (or whichever the underlying
// code was).
//
// The code mapping is 1:1 by name (NotFound -> CodeNotFound, etc.) so
// the function is dominated by the switch table; nothing about the
// payload is lost.
func statusToConnectErr(err error) error {
	if err == nil {
		return nil
	}
	// Already a connect.Error (returned from a downstream connect call,
	// or from helper code that produced one explicitly).
	var ce *connect.Error
	if errors.As(err, &ce) {
		return err
	}
	st, ok := status.FromError(err)
	if !ok {
		// Not a gRPC status -- let connect wrap as Unknown (its
		// default). Returning err unchanged preserves the message.
		return err
	}
	return connect.NewError(grpcToConnectCode(st.Code()), errors.New(st.Message()))
}

func grpcToConnectCode(c codes.Code) connect.Code {
	switch c {
	case codes.Canceled:
		return connect.CodeCanceled
	case codes.Unknown:
		return connect.CodeUnknown
	case codes.InvalidArgument:
		return connect.CodeInvalidArgument
	case codes.DeadlineExceeded:
		return connect.CodeDeadlineExceeded
	case codes.NotFound:
		return connect.CodeNotFound
	case codes.AlreadyExists:
		return connect.CodeAlreadyExists
	case codes.PermissionDenied:
		return connect.CodePermissionDenied
	case codes.ResourceExhausted:
		return connect.CodeResourceExhausted
	case codes.FailedPrecondition:
		return connect.CodeFailedPrecondition
	case codes.Aborted:
		return connect.CodeAborted
	case codes.OutOfRange:
		return connect.CodeOutOfRange
	case codes.Unimplemented:
		return connect.CodeUnimplemented
	case codes.Internal:
		return connect.CodeInternal
	case codes.Unavailable:
		return connect.CodeUnavailable
	case codes.DataLoss:
		return connect.CodeDataLoss
	case codes.Unauthenticated:
		return connect.CodeUnauthenticated
	default:
		return connect.CodeUnknown
	}
}
