// Package external implements a non-owning Provider: it registers a
// RUNNING replica pointing at an engine URL the operator runs
// themselves (on-prem vLLM, a k8s Service, or a local `iplane
// mock-engine`), rather than provisioning anything.
//
// It is a real provider, dispatched like local/runpod/vast, but it does
// NOT own the resource's lifecycle -- which is why several Provider
// methods are deliberately hollow. This is by design, not incomplete:
//
//   - Spawn fabricates an Instance describing the external endpoint.
//     No side effect; the engine already exists.
//   - Terminate DETACHES: it lets the Service drop iplane's record but
//     never touches the operator's engine. There is nothing for iplane
//     to tear down.
//   - Describe / List return empty: there is no provider-side registry
//     to query (same rationale as the local provider).
//   - Deploy (the Deployer capability) emits RUNNING + the endpoint
//     immediately -- there is no container to run.
//
// The endpoint travels from ReplicaSpec.engine_endpoint into the Spec's
// tag map (provisioners.ExternalEndpointTag) and out here.
//
// The natural second impl of this non-owning-provider category is a
// hosted OpenAI-compatible API provider (auth + per-token vendor cost);
// external stubs exactly those two axes.
package external

import (
	"context"
	"fmt"
	"time"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/sshkeys"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Provider implements provisioners.Provider (and the Deployer
// capability) for operator-managed engines reachable at a URL.
type Provider struct {
	clock func() time.Time
}

// New constructs the external provider. It needs no client or API key
// -- there is no provider API to talk to.
func New() *Provider { return &Provider{clock: time.Now} }

// Name satisfies provisioners.Provider.
func (p *Provider) Name() string { return provisioners.ProviderExternal }

// endpointFromSpec pulls the operator-supplied engine URL out of the
// spec's tag map. Returns "" when absent.
func endpointFromSpec(spec *provisionerv1.Spec) string {
	if spec == nil {
		return ""
	}
	return spec.GetTags()[provisioners.ExternalEndpointTag]
}

// Spawn fabricates an Instance describing the external engine. No side
// effect -- the engine already exists and is the operator's to manage.
// The record is ACTIVE immediately (no asynchronous provisioning), with
// a zero hourly rate (operator-owned hardware; cost is not iplane's to
// know). Errors when the endpoint tag is missing, since without a URL
// there is nothing to point at.
func (p *Provider) Spawn(ctx context.Context, spec *provisionerv1.Spec) (*provisionerv1.Instance, error) {
	if err := ctx.Err(); err != nil {
		return nil, provisioners.NewProviderError(p.Name(), "spawn", err, 0)
	}
	if spec == nil {
		return nil, provisioners.NewProviderError(p.Name(), "spawn", fmt.Errorf("spec is nil"), 0)
	}
	endpoint := endpointFromSpec(spec)
	if endpoint == "" {
		return nil, provisioners.NewProviderError(p.Name(), "spawn",
			fmt.Errorf("external provider requires an engine endpoint (set ReplicaSpec.engine_endpoint / --engine-endpoints)"), 0)
	}
	now := timestamppb.New(p.clock())
	return &provisionerv1.Instance{
		Id:            spec.GetId(),
		ProviderId:    "external:" + spec.GetId(),
		Provider:      p.Name(),
		Spec:          spec,
		Region:        spec.GetRegion(),
		HourlyRateUsd: 0,
		State:         provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE,
		CreatedAt:     now,
		ActivatedAt:   now,
	}, nil
}

// Terminate detaches: there is no provider-side engine for iplane to
// stop. The Service still patches the local record to TERMINATED so
// list shows it correctly. Idempotent by construction.
func (p *Provider) Terminate(ctx context.Context, providerID string) error {
	if err := ctx.Err(); err != nil {
		return provisioners.NewProviderError(p.Name(), "terminate", err, 0)
	}
	return nil
}

// Describe returns ErrNotFound: external has no provider-side registry;
// the iplane state file is the only record. Mirrors the local provider.
func (p *Provider) Describe(ctx context.Context, providerID string) (*provisionerv1.Instance, error) {
	return nil, provisioners.NewProviderError(p.Name(), "describe", provisioners.ErrNotFound, 0)
}

// List returns empty: no provider-side registry to enumerate.
func (p *Provider) List(ctx context.Context, filter map[string]string) ([]*provisionerv1.InstanceRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, provisioners.NewProviderError(p.Name(), "list", err, 0)
	}
	return nil, nil
}

// Deploy implements the Deployer capability. There is no container to
// run -- the engine is already serving -- so it emits RUNNING with the
// operator-supplied endpoint in a single step. The health-poll loop
// verifies the endpoint is actually live, the same as for any replica.
func (p *Provider) Deploy(ctx context.Context, dep *provisionerv1.Deployment, inst *provisionerv1.Instance, _ *sshkeys.KeyPair, emit func(provisioners.DeployStateUpdate)) error {
	if dep == nil || inst == nil {
		return fmt.Errorf("external.Deploy: deployment and instance are required")
	}
	endpoint := endpointFromSpec(inst.GetSpec())
	if endpoint == "" {
		emit(provisioners.DeployStateUpdate{
			State:         provisionerv1.DeploymentState_DEPLOYMENT_STATE_FAILED,
			Phase:         "external:attach",
			FailureReason: "external instance carries no engine endpoint",
		})
		return fmt.Errorf("external.Deploy: instance %q carries no engine endpoint", inst.GetId())
	}
	emit(provisioners.DeployStateUpdate{
		State:           provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
		Phase:           "external:attach",
		ProgressMessage: "attached to operator-managed engine " + endpoint,
		EngineEndpoint:  endpoint,
	})
	return nil
}

// Destroy detaches: emit TERMINATED without touching the operator's
// engine. Mirrors Terminate.
func (p *Provider) Destroy(ctx context.Context, dep *provisionerv1.Deployment, inst *provisionerv1.Instance, _ *sshkeys.KeyPair, emit func(provisioners.DeployStateUpdate)) error {
	emit(provisioners.DeployStateUpdate{
		State: provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATED,
		Phase: "external:detach",
	})
	return nil
}

// Compile-time checks: external is both a Provider and a Deployer.
var (
	_ provisioners.Provider = (*Provider)(nil)
	_ provisioners.Deployer = (*Provider)(nil)
)
