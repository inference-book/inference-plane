package services

import (
	"context"
	"time"

	inferencev1 "github.com/inference-book/inference-plane/gen/go/inferenceplane/v1"
	"github.com/inference-book/inference-plane/internal/backends"
	"github.com/inference-book/inference-plane/internal/metrics"
)

// HealthServer implements inferencev1.HealthServiceServer. Probes the
// configured backend and aggregates the result into a three-state
// response (SERVING / DEGRADED / NOT_SERVING).
//
// Connect adapters in internal/web/server/ expose this over Connect-RPC;
// grpc-gateway exposes the same Check method as GET /health (the
// google.api.http annotation handles the URL mapping).
type HealthServer struct {
	inferencev1.UnimplementedHealthServiceServer
	backend      backends.Backend
	metrics      *metrics.Recorder
	probeTimeout time.Duration
}

// NewHealthServer constructs a HealthServer that probes the given
// backend. The probe timeout caps how long a single probe can take --
// a slow backend never blocks the health endpoint past the operator's
// expectation of "this should respond quickly." metrics may be nil
// (no-op gauge updates) for tests that don't init telemetry.
func NewHealthServer(b backends.Backend, m *metrics.Recorder) *HealthServer {
	return &HealthServer{
		backend:      b,
		metrics:      m,
		probeTimeout: 2 * time.Second,
	}
}

// compile-time check that HealthServer satisfies the gRPC interface.
var _ inferencev1.HealthServiceServer = (*HealthServer)(nil)

// Check probes the backend's health. Returns SERVING when the backend's
// own /health returns nil; NOT_SERVING (with the backend's error message)
// otherwise. DEGRADED is reserved for future cases where the backend is
// reachable but reporting reduced capacity.
func (s *HealthServer) Check(ctx context.Context, _ *inferencev1.CheckRequest) (*inferencev1.CheckResponse, error) {
	probeCtx, cancel := context.WithTimeout(ctx, s.probeTimeout)
	defer cancel()

	err := s.backend.Health(probeCtx)
	s.metrics.SetBackendHealth(ctx, err == nil)

	if err != nil {
		return &inferencev1.CheckResponse{
			Status:  inferencev1.CheckResponse_STATUS_NOT_SERVING,
			Message: err.Error(),
		}, nil
	}

	return &inferencev1.CheckResponse{
		Status: inferencev1.CheckResponse_STATUS_SERVING,
	}, nil
}
