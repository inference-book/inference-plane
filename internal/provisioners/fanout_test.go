package provisioners_test

import (
	"context"
	"strings"
	"testing"
	"time"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/modelstores"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/stores/file"
	"github.com/inference-book/inference-plane/internal/sshkeys"
)

// fanOutMultiReplicaSvc builds a Service wired with an image-native
// provider whose Deploy emits a unique endpoint per replica (the
// per-replica instance id selects the endpoint, so r0/r1/r2 each
// stamp a distinct URL). deployFn override lets individual tests
// program failures by instance id.
func fanOutMultiReplicaSvc(t *testing.T, deployFn func(inst *provisionerv1.Instance, emit func(provisioners.DeployStateUpdate)) error) (*provisioners.Service, *file.Store) {
	t.Helper()
	prov := &fanOutMockProvider{name: "mockfan", deployFn: deployFn}
	store, err := file.Open(t.TempDir(), "default")
	if err != nil {
		t.Fatalf("file.Open: %v", err)
	}
	svc := provisioners.New([]provisioners.Provider{prov}, store, "default",
		provisioners.WithKeyStore(newKeyStore(t)),
	)
	return svc, store
}

// fanOutMockProvider satisfies provisioners.Provider + Deployer. Its
// Deploy emits a per-replica endpoint derived from the instance id
// (so each slot's engine_endpoints[i] is distinguishable). Optional
// deployFn override per-call lets tests program failures.
// AttachesMounts implements provisioners.MountAttacher. This double
// stands in for an image-native provider that honours mounts, which is
// what the warm-cache tests below are about. Declaring it is required
// rather than optional: a Deployer that stays silent is treated as
// unable to attach, so that a real adapter which forgets refuses the
// mount instead of running cold and reporting warm (#254).
func (m *fanOutMockProvider) AttachesMounts() bool { return true }

type fanOutMockProvider struct {
	name      string
	deployFn  func(inst *provisionerv1.Instance, emit func(provisioners.DeployStateUpdate)) error
	destroyFn func(inst *provisionerv1.Instance) error
}

func (p *fanOutMockProvider) Name() string { return p.name }

func (p *fanOutMockProvider) Spawn(_ context.Context, spec *provisionerv1.Spec) (*provisionerv1.Instance, error) {
	return &provisionerv1.Instance{
		Id:         spec.GetId(),
		ProviderId: p.name + ":" + spec.GetId(),
		Provider:   p.name,
		Spec:       spec,
		State:      provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE,
	}, nil
}

func (p *fanOutMockProvider) Terminate(_ context.Context, _ string) error { return nil }
func (p *fanOutMockProvider) Describe(_ context.Context, providerID string) (*provisionerv1.Instance, error) {
	return &provisionerv1.Instance{
		Id:         strings.TrimPrefix(providerID, p.name+":"),
		ProviderId: providerID,
		Provider:   p.name,
		State:      provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE,
	}, nil
}
func (p *fanOutMockProvider) List(context.Context, map[string]string) ([]*provisionerv1.InstanceRef, error) {
	return nil, nil
}

func (p *fanOutMockProvider) Deploy(_ context.Context, _ *provisionerv1.Deployment, inst *provisionerv1.Instance, _ *sshkeys.KeyPair, emit func(provisioners.DeployStateUpdate)) error {
	if p.deployFn != nil {
		return p.deployFn(inst, emit)
	}
	// Default: emit RUNNING with a per-replica endpoint so each slot's
	// engine_endpoints[i] differs.
	emit(provisioners.DeployStateUpdate{
		State:          provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
		Phase:          "test:fanout",
		ContainerID:    inst.GetId() + "-container",
		EngineEndpoint: "http://" + inst.GetId() + ":8000",
	})
	return nil
}

func (p *fanOutMockProvider) Destroy(_ context.Context, _ *provisionerv1.Deployment, inst *provisionerv1.Instance, _ *sshkeys.KeyPair, _ func(provisioners.DeployStateUpdate)) error {
	if p.destroyFn != nil {
		return p.destroyFn(inst)
	}
	return nil
}

// fanOutMultiReplicaSvcWithDestroy is fanOutMultiReplicaSvc with a
// programmable Destroy, so teardown tests can fail one replica and assert
// the others still go away (issue 228's partial-failure contract).
func fanOutMultiReplicaSvcWithDestroy(t *testing.T, destroyFn func(inst *provisionerv1.Instance) error) (*provisioners.Service, *file.Store) {
	t.Helper()
	prov := &fanOutMockProvider{name: "mockfan", destroyFn: destroyFn}
	store, err := file.Open(t.TempDir(), "default")
	if err != nil {
		t.Fatalf("file.Open: %v", err)
	}
	svc := provisioners.New([]provisioners.Provider{prov}, store, "default",
		provisioners.WithKeyStore(newKeyStore(t)),
	)
	return svc, store
}

func multiReplicaCreateReq(depID string, replicas int32) *provisionerv1.CreateDeploymentRequest {
	return &provisionerv1.CreateDeploymentRequest{
		Deployment: &provisionerv1.Deployment{
			Id:         depID,
			Image:      "vllm/vllm-openai:v0.7.0",
			Model:      "Qwen/Qwen2.5-1.5B-Instruct",
			EnginePort: 8000,
		},
		ReplicasSpec: []*provisionerv1.ReplicaSpec{{
			Provider:     "mockfan",
			Requirements: &provisionerv1.ResourceRequirements{Sku: "mock-sku"},
			Replicas:     replicas,
		}},
		Wait: true,
	}
}

// TestFanOut_AllSucceed: 3 replicas all-succeed -> RUNNING with 3
// instance_ids ([-r0, -r1, -r2]), 3 engine_endpoints (one per
// replica, distinct URLs), no failure_reason.
func TestFanOut_AllSucceed(t *testing.T) {
	svc, store := fanOutMultiReplicaSvc(t, nil)

	resp, err := svc.CreateDeployment(context.Background(), multiReplicaCreateReq("my-llama", 3))
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	dep := resp.GetDeployment()
	if dep.GetState() != provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING {
		t.Errorf("state = %s, want RUNNING", dep.GetState())
	}
	if got := dep.GetInstanceIds(); len(got) != 3 || got[0] != "my-llama-r0" || got[1] != "my-llama-r1" || got[2] != "my-llama-r2" {
		t.Errorf("instance_ids = %v, want [my-llama-r0 my-llama-r1 my-llama-r2]", got)
	}
	if got := dep.GetEngineEndpoints(); len(got) != 3 {
		t.Fatalf("engine_endpoints len = %d, want 3", len(got))
	}
	for i, ep := range dep.GetEngineEndpoints() {
		want := "http://my-llama-r" + string(rune('0'+i)) + ":8000"
		if ep != want {
			t.Errorf("engine_endpoints[%d] = %q, want %q", i, ep, want)
		}
	}
	if dep.GetFailureReason() != "" {
		t.Errorf("failure_reason should be empty, got %q", dep.GetFailureReason())
	}

	// Per-replica Instance records persisted.
	state, _ := store.Read()
	for _, id := range []string{"my-llama-r0", "my-llama-r1", "my-llama-r2"} {
		if _, ok := state.Instances[id]; !ok {
			t.Errorf("instance %q not persisted", id)
		}
	}
}

// seedWarmVolume records a pinned volume in the registry so auto-resolve
// has something to find.
func seedWarmVolume(t *testing.T, store *file.Store, id, provider, region string, models ...string) {
	t.Helper()
	if err := store.Update(func(f *provisioners.State) error {
		f.Volumes[id] = &provisionerv1.Volume{
			Id: id, Provider: provider, Region: region, MountPath: "/models", Models: models,
		}
		return nil
	}); err != nil {
		t.Fatalf("seed volume: %v", err)
	}
}

func warmDeployReq(id, model, provider, region string, replicas int32) *provisionerv1.CreateDeploymentRequest {
	return &provisionerv1.CreateDeploymentRequest{
		Deployment: &provisionerv1.Deployment{
			Id: id, Image: "vllm/vllm-openai:v0.7.0", Model: model, EnginePort: 8000,
		},
		ReplicasSpec: []*provisionerv1.ReplicaSpec{{
			Provider: provider, Region: region,
			Requirements: &provisionerv1.ResourceRequirements{Sku: "mock-sku"},
			Replicas:     replicas,
		}},
		Wait: true,
	}
}

// TestCreateDeployment_AutoResolvesWarmMountFromRegistry: with a model
// pinned on a volume in the deploy's (provider, region) and no config
// mount, the deploy auto-mounts that volume -- warm without any config.
func TestCreateDeployment_AutoResolvesWarmMountFromRegistry(t *testing.T) {
	svc, store := fanOutMultiReplicaSvc(t, nil)
	seedWarmVolume(t, store, "vol-1", "mockfan", "EU-RO-1", "Qwen/Qwen2.5-32B-Instruct-AWQ")

	resp, err := svc.CreateDeployment(context.Background(),
		warmDeployReq("warm", "Qwen/Qwen2.5-32B-Instruct-AWQ", "mockfan", "EU-RO-1", 1))
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	mounts := resp.GetDeployment().GetMounts()
	if len(mounts) != 1 || mounts[0].GetVolumeId() != "vol-1" || mounts[0].GetProvider() != "mockfan" {
		t.Errorf("auto-resolve should mount vol-1; got %+v", mounts)
	}
}

// TestCreateDeployment_NoPinStaysCold: no matching pin -> no mount.
func TestCreateDeployment_NoPinStaysCold(t *testing.T) {
	svc, store := fanOutMultiReplicaSvc(t, nil)
	// Pinned model exists but in a different region than the deploy.
	seedWarmVolume(t, store, "vol-1", "mockfan", "US-CA-1", "Qwen/Qwen2.5-32B-Instruct-AWQ")

	resp, err := svc.CreateDeployment(context.Background(),
		warmDeployReq("cold", "Qwen/Qwen2.5-32B-Instruct-AWQ", "mockfan", "EU-RO-1", 1))
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	if got := resp.GetDeployment().GetMounts(); len(got) != 0 {
		t.Errorf("no pin in the deploy's region -> cold; got mounts %+v", got)
	}
}

// TestCreateDeployment_HeterogeneousFleetStaysCold: a fleet spanning two
// regions can't share one mount, so auto-resolve is skipped.
func TestCreateDeployment_HeterogeneousFleetStaysCold(t *testing.T) {
	svc, store := fanOutMultiReplicaSvc(t, nil)
	seedWarmVolume(t, store, "vol-1", "mockfan", "EU-RO-1", "m")

	req := &provisionerv1.CreateDeploymentRequest{
		Deployment: &provisionerv1.Deployment{Id: "hetero", Image: "vllm/vllm-openai:v0.7.0", Model: "m", EnginePort: 8000},
		ReplicasSpec: []*provisionerv1.ReplicaSpec{
			{Provider: "mockfan", Region: "EU-RO-1", Requirements: &provisionerv1.ResourceRequirements{Sku: "mock-sku"}, Replicas: 1},
			{Provider: "mockfan", Region: "US-CA-1", Requirements: &provisionerv1.ResourceRequirements{Sku: "mock-sku"}, Replicas: 1},
		},
		Wait: true,
	}
	resp, err := svc.CreateDeployment(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	if got := resp.GetDeployment().GetMounts(); len(got) != 0 {
		t.Errorf("heterogeneous fleet must stay cold; got mounts %+v", got)
	}
}

// TestCreateDeployment_ConfigMountWinsOverAutoResolve: an explicit
// model_cache mount (from the ModelStore) takes precedence; the registry
// is only consulted when no mount was configured.
func TestCreateDeployment_ConfigMountWinsOverAutoResolve(t *testing.T) {
	prov := &fanOutMockProvider{name: "mockfan"}
	store, err := file.Open(t.TempDir(), "default")
	if err != nil {
		t.Fatalf("file.Open: %v", err)
	}
	ms := &stubModelStore{respond: func(spec string) (modelstores.Resolved, error) {
		return modelstores.Resolved{
			EngineModelArg: spec,
			Mounts:         []modelstores.Mount{{VolumeID: "config-vol", MountPath: "/models", Provider: "mockfan"}},
		}, nil
	}}
	svc := provisioners.New([]provisioners.Provider{prov}, store, "default",
		provisioners.WithKeyStore(newKeyStore(t)),
		provisioners.WithModelStore(ms))
	seedWarmVolume(t, store, "registry-vol", "mockfan", "EU-RO-1", "m")

	resp, err := svc.CreateDeployment(context.Background(), warmDeployReq("both", "m", "mockfan", "EU-RO-1", 1))
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	mounts := resp.GetDeployment().GetMounts()
	if len(mounts) != 1 || mounts[0].GetVolumeId() != "config-vol" {
		t.Errorf("config mount should win over the registry; got %+v", mounts)
	}
}

// TestFanOut_StampsMountsOntoRecord: a warm-cache store's mounts must
// reach the multi-replica deployment record -- N replicas sharing one
// pre-staged volume is the payoff case for warm mounts.
func TestFanOut_StampsMountsOntoRecord(t *testing.T) {
	prov := &fanOutMockProvider{name: "mockfan"}
	store, err := file.Open(t.TempDir(), "default")
	if err != nil {
		t.Fatalf("file.Open: %v", err)
	}
	ms := &stubModelStore{respond: func(spec string) (modelstores.Resolved, error) {
		return modelstores.Resolved{
			EngineModelArg: spec,
			Mounts:         []modelstores.Mount{{VolumeID: "vol-1", MountPath: "/models"}},
		}, nil
	}}
	svc := provisioners.New([]provisioners.Provider{prov}, store, "default",
		provisioners.WithKeyStore(newKeyStore(t)),
		provisioners.WithModelStore(ms),
	)
	resp, err := svc.CreateDeployment(context.Background(), multiReplicaCreateReq("warm", 2))
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	mounts := resp.GetDeployment().GetMounts()
	if len(mounts) != 1 || mounts[0].GetVolumeId() != "vol-1" || mounts[0].GetMountPath() != "/models" {
		t.Errorf("multi-replica deployment should carry ModelStore mounts; got %+v", mounts)
	}
}

// TestFanOut_OneExecutorFails: 3 replicas, r1's executor returns an
// error. Expectation: DEGRADED with 2 healthy slots populated, r1's
// slot empty, failure_reason names r1.
func TestFanOut_OneExecutorFails(t *testing.T) {
	svc, _ := fanOutMultiReplicaSvc(t, func(inst *provisionerv1.Instance, emit func(provisioners.DeployStateUpdate)) error {
		if inst.GetId() == "part-r1" {
			return testErr("simulated executor failure")
		}
		emit(provisioners.DeployStateUpdate{
			State:          provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
			ContainerID:    inst.GetId() + "-container",
			EngineEndpoint: "http://" + inst.GetId() + ":8000",
		})
		return nil
	})

	resp, err := svc.CreateDeployment(context.Background(), multiReplicaCreateReq("part", 3))
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	dep := resp.GetDeployment()
	if dep.GetState() != provisionerv1.DeploymentState_DEPLOYMENT_STATE_DEGRADED {
		t.Errorf("state = %s, want DEGRADED", dep.GetState())
	}
	eps := dep.GetEngineEndpoints()
	if len(eps) != 3 {
		t.Fatalf("expected 3 endpoint slots, got %d", len(eps))
	}
	if eps[0] != "http://part-r0:8000" || eps[2] != "http://part-r2:8000" {
		t.Errorf("healthy slots wrong: %v", eps)
	}
	if eps[1] != "" {
		t.Errorf("failed slot should be empty, got %q", eps[1])
	}
	if !strings.Contains(dep.GetFailureReason(), "1 of 3") {
		t.Errorf("failure_reason should mention 1 of 3: %q", dep.GetFailureReason())
	}
	if !strings.Contains(dep.GetFailureReason(), "part-r1") {
		t.Errorf("failure_reason should name r1: %q", dep.GetFailureReason())
	}
}

// TestFanOut_AllExecutorsFail: every executor errors -> FAILED with
// all 3 slots empty, failure_reason names every replica.
func TestFanOut_AllExecutorsFail(t *testing.T) {
	svc, _ := fanOutMultiReplicaSvc(t, func(inst *provisionerv1.Instance, _ func(provisioners.DeployStateUpdate)) error {
		return testErr("simulated " + inst.GetId() + " failure")
	})

	resp, err := svc.CreateDeployment(context.Background(), multiReplicaCreateReq("doomed", 3))
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	dep := resp.GetDeployment()
	if dep.GetState() != provisionerv1.DeploymentState_DEPLOYMENT_STATE_FAILED {
		t.Errorf("state = %s, want FAILED", dep.GetState())
	}
	for i, ep := range dep.GetEngineEndpoints() {
		if ep != "" {
			t.Errorf("slot %d should be empty on all-fail, got %q", i, ep)
		}
	}
	if !strings.Contains(dep.GetFailureReason(), "all 3") {
		t.Errorf("failure_reason should mention all 3: %q", dep.GetFailureReason())
	}
}

// TestFanOut_WaitFalse_ReturnsPending: wait=false returns PENDING
// immediately; the deployment transitions to RUNNING asynchronously
// once executors finish. Tests the async dispatch path.
func TestFanOut_WaitFalse_ReturnsPending(t *testing.T) {
	// Block executors until the test signals completion so we can
	// observe PENDING before any replica has reported.
	release := make(chan struct{})
	svc, _ := fanOutMultiReplicaSvc(t, func(inst *provisionerv1.Instance, emit func(provisioners.DeployStateUpdate)) error {
		<-release
		emit(provisioners.DeployStateUpdate{
			State:          provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
			EngineEndpoint: "http://" + inst.GetId() + ":8000",
		})
		return nil
	})

	req := multiReplicaCreateReq("async", 3)
	req.Wait = false
	resp, err := svc.CreateDeployment(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	if resp.GetDeployment().GetState() != provisionerv1.DeploymentState_DEPLOYMENT_STATE_PENDING {
		t.Errorf("wait=false initial state = %s, want PENDING", resp.GetDeployment().GetState())
	}
	close(release)
	// Poll Describe until aggregate state lands or timeout.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		d, _ := svc.DescribeDeployment(context.Background(), &provisionerv1.DescribeDeploymentRequest{Id: "async"})
		if d != nil && d.GetDeployment().GetState() == provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("aggregate state never reached RUNNING after wait=false")
}

// TestFanOut_RejectsInstanceIdWithReplicas: deployment.instance_id +
// replicas>1 is incoherent (multi-replica auto-provisions; pinned
// instance applies only to single-instance). Rejected at validation.
func TestFanOut_RejectsInstanceIdWithReplicas(t *testing.T) {
	svc, _ := fanOutMultiReplicaSvc(t, nil)
	req := multiReplicaCreateReq("bad", 3)
	req.Deployment.InstanceId = "pinned-pod"
	_, err := svc.CreateDeployment(context.Background(), req)
	if err == nil {
		t.Fatal("expected InvalidArgument for instance_id + replicas>1")
	}
	if !strings.Contains(err.Error(), "instance_id cannot be combined with replicas_spec") {
		t.Errorf("error should explain the conflict: %v", err)
	}
}

// TestFanOut_RejectsInstanceIdsWithReplicas: deployment.instance_ids
// (heterogeneous-fleet specification) + replicas>1 -- the fan-out
// synthesizes ids deterministically, operator-supplied lists land
// in #143.
func TestFanOut_RejectsInstanceIdsWithReplicas(t *testing.T) {
	svc, _ := fanOutMultiReplicaSvc(t, nil)
	req := multiReplicaCreateReq("bad", 3)
	req.Deployment.InstanceIds = []string{"a", "b", "c"}
	_, err := svc.CreateDeployment(context.Background(), req)
	if err == nil {
		t.Fatal("expected InvalidArgument for instance_ids + replicas>1")
	}
	if !strings.Contains(err.Error(), "instance_ids cannot be combined") {
		t.Errorf("error should explain the conflict: %v", err)
	}
}

// TestFanOut_Replicas1_UsesSingleInstancePath: --replicas 1 stays on
// the single-instance code path (no -r0 suffix, no per-slot
// arrays). Beat 1+2 behavior preserved exactly.
func TestFanOut_Replicas1_UsesSingleInstancePath(t *testing.T) {
	svc, store := fanOutMultiReplicaSvc(t, nil)
	req := multiReplicaCreateReq("single", 1)
	resp, err := svc.CreateDeployment(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	dep := resp.GetDeployment()
	if dep.GetState() != provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING {
		t.Errorf("state = %s, want RUNNING", dep.GetState())
	}
	// Single-instance path uses the deploy id verbatim (no -r0 suffix).
	if dep.GetInstanceId() != "single" {
		t.Errorf("instance_id = %q, want single", dep.GetInstanceId())
	}
	// The single-instance path populates engine_endpoints as a 1-slot
	// list via patchDeployment (Beat 3.1 / #84 behavior). The list
	// should match the singular engine_endpoint.
	if eps := dep.GetEngineEndpoints(); len(eps) != 1 || eps[0] == "" {
		t.Errorf("expected 1-slot engine_endpoints, got %v", eps)
	}
	// No -r0 Instance record.
	state, _ := store.Read()
	if _, ok := state.Instances["single-r0"]; ok {
		t.Errorf("single-instance path should not synthesize -r0 record")
	}
}

// testErr is an error helper. testify isn't in scope here, so we
// build a simple error type for failure-injection paths.
type testErrString string

func (e testErrString) Error() string { return string(e) }
func testErr(s string) error          { return testErrString(s) }

// A provider that panics used to take the daemon with it: launchReplica
// runs Deploy in a bare goroutine, so a nil dereference inside an adapter
// is a process-level crash rather than a failed replica. It happened for
// real on the Vast deployer (#392), at a line twelve above the call that
// rents the machine.
//
// The direction that matters is money, not tidiness. A panic past the rent
// kills the control plane while it holds a rented box whose contract id was
// never written down, so nothing knows to destroy it (#393).
func TestFanOutContainsAProviderPanic(t *testing.T) {
	svc, _ := fanOutMultiReplicaSvc(t, func(inst *provisionerv1.Instance, emit func(provisioners.DeployStateUpdate)) error {
		if inst.GetId() == "boom-r1" {
			var offer *struct{ ID int }
			_ = offer.ID // the shape of the real one: a nil deref mid-deploy
		}
		emit(provisioners.DeployStateUpdate{
			State:          provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
			ContainerID:    inst.GetId() + "-container",
			EngineEndpoint: "http://" + inst.GetId() + ":8000",
		})
		return nil
	})

	resp, err := svc.CreateDeployment(context.Background(), multiReplicaCreateReq("boom", 3))
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}

	dep := resp.GetDeployment()
	// One failed replica out of three, which is the same answer an ordinary
	// error from the same adapter would have produced.
	if dep.GetState() != provisionerv1.DeploymentState_DEPLOYMENT_STATE_DEGRADED {
		t.Errorf("state = %s, want DEGRADED", dep.GetState())
	}
	if eps := dep.GetEngineEndpoints(); len(eps) != 3 || eps[1] != "" {
		t.Errorf("slots wrong: %v", eps)
	}
	// And the operator is told which replica died and that it panicked,
	// since a panic is a bug report rather than a capacity problem.
	if !strings.Contains(dep.GetFailureReason(), "boom-r1") {
		t.Errorf("failure_reason should name the replica: %q", dep.GetFailureReason())
	}
	if !strings.Contains(dep.GetFailureReason(), "panic") {
		t.Errorf("failure_reason should say it panicked: %q", dep.GetFailureReason())
	}
}
