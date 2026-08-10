package engines

import (
	"context"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// NodeIdentity is what the control plane knows about an engine's node that
// the engine cannot know about itself.
//
// Both fields look like things the agent should have reported, and neither
// can be. The provider's machine id is unreadable from inside a container
// (docs/design/0007 finding 4), and the endpoint is derived from a pod id
// that does not exist until the create call returns, so it cannot be written
// into the env of the container that call is creating. Deploy-time injection
// carries a correlation key; this carries the rest.
type NodeIdentity struct {
	HostID   string
	Provider string
	Endpoint string
}

// Locator resolves the identity above from an engine id.
//
// An interface supplied by the daemon rather than a direct dependency, for
// the same reason Drainer is: the registry knows what engines exist, not how
// the hardware under them was rented. It also keeps the dependency pointing
// one way, since this package already imports the provisioner service and
// the service must not import it back.
type Locator interface {
	// LocateEngine returns what the control plane recorded for engineID.
	// found is false for an engine the control plane did not provision,
	// which is a legitimate case rather than an error.
	LocateEngine(ctx context.Context, engineID string) (id NodeIdentity, found bool, err error)
}

// LocatorFunc adapts a function to Locator, so the daemon can wire a method
// on its provisioner service without declaring a named type.
type LocatorFunc func(ctx context.Context, engineID string) (NodeIdentity, bool, error)

// LocateEngine implements Locator.
func (f LocatorFunc) LocateEngine(ctx context.Context, engineID string) (NodeIdentity, bool, error) {
	return f(ctx, engineID)
}

// WithLocator supplies the enrichment source. Without it, registrations are
// stored exactly as reported, which is the right behaviour for a control
// plane that provisioned nothing.
func WithLocator(l Locator) AdapterOption {
	return func(a *ConnectAdapter) { a.locator = l }
}

// enrich fills the fields the agent could not know, and only those.
//
// Reported values always win. An agent that knows its host id (a future
// provider that exposes one in the container, or an operator running the
// agent by hand with --host-id) is a better source than our record of what
// we rented, and silently overwriting it would hide a genuine disagreement
// between what we think we provisioned and what is actually running.
//
// Enrichment failure is not registration failure. A member that registers
// with a blank host id is visible and debuggable; a member that fails to
// register because a lookup errored is invisible, and the whole point of
// the channel is to stop losing members.
func (a *ConnectAdapter) enrich(ctx context.Context, e *provisionerv1.Engine) {
	if a.locator == nil || e.GetId() == "" {
		return
	}

	// A single-node span is the only shape enrichment can complete. A
	// multi-node engine reports its own composition, because the group is
	// the thing that knows which nodes joined it, and guessing the rest from
	// one slot's record would invent membership.
	span := e.GetSpan()
	if len(span) > 1 {
		return
	}

	needHost := len(span) == 0 || span[0].GetHostId() == ""
	if !needHost && e.GetEndpoint() != "" {
		// Nothing to fill. Skip the lookup rather than reading state on every
		// renewal: renewals outnumber first registrations by the lease
		// divisor and arrive already complete.
		return
	}

	id, found, err := a.locator.LocateEngine(ctx, e.GetId())
	if err != nil || !found {
		return
	}

	if e.GetEndpoint() == "" {
		e.Endpoint = id.Endpoint
	}
	if len(span) == 0 {
		e.Span = []*provisionerv1.EngineNode{{}}
		span = e.Span
	}
	if span[0].GetHostId() == "" {
		span[0].HostId = id.HostID
	}
	if span[0].GetProvider() == "" {
		span[0].Provider = id.Provider
	}
}
