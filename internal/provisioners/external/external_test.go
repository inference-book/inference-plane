package external_test

import (
	"context"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/external"
)

func specWithEndpoint(id, endpoint string) *provisionerv1.Spec {
	spec := &provisionerv1.Spec{Id: id}
	if endpoint != "" {
		spec.Tags = map[string]string{provisioners.ExternalEndpointTag: endpoint}
	}
	return spec
}

// TestSpawn_ReadsEndpointFromTag: Spawn fabricates an ACTIVE instance for
// the operator-supplied endpoint, at zero cost, carrying the endpoint on
// its spec so Deploy can read it back.
func TestSpawn_ReadsEndpointFromTag(t *testing.T) {
	p := external.New()
	inst, err := p.Spawn(context.Background(), specWithEndpoint("e0", "http://host:9001"))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if inst.GetState() != provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE {
		t.Errorf("state = %s, want ACTIVE", inst.GetState())
	}
	if inst.GetProvider() != provisioners.ProviderExternal {
		t.Errorf("provider = %q, want external", inst.GetProvider())
	}
	if inst.GetHourlyRateUsd() != 0 {
		t.Errorf("hourly_rate_usd = %v, want 0 (operator-owned)", inst.GetHourlyRateUsd())
	}
	if got := inst.GetSpec().GetTags()[provisioners.ExternalEndpointTag]; got != "http://host:9001" {
		t.Errorf("endpoint tag = %q, want http://host:9001", got)
	}
}

// TestSpawn_MissingEndpointErrors: without an endpoint there is nothing to
// attach to, so Spawn fails rather than fabricating a dangling instance.
func TestSpawn_MissingEndpointErrors(t *testing.T) {
	p := external.New()
	if _, err := p.Spawn(context.Background(), specWithEndpoint("e0", "")); err == nil {
		t.Fatal("Spawn without endpoint should error")
	}
}

// TestDeploy_EmitsRunningWithEndpoint: Deploy attaches in one step,
// emitting RUNNING with the endpoint (no container run).
func TestDeploy_EmitsRunningWithEndpoint(t *testing.T) {
	p := external.New()
	inst, err := p.Spawn(context.Background(), specWithEndpoint("e0", "http://host:9001"))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	var last provisioners.DeployStateUpdate
	err = p.Deploy(context.Background(), &provisionerv1.Deployment{Id: "d"}, inst, nil, func(u provisioners.DeployStateUpdate) {
		last = u
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if last.State != provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING {
		t.Errorf("emitted state = %s, want RUNNING", last.State)
	}
	if last.EngineEndpoint != "http://host:9001" {
		t.Errorf("emitted endpoint = %q, want http://host:9001", last.EngineEndpoint)
	}
}

// TestDeploy_MissingEndpointFails: an instance without an endpoint can't be
// attached; Deploy emits FAILED and errors.
func TestDeploy_MissingEndpointFails(t *testing.T) {
	p := external.New()
	inst := &provisionerv1.Instance{Id: "e0", Spec: &provisionerv1.Spec{Id: "e0"}}
	var last provisioners.DeployStateUpdate
	err := p.Deploy(context.Background(), &provisionerv1.Deployment{Id: "d"}, inst, nil, func(u provisioners.DeployStateUpdate) {
		last = u
	})
	if err == nil {
		t.Fatal("Deploy without endpoint should error")
	}
	if last.State != provisionerv1.DeploymentState_DEPLOYMENT_STATE_FAILED {
		t.Errorf("emitted state = %s, want FAILED", last.State)
	}
}

// TestTerminate_Detaches: Terminate never touches the operator's engine, so
// it always succeeds (the Service just drops the local record).
func TestTerminate_Detaches(t *testing.T) {
	p := external.New()
	if err := p.Terminate(context.Background(), "external:e0"); err != nil {
		t.Errorf("Terminate should detach cleanly, got %v", err)
	}
}
