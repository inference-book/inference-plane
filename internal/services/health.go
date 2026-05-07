package services

import (
	"context"
	"time"

	"connectrpc.com/connect"

	inferencev1 "github.com/inference-book/inference-plane/gen/go/inferenceplane/v1"
	"github.com/inference-book/inference-plane/gen/go/inferenceplane/v1/inferenceplanev1connect"
	"github.com/inference-book/inference-plane/internal/backends"
)

// HealthServer implements inferenceplanev1connect.HealthServiceHandler.
// Probes the configured backend and aggregates the result into a
// three-state response (SERVING / DEGRADED / NOT_SERVING).
//
// The grpc-gateway annotation maps Check to GET /health, so external
// load balancers and Kubernetes readiness probes hit it as a plain
// HTTP endpoint with no JSON body.
type HealthServer struct {
	backend     backends.Backend
	probeTimeout time.Duration
}

// NewHealthServer constructs a HealthServer that probes the given
// backend. The probe timeout caps how long a single probe can take --
// a slow backend never blocks the health endpoint past the operator's
// expectations.
func NewHealthServer(b backends.Backend) *HealthServer {
	return &HealthServer{
		backend:      b,
		probeTimeout: 2 * time.Second,
	}
}

// compile-time check that HealthServer satisfies the connect handler.
var _ inferenceplanev1connect.HealthServiceHandler = (*HealthServer)(nil)

// Check probes the backend's health. Returns SERVING when the backend's
// own /health returns nil; NOT_SERVING (with the backend's error message)
// otherwise. DEGRADED is reserved for future cases where the backend is
// reachable but reporting reduced capacity (e.g., a vLLM instance with
// elevated KV cache pressure).
func (s *HealthServer) Check(
	ctx context.Context,
	req *connect.Request[inferencev1.CheckRequest],
) (*connect.Response[inferencev1.CheckResponse], error) {
	probeCtx, cancel := context.WithTimeout(ctx, s.probeTimeout)
	defer cancel()

	if err := s.backend.Health(probeCtx); err != nil {
		return connect.NewResponse(&inferencev1.CheckResponse{
			Status:  inferencev1.CheckResponse_STATUS_NOT_SERVING,
			Message: err.Error(),
		}), nil
	}

	return connect.NewResponse(&inferencev1.CheckResponse{
		Status: inferencev1.CheckResponse_STATUS_SERVING,
	}), nil
}
