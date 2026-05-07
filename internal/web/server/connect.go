package server

import (
	"context"

	"connectrpc.com/connect"

	inferencev1 "github.com/inference-book/inference-plane/gen/go/inferenceplane/v1"
)

// ConnectInferenceServiceAdapter adapts the gRPC InferenceServiceClient
// to the connect-rpc InferenceServiceHandler interface. Each method
// unwraps the connect.Request, calls the gRPC client (which dispatches
// to the in-process gRPC server), and wraps the response back.
//
// Errors propagate as gRPC status errors; connect-rpc maps them to
// the right Connect error code automatically -- no manual translation
// needed in the adapter.
type ConnectInferenceServiceAdapter struct {
	client inferencev1.InferenceServiceClient
}

// NewConnectInferenceServiceAdapter constructs the adapter.
func NewConnectInferenceServiceAdapter(client inferencev1.InferenceServiceClient) *ConnectInferenceServiceAdapter {
	return &ConnectInferenceServiceAdapter{client: client}
}

func (a *ConnectInferenceServiceAdapter) Complete(
	ctx context.Context,
	req *connect.Request[inferencev1.CompleteRequest],
) (*connect.Response[inferencev1.CompleteResponse], error) {
	resp, err := a.client.Complete(ctx, req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

func (a *ConnectInferenceServiceAdapter) ChatComplete(
	ctx context.Context,
	req *connect.Request[inferencev1.ChatCompleteRequest],
) (*connect.Response[inferencev1.ChatCompleteResponse], error) {
	resp, err := a.client.ChatComplete(ctx, req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

// ConnectHealthServiceAdapter adapts the gRPC HealthServiceClient to
// the connect-rpc HealthServiceHandler interface.
type ConnectHealthServiceAdapter struct {
	client inferencev1.HealthServiceClient
}

// NewConnectHealthServiceAdapter constructs the adapter.
func NewConnectHealthServiceAdapter(client inferencev1.HealthServiceClient) *ConnectHealthServiceAdapter {
	return &ConnectHealthServiceAdapter{client: client}
}

func (a *ConnectHealthServiceAdapter) Check(
	ctx context.Context,
	req *connect.Request[inferencev1.CheckRequest],
) (*connect.Response[inferencev1.CheckResponse], error) {
	resp, err := a.client.Check(ctx, req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}
