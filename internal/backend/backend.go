package backend

import "context"

// Backend abstracts the inference engine. Any engine that exposes these
// three methods can plug into the control plane.
//
// The interface is deliberately narrow. Generate carries a context so the
// control plane can cancel in-flight inference when the client disconnects;
// Health surfaces the engine's view of its own readiness; Name labels metrics
// and logs so multi-backend deployments stay legible.
type Backend interface {
	// Generate runs inference for a single request. The supplied context
	// MUST be honored: cancellation propagates from the inbound HTTP
	// request and aborting the upstream call here is what prevents the
	// GPU from finishing work nobody is waiting for.
	Generate(ctx context.Context, req GenerateRequest) (GenerateResponse, error)

	// Health probes the engine. Returns nil for StatusHealthy. A non-nil
	// error indicates StatusUnhealthy; a degraded state surfaces through
	// the returned CheckResult attached via context (see internal/server/health).
	Health(ctx context.Context) error

	// Name identifies this backend instance in metrics labels and log fields.
	// Distinct from the model name -- one backend can serve multiple models.
	Name() string
}
