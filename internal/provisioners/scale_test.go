package provisioners_test

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
)

// TestScale_Up_FromOneToThree: scale a single-replica deployment to 3.
// Two new slots appended at r1, r2; deployment stays RUNNING; the
// original instance (just deploy_id) is preserved.
func TestScale_Up_FromOneToThree(t *testing.T) {
	svc, _ := fanOutMultiReplicaSvc(t, nil)

	createReq := multiReplicaCreateReq("grow", 1)
	if _, err := svc.CreateDeployment(context.Background(), createReq); err != nil {
		t.Fatalf("Create: %v", err)
	}

	resp, err := svc.ScaleDeployment(context.Background(), &provisionerv1.ScaleDeploymentRequest{
		Id:             "grow",
		TargetReplicas: 3,
		Wait:           true,
	})
	if err != nil {
		t.Fatalf("Scale: %v", err)
	}
	dep := resp.GetDeployment()
	if dep.GetState() != provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING {
		t.Errorf("state = %s, want RUNNING", dep.GetState())
	}
	ids := dep.GetInstanceIds()
	if len(ids) != 3 {
		t.Fatalf("instance_ids len = %d, want 3", len(ids))
	}
	// Slot 0: the original single-instance Beat 1+2 anchor id.
	if ids[0] != "grow" {
		t.Errorf("slot 0 = %q, want grow", ids[0])
	}
	// Slots 1, 2: synthesized via the scale verb.
	if ids[1] != "grow-r1" || ids[2] != "grow-r2" {
		t.Errorf("new slots = %v, want [grow-r1 grow-r2]", ids[1:])
	}
}

// TestScale_Up_MultiReplicaToMore: scale 3->5. Two new slots appended
// at r3, r4; r0/r1/r2 untouched.
func TestScale_Up_MultiReplicaToMore(t *testing.T) {
	svc, _ := fanOutMultiReplicaSvc(t, nil)

	if _, err := svc.CreateDeployment(context.Background(), multiReplicaCreateReq("scaler", 3)); err != nil {
		t.Fatalf("Create: %v", err)
	}

	resp, err := svc.ScaleDeployment(context.Background(), &provisionerv1.ScaleDeploymentRequest{
		Id:             "scaler",
		TargetReplicas: 5,
		Wait:           true,
	})
	if err != nil {
		t.Fatalf("Scale: %v", err)
	}
	dep := resp.GetDeployment()
	if dep.GetState() != provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING {
		t.Errorf("state = %s, want RUNNING", dep.GetState())
	}
	ids := dep.GetInstanceIds()
	want := []string{"scaler-r0", "scaler-r1", "scaler-r2", "scaler-r3", "scaler-r4"}
	if len(ids) != len(want) {
		t.Fatalf("instance_ids len = %d, want %d", len(ids), len(want))
	}
	for i, w := range want {
		if ids[i] != w {
			t.Errorf("slot %d = %q, want %q", i, ids[i], w)
		}
	}
}

// TestScale_Up_PartialFailure: scale 1->3 where r2's executor fails.
// r1 succeeds, r2 fails; deployment lands DEGRADED with
// failure_reason naming r2.
func TestScale_Up_PartialFailure(t *testing.T) {
	svc, _ := fanOutMultiReplicaSvc(t, func(inst *provisionerv1.Instance, emit func(provisioners.DeployStateUpdate)) error {
		if inst.GetId() == "partial-r2" {
			return testErr("simulated scale failure")
		}
		emit(provisioners.DeployStateUpdate{
			State:          provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
			EngineEndpoint: "http://" + inst.GetId() + ":8000",
		})
		return nil
	})

	if _, err := svc.CreateDeployment(context.Background(), multiReplicaCreateReq("partial", 1)); err != nil {
		t.Fatalf("Create: %v", err)
	}

	resp, err := svc.ScaleDeployment(context.Background(), &provisionerv1.ScaleDeploymentRequest{
		Id:             "partial",
		TargetReplicas: 3,
		Wait:           true,
	})
	if err != nil {
		t.Fatalf("Scale: %v", err)
	}
	dep := resp.GetDeployment()
	if dep.GetState() != provisionerv1.DeploymentState_DEPLOYMENT_STATE_DEGRADED {
		t.Errorf("state = %s, want DEGRADED", dep.GetState())
	}
	if !strings.Contains(dep.GetFailureReason(), "1 of 2 new replicas") {
		t.Errorf("failure_reason should say '1 of 2 new replicas': %q", dep.GetFailureReason())
	}
	if !strings.Contains(dep.GetFailureReason(), "partial-r2") {
		t.Errorf("failure_reason should name partial-r2: %q", dep.GetFailureReason())
	}
}

// TestScale_NoOp_SameCount: scale 3->3 is a no-op; record unchanged.
func TestScale_NoOp_SameCount(t *testing.T) {
	svc, _ := fanOutMultiReplicaSvc(t, nil)
	if _, err := svc.CreateDeployment(context.Background(), multiReplicaCreateReq("noop", 3)); err != nil {
		t.Fatalf("Create: %v", err)
	}

	resp, err := svc.ScaleDeployment(context.Background(), &provisionerv1.ScaleDeploymentRequest{
		Id:             "noop",
		TargetReplicas: 3,
		Wait:           true,
	})
	if err != nil {
		t.Fatalf("Scale: %v", err)
	}
	if got := resp.GetDeployment().GetInstanceIds(); len(got) != 3 {
		t.Errorf("noop should leave 3 slots; got %d", len(got))
	}
}

// TestScale_Down_Unimplemented: target < current returns
// Unimplemented pointing at #145.
func TestScale_Down_Unimplemented(t *testing.T) {
	svc, _ := fanOutMultiReplicaSvc(t, nil)
	if _, err := svc.CreateDeployment(context.Background(), multiReplicaCreateReq("shrink", 3)); err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, err := svc.ScaleDeployment(context.Background(), &provisionerv1.ScaleDeploymentRequest{
		Id:             "shrink",
		TargetReplicas: 2,
		Wait:           true,
	})
	if err == nil {
		t.Fatal("expected Unimplemented, got nil")
	}
	if c := status.Code(err); c != codes.Unimplemented {
		t.Errorf("code = %s, want Unimplemented", c)
	}
	if !strings.Contains(err.Error(), "#145") {
		t.Errorf("error should reference #145: %v", err)
	}
}

// TestScale_RejectsZeroOrNegative: target <= 0 is InvalidArgument.
func TestScale_RejectsZeroOrNegative(t *testing.T) {
	svc, _ := fanOutMultiReplicaSvc(t, nil)
	if _, err := svc.CreateDeployment(context.Background(), multiReplicaCreateReq("zero", 1)); err != nil {
		t.Fatalf("Create: %v", err)
	}

	for _, tgt := range []int32{0, -1} {
		_, err := svc.ScaleDeployment(context.Background(), &provisionerv1.ScaleDeploymentRequest{
			Id:             "zero",
			TargetReplicas: tgt,
		})
		if err == nil {
			t.Errorf("target=%d should be InvalidArgument", tgt)
			continue
		}
		if c := status.Code(err); c != codes.InvalidArgument {
			t.Errorf("target=%d: code = %s, want InvalidArgument", tgt, c)
		}
	}
}

// TestScale_RejectsNotFound: target a missing deployment id.
func TestScale_RejectsNotFound(t *testing.T) {
	svc, _ := fanOutMultiReplicaSvc(t, nil)
	_, err := svc.ScaleDeployment(context.Background(), &provisionerv1.ScaleDeploymentRequest{
		Id:             "ghost",
		TargetReplicas: 3,
	})
	if err == nil {
		t.Fatal("expected NotFound for missing deployment")
	}
	if c := status.Code(err); c != codes.NotFound {
		t.Errorf("code = %s, want NotFound", c)
	}
}

// TestScale_RejectsPendingDeployment: a deployment that hasn't reached
// a serving state yet (PENDING/STARTING/CONFIGURING) is not
// scale-eligible. v0.2 ch7-beat3.8 spec.
func TestScale_RejectsPendingDeployment(t *testing.T) {
	svc, store := fanOutMultiReplicaSvc(t, nil)
	// Seed a PENDING record directly so we don't have to race
	// CreateDeployment's executor.
	_ = store.Update(func(f *provisioners.State) error {
		f.Deployments["pending"] = &provisionerv1.Deployment{
			Id:    "pending",
			State: provisionerv1.DeploymentState_DEPLOYMENT_STATE_PENDING,
		}
		return nil
	})
	_, err := svc.ScaleDeployment(context.Background(), &provisionerv1.ScaleDeploymentRequest{
		Id:             "pending",
		TargetReplicas: 3,
	})
	if err == nil {
		t.Fatal("expected FailedPrecondition for PENDING")
	}
	if c := status.Code(err); c != codes.FailedPrecondition {
		t.Errorf("code = %s, want FailedPrecondition", c)
	}
}

// TestScale_DryRun_PreviewsPlannedSlots: dry_run=true returns the
// planned new instance ids without mutating state.
func TestScale_DryRun_PreviewsPlannedSlots(t *testing.T) {
	svc, _ := fanOutMultiReplicaSvc(t, nil)
	if _, err := svc.CreateDeployment(context.Background(), multiReplicaCreateReq("dry", 2)); err != nil {
		t.Fatalf("Create: %v", err)
	}

	resp, err := svc.ScaleDeployment(context.Background(), &provisionerv1.ScaleDeploymentRequest{
		Id:             "dry",
		TargetReplicas: 4,
		DryRun:         true,
	})
	if err != nil {
		t.Fatalf("Scale: %v", err)
	}
	planned := resp.GetPlannedInstanceIds()
	if len(planned) != 2 || planned[0] != "dry-r2" || planned[1] != "dry-r3" {
		t.Errorf("planned = %v, want [dry-r2 dry-r3]", planned)
	}
	// State unmutated -- still 2 slots.
	dep := resp.GetDeployment()
	if got := dep.GetInstanceIds(); len(got) != 2 {
		t.Errorf("dry-run should not mutate; got %d slots, want 2", len(got))
	}
}

// TestScale_PreservesDegradedTombstones: a deployment created in a
// DEGRADED state (one slot failed) scales up. The scale append
// extends the slot table; the existing tombstone slot stays empty.
// Demonstrates the "scale appends, doesn't repair" semantic.
func TestScale_PreservesDegradedTombstones(t *testing.T) {
	var failOnce bool
	svc, _ := fanOutMultiReplicaSvc(t, func(inst *provisionerv1.Instance, emit func(provisioners.DeployStateUpdate)) error {
		// Fail r1 of the initial create; pass everything else.
		if inst.GetId() == "tomb-r1" && !failOnce {
			failOnce = true
			return testErr("create-time failure")
		}
		emit(provisioners.DeployStateUpdate{
			State:          provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
			EngineEndpoint: "http://" + inst.GetId() + ":8000",
		})
		return nil
	})

	if _, err := svc.CreateDeployment(context.Background(), multiReplicaCreateReq("tomb", 3)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Pre-scale: 3 slots, slot 1 is tombstone (DEGRADED).
	resp, err := svc.ScaleDeployment(context.Background(), &provisionerv1.ScaleDeploymentRequest{
		Id:             "tomb",
		TargetReplicas: 5,
		Wait:           true,
	})
	if err != nil {
		t.Fatalf("Scale: %v", err)
	}
	dep := resp.GetDeployment()
	ids := dep.GetInstanceIds()
	if len(ids) != 5 {
		t.Fatalf("len = %d, want 5", len(ids))
	}
	// Tombstone slot 1's engine_endpoint stays empty (scale appended;
	// didn't repair). The instance_id may still be set -- the Instance
	// record exists (rent succeeded) but the engine never came up, so
	// the router (#85) skips the slot via the empty-endpoint check.
	eps := dep.GetEngineEndpoints()
	if eps[1] != "" {
		t.Errorf("slot 1's engine_endpoint should remain tombstone (empty), got %q", eps[1])
	}
	if ids[3] != "tomb-r3" || ids[4] != "tomb-r4" {
		t.Errorf("appended slots = [%q, %q], want [tomb-r3 tomb-r4]", ids[3], ids[4])
	}
	// Deployment stays DEGRADED -- scale didn't repair the existing
	// tombstone. #93's reconciliation will lift it.
	if dep.GetState() != provisionerv1.DeploymentState_DEPLOYMENT_STATE_DEGRADED {
		t.Errorf("state = %s, want DEGRADED (existing tombstone preserved)", dep.GetState())
	}
}
