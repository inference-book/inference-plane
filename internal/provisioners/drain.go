package provisioners

import (
	"context"
	"fmt"
	"time"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// DefaultDrainTimeout is how long a drain waits for in-flight work to finish
// before releasing the hardware.
//
// Two minutes because the work being waited on is token generation: a long
// streaming completion can run well past the few seconds an HTTP service
// would budget. Too short and a drain cuts off responses mid-stream, which is
// the failure the verb exists to avoid; too long and an operator reclaiming
// capacity waits on nothing.
const DefaultDrainTimeout = 2 * time.Minute

// DrainOptions tunes a drain.
type DrainOptions struct {
	// Timeout is how long to let in-flight work finish. Zero means
	// DefaultDrainTimeout.
	Timeout time.Duration
	// Force skips the wait entirely. In-flight requests on the drained
	// replicas see their connections cut.
	Force bool
}

// DrainReplicas takes replicas out of service gracefully: quarantine, then
// wait. It does NOT release hardware; the caller decides what release means.
//
// The split is deliberate and is what lets two callers share this. Fleet
// drain (issue 205) quarantines every replica of a member and then destroys
// the whole deployment. Scale-down (issue 145) quarantines a tail subset and
// then destroys just those instances, repacking the surviving slots. The
// graceful part is identical; only the release differs, so only the release
// lives in the callers.
//
// Quarantine is the same mechanism the health poller uses, which is what
// makes this cheap: the router already refuses to dispatch to a quarantined
// replica, so "stop new work landing here" needs no new data-path concept.
//
// The wait is a fixed grace period rather than a poll for in-flight reaching
// zero. The control plane has no trustworthy count to poll: in-flight lives
// in the router, and the only path that carries it upward is the engine
// registry, which is deliberately lagging. Draining early on a stale zero
// would cut live requests, which is precisely what the verb promises not to
// do. A bounded wait is what Kubernetes does for the same reason, and issue
// 145 already specified a configurable max-wait rather than a count.
//
// Returns an error only if quarantine itself fails. A drain whose grace
// period elapses with work still running is not an error: the timeout is the
// operator's stated budget, and expiring it is the documented outcome.
func (s *Service) DrainReplicas(ctx context.Context, deployID string, instanceIDs []string, opts DrainOptions) error {
	if deployID == "" {
		return fmt.Errorf("drain: deployment id is required")
	}
	if len(instanceIDs) == 0 {
		return fmt.Errorf("drain: no replicas to drain for deployment %q", deployID)
	}

	for _, instID := range instanceIDs {
		if instID == "" {
			continue
		}
		if err := s.Quarantine(deployID, instID); err != nil {
			return fmt.Errorf("drain: quarantine %s: %w", instID, err)
		}
	}

	if opts.Force {
		return nil
	}
	wait := opts.Timeout
	if wait <= 0 {
		wait = DefaultDrainTimeout
	}
	select {
	case <-ctx.Done():
		// Caller gave up (client disconnect, daemon shutdown). The replicas
		// stay quarantined, which is the safe half-state: no new work lands
		// on them and nothing has been destroyed.
		return ctx.Err()
	case <-time.After(wait):
		return nil
	}
}

// DrainAndDestroyDeployment is the fleet-drain path: drain every replica of a
// deployment, then release all of it.
//
// Releasing a distributed member releases every node in its group, and doing
// that as one action is the difference between a clean reclaim and N separate
// teardowns the operator has to sequence by hand. Teardown itself is
// DestroyDeployment, which since issue 228 actually releases every replica
// rather than only the singular instance_id.
func (s *Service) DrainAndDestroyDeployment(ctx context.Context, deployID string, opts DrainOptions) ([]string, error) {
	file, err := s.store.Read()
	if err != nil {
		return nil, err
	}
	dep, ok := file.Deployments[deployID]
	if !ok {
		return nil, fmt.Errorf("no deployment with id %q", deployID)
	}
	instanceIDs := EffectiveInstanceIDs(dep)
	if err := s.DrainReplicas(ctx, deployID, instanceIDs, opts); err != nil {
		return nil, err
	}
	if _, err := s.DestroyDeployment(ctx, &provisionerv1.DestroyDeploymentRequest{Id: deployID}); err != nil {
		return nil, err
	}
	return instanceIDs, nil
}
