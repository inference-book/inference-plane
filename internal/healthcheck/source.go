package healthcheck

import (
	"context"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
)

// serviceLike is the slice of *provisioners.Service the source +
// quarantiner adapters need. Narrow interface so tests can substitute
// a fake without standing up a real Service.
type serviceLike interface {
	ListDeployments(ctx context.Context, req *provisionerv1.ListDeploymentsRequest) (*provisionerv1.ListDeploymentsResponse, error)
	Quarantine(deployID, instanceID string) error
	Restore(deployID, instanceID string) error
}

// ServiceAdapter is a DeploymentSource + Quarantiner backed by an
// in-process *provisioners.Service. cmd/iplane/cmd/serve.go uses
// this to wire the Runner into the daemon without going through
// gRPC -- this is control-plane code calling control-plane code.
type ServiceAdapter struct {
	svc serviceLike
}

// NewServiceAdapter wraps a Service for the Runner. Returns both
// halves of the contract -- the same instance satisfies
// DeploymentSource and Quarantiner so the daemon registers it once.
func NewServiceAdapter(svc *provisioners.Service) *ServiceAdapter {
	return &ServiceAdapter{svc: svc}
}

// Snapshot lists RUNNING deployments and reduces each to its
// (instance_id, engine_endpoint, quarantined?) replica set. Empty
// or destroyed deployments are omitted; non-RUNNING deployments
// are filtered server-side via the ListDeployments state filter.
//
// Uses EffectiveInstanceIDs / EffectiveEndpoints so single-instance
// (Beat 1+2) deployments are visible to the health loop too --
// their length-1 replica set gets probed exactly like a multi-
// instance deployment.
func (a *ServiceAdapter) Snapshot() []DeploymentSnapshot {
	resp, err := a.svc.ListDeployments(context.Background(), &provisionerv1.ListDeploymentsRequest{
		State: provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
	})
	if err != nil || resp == nil {
		return nil
	}
	deps := resp.GetDeployments()
	if len(deps) == 0 {
		return nil
	}
	out := make([]DeploymentSnapshot, 0, len(deps))
	for _, dep := range deps {
		ids := provisioners.EffectiveInstanceIDs(dep)
		eps := provisioners.EffectiveEndpoints(dep)
		if len(eps) == 0 {
			continue
		}
		unhealthy := make(map[string]struct{}, len(dep.GetUnhealthyInstanceIds()))
		for _, id := range dep.GetUnhealthyInstanceIds() {
			unhealthy[id] = struct{}{}
		}
		snap := DeploymentSnapshot{DeployID: dep.GetId()}
		for i, ep := range eps {
			instanceID := ""
			if i < len(ids) {
				instanceID = ids[i]
			}
			_, q := unhealthy[instanceID]
			snap.Replicas = append(snap.Replicas, ReplicaSnapshot{
				InstanceID:  instanceID,
				Endpoint:    ep,
				Quarantined: q,
			})
		}
		out = append(out, snap)
	}
	return out
}

// Quarantine delegates to the underlying service. Errors propagate
// to the runner, which logs them at WARN -- the next tick will
// retry naturally.
func (a *ServiceAdapter) Quarantine(deployID, instanceID string) error {
	return a.svc.Quarantine(deployID, instanceID)
}

// Restore delegates to the underlying service.
func (a *ServiceAdapter) Restore(deployID, instanceID string) error {
	return a.svc.Restore(deployID, instanceID)
}
