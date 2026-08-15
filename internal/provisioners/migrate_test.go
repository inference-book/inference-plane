package provisioners_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/local"
	"github.com/inference-book/inference-plane/internal/provisioners/stores/file"
)

// migrateFixture seeds a store with a running two-replica deployment on one
// provider, plus whatever volumes the test needs staged.
func migrateFixture(t *testing.T, model string, volumes ...*provisionerv1.Volume) *provisioners.Service {
	t.Helper()
	store, err := file.Open(settleableTempDir(t), "test-operator")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	err = store.Update(func(f *provisioners.State) error {
		f.Instances["dep-r0"] = &provisionerv1.Instance{Id: "dep-r0", Provider: "vast"}
		f.Instances["dep-r1"] = &provisionerv1.Instance{Id: "dep-r1", Provider: "vast"}
		f.Deployments["dep"] = &provisionerv1.Deployment{
			Id:          "dep",
			Model:       model,
			State:       provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
			InstanceIds: []string{"dep-r0", "dep-r1"},
			ReplicaSpecs: []*provisionerv1.ReplicaSpec{
				{Provider: "vast", Requirements: &provisionerv1.ResourceRequirements{MinVramGb: 80}},
			},
		}
		for _, v := range volumes {
			f.Volumes[v.GetId()] = v
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed store: %v", err)
	}
	return provisioners.New([]provisioners.Provider{local.New()}, store, "test-operator")
}

// settleableTempDir is t.TempDir with a retrying cleanup.
//
// ScaleDeployment launches a writer goroutine per slot and returns before they
// finish, so a failed grow can still be patching the state file after the call
// has returned an error. Plain t.TempDir then races that writer and fails the
// test in cleanup with "directory not empty", which is a flake in the harness
// rather than a defect in what is being tested.
//
// Retrying is the honest workaround and not a fix. That fire-and-forget writers
// outlive the call they belong to is a real property of the deploy path, filed
// separately; a test should not be the thing that discovers it each time.
func settleableTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "iplane-migrate-*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() {
		for i := 0; i < 20; i++ {
			if err := os.RemoveAll(dir); err == nil {
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Logf("could not remove %s; a deploy writer outlived the test", dir)
	})
	return dir
}

// The cost that dominates a migration and that nobody expects. Weights are
// staged per provider and per region, so moving somewhere they are not pinned
// pays a full cold start. An operator who was not told reads a stalled deploy
// as a hang.
func TestMigrateWarnsWhenTheWarmCacheDoesNotFollow(t *testing.T) {
	svc := migrateFixture(t, "meta-llama/Llama-3-70B")

	plan, err := svc.Migrate(context.Background(), provisioners.MigrateRequest{
		DeploymentID: "dep",
		To:           &provisionerv1.ReplicaSpec{Provider: "runpod", Region: "US-TX-3"},
		DryRun:       true,
	})
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	if plan.WarmCacheFollows {
		t.Error("claimed the warm cache follows to a provider where nothing is pinned")
	}
	joined := strings.Join(plan.Warnings, "\n")
	if !strings.Contains(joined, "cold start") {
		t.Errorf("warnings do not name the cost that dominates the move:\n%s", joined)
	}
	if !strings.Contains(joined, "meta-llama/Llama-3-70B") {
		t.Errorf("warnings do not name the model:\n%s", joined)
	}
}

// The mirror. A destination where the model is already staged is a warm move,
// and saying so is what makes the warning above worth reading rather than
// boilerplate that appears every time.
func TestMigrateRecognisesAWarmDestination(t *testing.T) {
	svc := migrateFixture(t, "meta-llama/Llama-3-70B", &provisionerv1.Volume{
		Id: "vol-1", Provider: "runpod", Region: "US-TX-3",
		Models: []string{"meta-llama/Llama-3-70B"},
	})

	plan, err := svc.Migrate(context.Background(), provisioners.MigrateRequest{
		DeploymentID: "dep",
		To:           &provisionerv1.ReplicaSpec{Provider: "runpod", Region: "US-TX-3"},
		DryRun:       true,
	})
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	if !plan.WarmCacheFollows {
		t.Error("did not recognise a destination where the model is already pinned")
	}
	for _, w := range plan.Warnings {
		if strings.Contains(w, "cold start") {
			t.Errorf("warned about a cold start on a warm destination: %s", w)
		}
	}
}

// A volume is datacenter-locked as well as provider-scoped, so the same model
// pinned in the wrong region does not help. Matching on provider alone would
// promise a warm move that arrives cold.
func TestMigrateRequiresTheRegionToMatchToo(t *testing.T) {
	svc := migrateFixture(t, "m", &provisionerv1.Volume{
		Id: "vol-1", Provider: "runpod", Region: "EU-RO-1", Models: []string{"m"},
	})

	plan, err := svc.Migrate(context.Background(), provisioners.MigrateRequest{
		DeploymentID: "dep",
		To:           &provisionerv1.ReplicaSpec{Provider: "runpod", Region: "US-TX-3"},
		DryRun:       true,
	})
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if plan.WarmCacheFollows {
		t.Error("treated a volume in another region as following the move")
	}
}

// "Same shape, different vendor" is the common migration and should not
// require restating the hardware. Losing the source requirements would
// silently re-place onto whatever the destination's defaults are.
func TestMigrateInheritsTheSourceShape(t *testing.T) {
	svc := migrateFixture(t, "m")

	plan, err := svc.Migrate(context.Background(), provisioners.MigrateRequest{
		DeploymentID: "dep",
		To:           &provisionerv1.ReplicaSpec{Provider: "runpod"},
		DryRun:       true,
	})
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	if plan.ReplicaCount != 2 {
		t.Errorf("replica count = %d, want the source's 2", plan.ReplicaCount)
	}
	if plan.FromProvider != "vast" || plan.ToProvider != "runpod" {
		t.Errorf("route = %s -> %s, want vast -> runpod", plan.FromProvider, plan.ToProvider)
	}
}

// A dry run must not touch anything. It is the only way to see the cold-start
// warning before committing, so it has to be free to run.
func TestMigrateDryRunProvisionsNothing(t *testing.T) {
	svc := migrateFixture(t, "m")

	plan, err := svc.Migrate(context.Background(), provisioners.MigrateRequest{
		DeploymentID: "dep",
		To:           &provisionerv1.ReplicaSpec{Provider: "runpod"},
		DryRun:       true,
	})
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if len(plan.AddedInstances) != 0 {
		t.Errorf("a dry run reported provisioned replicas: %v", plan.AddedInstances)
	}
}

// Migrate is capacity tuning of something already serving, not a recovery
// mechanism, which is the same precondition scale carries.
func TestMigrateRefusesADeploymentThatIsNotServing(t *testing.T) {
	store, err := file.Open(t.TempDir(), "op")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	_ = store.Update(func(f *provisioners.State) error {
		f.Deployments["dep"] = &provisionerv1.Deployment{
			Id:          "dep",
			State:       provisionerv1.DeploymentState_DEPLOYMENT_STATE_FAILED,
			InstanceIds: []string{"dep-r0"},
		}
		return nil
	})
	svc := provisioners.New([]provisioners.Provider{local.New()}, store, "op")

	_, err = svc.Migrate(context.Background(), provisioners.MigrateRequest{
		DeploymentID: "dep",
		To:           &provisionerv1.ReplicaSpec{Provider: "runpod"},
		DryRun:       true,
	})

	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("code = %v, want FailedPrecondition for a FAILED deployment", status.Code(err))
	}
}

// A destination equal to the source is almost certainly a mistake, and it is
// cheap to say so before spending minutes re-renting the same hardware.
func TestMigrateFlagsASameProviderMove(t *testing.T) {
	svc := migrateFixture(t, "m")

	plan, err := svc.Migrate(context.Background(), provisioners.MigrateRequest{
		DeploymentID: "dep",
		To:           &provisionerv1.ReplicaSpec{Provider: "vast"},
		DryRun:       true,
	})
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if !strings.Contains(strings.Join(plan.Warnings, "\n"), "re-places rather than migrates") {
		t.Errorf("a same-provider move was not flagged: %v", plan.Warnings)
	}
}

// The destination is required. Defaulting it would make an expensive,
// hard-to-reverse operation depend on an implicit choice.
func TestMigrateRequiresADestination(t *testing.T) {
	svc := migrateFixture(t, "m")

	_, err := svc.Migrate(context.Background(), provisioners.MigrateRequest{
		DeploymentID: "dep",
		To:           &provisionerv1.ReplicaSpec{},
	})

	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument when no destination is named", status.Code(err))
	}
}

// The ordering is the whole design, and this is the half of it that can be
// tested without renting anything: if growing onto the destination fails, the
// source must still be serving.
//
// Draining first and provisioning second would be the natural way to write
// this and it would take the deployment down whenever the destination was out
// of stock, which is one of the three reasons #290 lists for migrating in the
// first place.
func TestMigrateLeavesTheSourceServingWhenTheDestinationFails(t *testing.T) {
	svc := migrateFixture(t, "m")

	// "nosuchvendor" is not configured, so growing cannot succeed.
	_, err := svc.Migrate(context.Background(), provisioners.MigrateRequest{
		DeploymentID: "dep",
		To:           &provisionerv1.ReplicaSpec{Provider: "nosuchvendor"},
	})
	if err == nil {
		t.Fatal("expected the migration to fail when the destination cannot be provisioned")
	}
	if !strings.Contains(err.Error(), "growing onto") {
		t.Errorf("error does not say which half failed: %v", err)
	}

	// The source replicas must be untouched: same ids, not quarantined.
	got, derr := svc.DescribeDeployment(context.Background(),
		&provisionerv1.DescribeDeploymentRequest{Id: "dep"})
	if derr != nil {
		t.Fatalf("DescribeDeployment: %v", derr)
	}
	dep := got.GetDeployment()
	if len(dep.GetInstanceIds()) != 2 {
		t.Errorf("instance_ids = %v, want the original two still present", dep.GetInstanceIds())
	}
	if len(dep.GetUnhealthyInstanceIds()) != 0 {
		t.Errorf("source replicas were quarantined despite the destination failing: %v",
			dep.GetUnhealthyInstanceIds())
	}
}
