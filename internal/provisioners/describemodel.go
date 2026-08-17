package provisioners

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/modelstores"
)

// DescribeModel reports the shape a model spec resolves to.
//
// The work is the model store's; this is the wire surface over it, and
// the surface exists because the credential the store needs lives in the
// daemon's environment. A CLI host resolving the same spec in its own
// process reports a gated model as unreadable while the daemon reads it
// perfectly well, which is the defect that shipped twice already, in the
// capacity search (#304) and in the pin registry (#307).
//
// Reading a model's config takes no state lock, because it touches no
// state. That matters for the same reason it mattered for `model ls`: an
// operator sizing a deployment is usually doing it while a control plane
// is running, and a verb that refuses then is a verb that refuses when it
// is wanted.
func (s *Service) DescribeModel(ctx context.Context, req *provisionerv1.DescribeModelRequest) (*provisionerv1.DescribeModelResponse, error) {
	if req.GetModelSpec() == "" {
		return nil, status.Error(codes.InvalidArgument, "model_spec is required")
	}

	// Optional capability, asserted rather than assumed. A store with no
	// hub behind it cannot answer, and Unimplemented says so. Returning
	// an empty architecture instead would be a model with no layers,
	// which computes a KV cache of zero and therefore fits anything.
	src, ok := s.modelStore.(modelstores.ArchitectureSource)
	if !ok {
		return nil, status.Error(codes.Unimplemented, noArchitectureSourceMsg)
	}

	resp, err := src.Architecture(ctx, req)
	if err != nil {
		// A decorator satisfies the interface in order to forward it, so
		// past one the assertion above stops distinguishing "cannot
		// report a shape" from "cannot report this model's". The
		// sentinel restores that, and it has to be checked here because
		// the two codes send an operator to different places: one to
		// their store configuration, the other to their model spec.
		if errors.Is(err, modelstores.ErrNoArchitectureSource) {
			return nil, status.Error(codes.Unimplemented, noArchitectureSourceMsg)
		}
		// InvalidArgument rather than Internal: every failure the store
		// returns here is about the spec or about what the hub publishes
		// for it, and both are the caller's to fix.
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return resp, nil
}

// noArchitectureSourceMsg is shared by the two paths that reach the same
// conclusion, so an operator sees one message whether the store declined
// the assertion or a decorator reported the store behind it could not
// answer.
const noArchitectureSourceMsg = "the configured model store cannot report a model's architecture; " +
	"model validation is off or the store is a pass-through, so there is no hub to ask"
