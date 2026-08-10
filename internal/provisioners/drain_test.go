package provisioners_test

import (
	"context"
	"testing"
	"time"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
)

func TestDrainReplicasQuarantinesBeforeWaiting(t *testing.T) {
	svc, store := fanOutMultiReplicaSvc(t, nil)
	if _, err := svc.CreateDeployment(context.Background(), multiReplicaCreateReq("my-llama", 3)); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}

	ids := []string{"my-llama-r0", "my-llama-r1", "my-llama-r2"}
	// Force skips the wait, so this returns immediately and we can assert the
	// quarantine half in isolation.
	if err := svc.DrainReplicas(context.Background(), "my-llama", ids,
		provisioners.DrainOptions{Force: true}); err != nil {
		t.Fatalf("DrainReplicas: %v", err)
	}

	st, _ := store.Read()
	quarantined := map[string]bool{}
	for _, id := range st.Deployments["my-llama"].GetUnhealthyInstanceIds() {
		quarantined[id] = true
	}
	for _, id := range ids {
		if !quarantined[id] {
			t.Errorf("%s was not quarantined; the router would keep dispatching to a draining replica", id)
		}
	}
}

// The wait is the operator's stated budget. Force is the documented way to
// skip it, and it must actually skip it rather than shortening it.
func TestDrainReplicasForceDoesNotWait(t *testing.T) {
	svc, _ := fanOutMultiReplicaSvc(t, nil)
	if _, err := svc.CreateDeployment(context.Background(), multiReplicaCreateReq("my-llama", 2)); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	start := time.Now()
	err := svc.DrainReplicas(context.Background(), "my-llama",
		[]string{"my-llama-r0", "my-llama-r1"},
		provisioners.DrainOptions{Force: true, Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("DrainReplicas: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("--force waited %s; it is the no-wait path", elapsed)
	}
}

func TestDrainReplicasHonoursTheTimeout(t *testing.T) {
	svc, _ := fanOutMultiReplicaSvc(t, nil)
	if _, err := svc.CreateDeployment(context.Background(), multiReplicaCreateReq("my-llama", 1)); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	start := time.Now()
	if err := svc.DrainReplicas(context.Background(), "my-llama", []string{"my-llama"},
		provisioners.DrainOptions{Timeout: 150 * time.Millisecond}); err != nil {
		t.Fatalf("DrainReplicas: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Errorf("drain returned after %s, before its %s budget elapsed", elapsed, 150*time.Millisecond)
	}
}

// A cancelled drain leaves replicas quarantined and nothing destroyed. That
// half-state is the safe one: no new work lands, and the operator can retry
// or restore.
func TestDrainReplicasCancelledLeavesQuarantineInPlace(t *testing.T) {
	svc, store := fanOutMultiReplicaSvc(t, nil)
	if _, err := svc.CreateDeployment(context.Background(), multiReplicaCreateReq("my-llama", 2)); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := svc.DrainReplicas(ctx, "my-llama", []string{"my-llama-r0", "my-llama-r1"},
		provisioners.DrainOptions{Timeout: 10 * time.Second})
	if err == nil {
		t.Fatal("a cancelled drain reported success")
	}
	st, _ := store.Read()
	if len(st.Deployments["my-llama"].GetUnhealthyInstanceIds()) != 2 {
		t.Error("cancelled drain did not leave the replicas quarantined")
	}
	for _, id := range []string{"my-llama-r0", "my-llama-r1"} {
		if inst := st.Instances[id]; inst.GetState() == provisionerv1.InstanceState_INSTANCE_STATE_TERMINATED {
			t.Errorf("%s was destroyed by a cancelled drain", id)
		}
	}
}

// The headline: draining a member releases every node in its span, with
// nothing left behind. This is the acceptance criterion issue 205 names.
func TestDrainAndDestroyReleasesEveryNode(t *testing.T) {
	svc, store := fanOutMultiReplicaSvc(t, nil)
	if _, err := svc.CreateDeployment(context.Background(), multiReplicaCreateReq("my-llama", 3)); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}

	released, err := svc.DrainAndDestroyDeployment(context.Background(), "my-llama",
		provisioners.DrainOptions{Force: true})
	if err != nil {
		t.Fatalf("DrainAndDestroyDeployment: %v", err)
	}
	if len(released) != 3 {
		t.Errorf("released %v, want 3 nodes", released)
	}

	st, _ := store.Read()
	for _, id := range []string{"my-llama-r0", "my-llama-r1", "my-llama-r2"} {
		if got := st.Instances[id].GetState(); got != provisionerv1.InstanceState_INSTANCE_STATE_TERMINATED {
			t.Errorf("%s = %s after drain, want TERMINATED; an orphaned node keeps billing", id, got)
		}
	}
	if got := st.Deployments["my-llama"].GetState(); got != provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATED {
		t.Errorf("deployment = %s after drain, want TERMINATED", got)
	}
}

func TestDrainReplicasRejectsEmptyInput(t *testing.T) {
	svc, _ := fanOutMultiReplicaSvc(t, nil)
	if err := svc.DrainReplicas(context.Background(), "my-llama", nil, provisioners.DrainOptions{}); err == nil {
		t.Error("draining zero replicas was accepted")
	}
	if err := svc.DrainReplicas(context.Background(), "", []string{"x"}, provisioners.DrainOptions{}); err == nil {
		t.Error("draining without a deployment id was accepted")
	}
}
