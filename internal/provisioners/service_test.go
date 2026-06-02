package provisioners_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/deployments/sshdocker"
	"github.com/inference-book/inference-plane/internal/modelstores"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/stores/file"
	"github.com/inference-book/inference-plane/internal/sshkeys"
	"github.com/panyam/oneauth/keys"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// mockKeyRegistrarProvider is mockProvider + a KeyRegistrar
// implementation. Tests pick this struct (vs mockProvider) when they
// want the provider to be ensured-with-a-key before Spawn.
type mockKeyRegistrarProvider struct {
	*mockProvider
	registrarCalls int
	registrarErr   error
	lastPubKey     []byte
	lastComment    string
}

func (m *mockKeyRegistrarProvider) EnsurePublicKey(ctx context.Context, publicKey []byte, comment string) error {
	m.registrarCalls++
	m.lastPubKey = append([]byte(nil), publicKey...)
	m.lastComment = comment
	return m.registrarErr
}

func newKeyStore(t *testing.T) *sshkeys.Store {
	t.Helper()
	s, err := sshkeys.New(
		sshkeys.WithKeyStorage(keys.NewInMemoryKeyStore()),
		sshkeys.WithClock(func() time.Time { return time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC) }),
	)
	if err != nil {
		t.Fatalf("sshkeys.New: %v", err)
	}
	return s
}

// mockProvider lets each test wire up the four Provider methods
// independently. Unsupplied methods default to safe no-ops.
type mockProvider struct {
	name     string
	spawn    func(ctx context.Context, spec *provisionerv1.Spec) (*provisionerv1.Instance, error)
	term     func(ctx context.Context, providerID string) error
	describe func(ctx context.Context, providerID string) (*provisionerv1.Instance, error)
	list     func(ctx context.Context, filter map[string]string) ([]*provisionerv1.InstanceRef, error)

	spawnCalls int
	termCalls  int
}

func (m *mockProvider) Name() string { return m.name }

func (m *mockProvider) Spawn(ctx context.Context, spec *provisionerv1.Spec) (*provisionerv1.Instance, error) {
	m.spawnCalls++
	if m.spawn != nil {
		return m.spawn(ctx, spec)
	}
	return &provisionerv1.Instance{
		Id:         spec.GetId(),
		ProviderId: "mock:" + spec.GetId(),
		Provider:   m.name,
		Spec:       spec,
		Region:     spec.GetRegion(),
		State:      provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE,
	}, nil
}

func (m *mockProvider) Terminate(ctx context.Context, providerID string) error {
	m.termCalls++
	if m.term != nil {
		return m.term(ctx, providerID)
	}
	return nil
}

func (m *mockProvider) Describe(ctx context.Context, providerID string) (*provisionerv1.Instance, error) {
	if m.describe != nil {
		return m.describe(ctx, providerID)
	}
	return nil, provisioners.NewProviderError(m.name, "describe", provisioners.ErrNotFound, 0)
}

func (m *mockProvider) List(ctx context.Context, filter map[string]string) ([]*provisionerv1.InstanceRef, error) {
	if m.list != nil {
		return m.list(ctx, filter)
	}
	return nil, nil
}

func newSvc(t *testing.T, provs ...provisioners.Provider) (*provisioners.Service, *file.Store) {
	t.Helper()
	store, err := file.Open(t.TempDir(), "default")
	if err != nil {
		t.Fatalf("file.Open: %v", err)
	}
	clock := func() time.Time { return time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC) }
	svc := provisioners.New(provs, store, "default", provisioners.WithClock(clock))
	return svc, store
}

func okSpec() *provisionerv1.Spec {
	return &provisionerv1.Spec{
		Id:       "my-pod",
		Provider: "mock",
		Region:   "us-ca-1",
		Requirements: &provisionerv1.ResourceRequirements{
			Class:    provisioners.GPUClassSmall,
			GpuCount: 1,
		},
	}
}

func TestCreateInstance_HappyPath(t *testing.T) {
	mock := &mockProvider{name: "mock"}
	svc, _ := newSvc(t, mock)

	resp, err := svc.CreateInstance(context.Background(), &provisionerv1.CreateInstanceRequest{Spec: okSpec()})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if resp.GetAlreadyExisted() {
		t.Error("AlreadyExisted should be false on first create")
	}
	if mock.spawnCalls != 1 {
		t.Errorf("Spawn called %d times, want 1", mock.spawnCalls)
	}
	got := resp.GetInstance()
	if got.GetState() != provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE {
		t.Errorf("State = %v, want ACTIVE", got.GetState())
	}
	if got.GetId() != "my-pod" {
		t.Errorf("Id = %q, want my-pod", got.GetId())
	}
}

func TestCreateInstance_IdempotentOnActive(t *testing.T) {
	mock := &mockProvider{name: "mock"}
	svc, _ := newSvc(t, mock)

	for range 3 {
		_, err := svc.CreateInstance(context.Background(), &provisionerv1.CreateInstanceRequest{Spec: okSpec()})
		if err != nil {
			t.Fatalf("CreateInstance: %v", err)
		}
	}
	if mock.spawnCalls != 1 {
		t.Errorf("Spawn called %d times, want 1 (subsequent creates should hit local-state idempotency)", mock.spawnCalls)
	}
	resp, _ := svc.CreateInstance(context.Background(), &provisionerv1.CreateInstanceRequest{Spec: okSpec()})
	if !resp.GetAlreadyExisted() {
		t.Error("AlreadyExisted should be true on repeat create")
	}
}

func TestCreateInstance_CrossProviderCollisionRejected(t *testing.T) {
	mock1 := &mockProvider{name: "mock"}
	mock2 := &mockProvider{name: "other"}
	svc, _ := newSvc(t, mock1, mock2)

	if _, err := svc.CreateInstance(context.Background(), &provisionerv1.CreateInstanceRequest{Spec: okSpec()}); err != nil {
		t.Fatalf("first create: %v", err)
	}
	conflicting := okSpec()
	conflicting.Provider = "other"
	_, err := svc.CreateInstance(context.Background(), &provisionerv1.CreateInstanceRequest{Spec: conflicting})
	if err == nil {
		t.Fatal("expected error when creating same id on different provider")
	}
}

func TestCreateInstance_InvalidID(t *testing.T) {
	mock := &mockProvider{name: "mock"}
	svc, _ := newSvc(t, mock)

	cases := []string{
		"",                // empty
		"iplane-foo",      // reserved prefix
		"With_Capitals",   // not DNS-safe
		"-leading-hyphen", // leading hyphen
	}
	for _, id := range cases {
		spec := okSpec()
		spec.Id = id
		_, err := svc.CreateInstance(context.Background(), &provisionerv1.CreateInstanceRequest{Spec: spec})
		if err == nil {
			t.Errorf("id %q should be rejected", id)
		}
	}
}

func TestCreateInstance_RequirementsClassSkuMutex(t *testing.T) {
	mock := &mockProvider{name: "mock"}
	svc, _ := newSvc(t, mock)

	spec := okSpec()
	spec.Requirements = &provisionerv1.ResourceRequirements{Class: "small", Sku: "RTX 4090"}
	_, err := svc.CreateInstance(context.Background(), &provisionerv1.CreateInstanceRequest{Spec: spec})
	if err == nil {
		t.Error("specifying both class and sku should be rejected")
	}
}

func TestCreateInstance_ClassShorthandExpandsToConstraints(t *testing.T) {
	// When the operator passes class=small without any explicit
	// numeric constraints, the service should fill them in from the
	// class defaults before dispatching to the provider. We observe
	// this by capturing the spec the provider sees.
	var observed *provisionerv1.Spec
	mock := &mockProvider{
		name: "mock",
		spawn: func(ctx context.Context, spec *provisionerv1.Spec) (*provisionerv1.Instance, error) {
			observed = spec
			return &provisionerv1.Instance{Id: spec.GetId(), ProviderId: "mock:" + spec.GetId(), Provider: "mock", Spec: spec, State: provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE}, nil
		},
	}
	svc, _ := newSvc(t, mock)
	if _, err := svc.CreateInstance(context.Background(), &provisionerv1.CreateInstanceRequest{Spec: okSpec()}); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if observed.GetRequirements().GetMinVramGb() != 24 {
		t.Errorf("class=small should expand to min_vram_gb=24, got %d", observed.GetRequirements().GetMinVramGb())
	}
	if observed.GetRequirements().GetMinDiskGb() != 20 {
		t.Errorf("class=small should expand to min_disk_gb=20, got %d", observed.GetRequirements().GetMinDiskGb())
	}
	if observed.GetRequirements().GetMinRamGb() != 16 {
		t.Errorf("class=small should expand to min_ram_gb=16, got %d", observed.GetRequirements().GetMinRamGb())
	}
}

func TestCreateInstance_SpawnFailure_RecordsFailedState(t *testing.T) {
	wantErr := provisioners.NewProviderError("mock", "spawn", errors.New("quota exceeded"), 429)
	mock := &mockProvider{
		name:  "mock",
		spawn: func(ctx context.Context, spec *provisionerv1.Spec) (*provisionerv1.Instance, error) { return nil, wantErr },
	}
	svc, store := newSvc(t, mock)

	_, err := svc.CreateInstance(context.Background(), &provisionerv1.CreateInstanceRequest{Spec: okSpec()})
	if err == nil {
		t.Fatal("expected error from Spawn failure")
	}
	// The Service wraps Spawn errors as gRPC status (codes.Unknown
	// preserves the message). The wrapped *ProviderError is no longer
	// reachable through errors.As across the status boundary -- the
	// state-file record + failure_reason are the load-bearing signal
	// for operator-facing failure reporting.
	f, _ := store.Read()
	rec, ok := f.Instances["my-pod"]
	if !ok {
		t.Fatal("failed Spawn should leave a record (state=FAILED) for operator visibility")
	}
	if rec.GetState() != provisionerv1.InstanceState_INSTANCE_STATE_FAILED {
		t.Errorf("State = %v, want FAILED", rec.GetState())
	}
	if rec.GetFailureReason() == "" {
		t.Error("FailureReason should be populated")
	}
}

func TestCreateInstance_TagsStampedOnSpawn(t *testing.T) {
	var observedSpec *provisionerv1.Spec
	mock := &mockProvider{
		name: "mock",
		spawn: func(ctx context.Context, spec *provisionerv1.Spec) (*provisionerv1.Instance, error) {
			observedSpec = spec
			return &provisionerv1.Instance{Id: spec.GetId(), ProviderId: "mock:" + spec.GetId(), Provider: "mock", Spec: spec, State: provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE}, nil
		},
	}
	svc, _ := newSvc(t, mock)

	if _, err := svc.CreateInstance(context.Background(), &provisionerv1.CreateInstanceRequest{Spec: okSpec()}); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if observedSpec.GetTags()[provisioners.TagID] != "my-pod" {
		t.Errorf("Spawn spec missing iplane-id tag: %v", observedSpec.GetTags())
	}
	if observedSpec.GetTags()[provisioners.TagOperator] != "default" {
		t.Errorf("Spawn spec missing iplane-operator tag: %v", observedSpec.GetTags())
	}
}

func TestDestroyInstance_HappyPath(t *testing.T) {
	mock := &mockProvider{name: "mock"}
	svc, store := newSvc(t, mock)

	if _, err := svc.CreateInstance(context.Background(), &provisionerv1.CreateInstanceRequest{Spec: okSpec()}); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	resp, err := svc.DestroyInstance(context.Background(), &provisionerv1.DestroyInstanceRequest{Id: "my-pod"})
	if err != nil {
		t.Fatalf("DestroyInstance: %v", err)
	}
	if resp.GetInstance().GetState() != provisionerv1.InstanceState_INSTANCE_STATE_TERMINATED {
		t.Errorf("State = %v, want TERMINATED", resp.GetInstance().GetState())
	}
	if mock.termCalls != 1 {
		t.Errorf("Terminate called %d times, want 1", mock.termCalls)
	}
	f, _ := store.Read()
	if f.Instances["my-pod"].GetTerminatedAt() == nil {
		t.Error("TerminatedAt should be set")
	}
}

func TestDestroyInstance_NotFound(t *testing.T) {
	svc, _ := newSvc(t, &mockProvider{name: "mock"})
	_, err := svc.DestroyInstance(context.Background(), &provisionerv1.DestroyInstanceRequest{Id: "ghost"})
	if err == nil {
		t.Fatal("DestroyInstance of unknown id should error")
	}
}

func TestDestroyInstance_Force_SkipsProviderCall(t *testing.T) {
	mock := &mockProvider{name: "mock"}
	svc, _ := newSvc(t, mock)

	if _, err := svc.CreateInstance(context.Background(), &provisionerv1.CreateInstanceRequest{Spec: okSpec()}); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	_, err := svc.DestroyInstance(context.Background(), &provisionerv1.DestroyInstanceRequest{Id: "my-pod", Force: true})
	if err != nil {
		t.Fatalf("DestroyInstance: %v", err)
	}
	if mock.termCalls != 0 {
		t.Errorf("Terminate called %d times with --force, want 0", mock.termCalls)
	}
}

func TestListInstances_LocalReturnsRecords(t *testing.T) {
	mock := &mockProvider{name: "mock"}
	svc, _ := newSvc(t, mock)

	for _, id := range []string{"pod-a", "pod-b", "pod-c"} {
		spec := okSpec()
		spec.Id = id
		if _, err := svc.CreateInstance(context.Background(), &provisionerv1.CreateInstanceRequest{Spec: spec}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	resp, err := svc.ListInstances(context.Background(), &provisionerv1.ListInstancesRequest{})
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if len(resp.GetInstances()) != 3 {
		t.Errorf("got %d instances, want 3", len(resp.GetInstances()))
	}
}

func TestListInstances_SelfHealsPending(t *testing.T) {
	// Seed a pending record manually (simulating a crashed prior
	// CreateInstance), then verify List promotes it to ACTIVE when the
	// provider says yes.
	mock := &mockProvider{
		name: "mock",
		list: func(ctx context.Context, filter map[string]string) ([]*provisionerv1.InstanceRef, error) {
			return []*provisionerv1.InstanceRef{{
				ProviderId:    "mock:my-pod",
				ProviderState: "running",
				Tags:          filter,
			}}, nil
		},
		describe: func(ctx context.Context, providerID string) (*provisionerv1.Instance, error) {
			return &provisionerv1.Instance{
				Id:         "my-pod",
				ProviderId: providerID,
				Provider:   "mock",
				State:      provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE,
			}, nil
		},
	}
	svc, store := newSvc(t, mock)

	// Hand-seed the pending record.
	if err := store.Update(func(f *provisioners.State) error {
		f.Instances["my-pod"] = &provisionerv1.Instance{
			Id:        "my-pod",
			Provider:  "mock",
			Spec:      okSpec(),
			State:     provisionerv1.InstanceState_INSTANCE_STATE_PENDING,
			CreatedAt: timestamppb.Now(),
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Note: the mock's list returns a non-runpod state ("running") that
	// service.isActiveLikeProviderState only treats as active for
	// ProviderRunPod. For v0.1 phase 1.2, isActiveLikeProviderState
	// returns false for "mock" -- so the self-heal path adopts via the
	// pending->describe code path. Verify the test by reading state
	// post-list and checking the result.
	if _, err := svc.ListInstances(context.Background(), &provisionerv1.ListInstancesRequest{}); err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	f, _ := store.Read()
	got := f.Instances["my-pod"]
	if got.GetState() != provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE {
		t.Errorf("State after self-heal = %v, want ACTIVE", got.GetState())
	}
}

// mockSSHWaiter is mockProvider + the SSHReadyWaiter capability.
// Tests with this provider exercise the WaitForInstanceReady path.
type mockSSHWaiter struct {
	*mockProvider
	waitCalls  int
	waitTarget *provisionerv1.SshTarget
	waitErr    error
}

func (m *mockSSHWaiter) WaitForSSHReady(ctx context.Context, providerID string) (*provisionerv1.SshTarget, error) {
	m.waitCalls++
	if m.waitErr != nil {
		return nil, m.waitErr
	}
	return m.waitTarget, nil
}

func TestWaitForInstanceReady_NotFound(t *testing.T) {
	svc, _ := newSvc(t, &mockProvider{name: "mock"})
	_, err := svc.WaitForInstanceReady(context.Background(), &provisionerv1.WaitForInstanceReadyRequest{Id: "nope"})
	if err == nil {
		t.Fatal("expected NotFound for unknown id")
	}
}

func TestWaitForInstanceReady_AlreadyReady(t *testing.T) {
	// Instance already has SSH populated in state -- the Service must
	// short-circuit without touching the provider so retries are cheap.
	waiter := &mockSSHWaiter{mockProvider: &mockProvider{name: "mock"}}
	svc, store := newSvc(t, waiter)
	_ = store.Update(func(f *provisioners.State) error {
		f.Instances["my-pod"] = &provisionerv1.Instance{
			Id: "my-pod", Provider: "mock", ProviderId: "mock:my-pod",
			State: provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE,
			Ssh:   &provisionerv1.SshTarget{Host: "1.2.3.4", Port: 22, User: "root"},
		}
		return nil
	})
	resp, err := svc.WaitForInstanceReady(context.Background(), &provisionerv1.WaitForInstanceReadyRequest{Id: "my-pod"})
	if err != nil {
		t.Fatalf("WaitForInstanceReady: %v", err)
	}
	if !resp.GetAlreadyReady() {
		t.Errorf("expected already_ready=true; got false")
	}
	if waiter.waitCalls != 0 {
		t.Errorf("provider waiter should not have been called; got %d calls", waiter.waitCalls)
	}
}

func TestWaitForInstanceReady_ProviderWithoutCapability_NoOp(t *testing.T) {
	// Provider doesn't implement SSHReadyWaiter (e.g. local). The
	// Service returns the current Instance unchanged; not an error.
	svc, store := newSvc(t, &mockProvider{name: "mock"})
	_ = store.Update(func(f *provisioners.State) error {
		f.Instances["my-pod"] = &provisionerv1.Instance{
			Id: "my-pod", Provider: "mock", ProviderId: "mock:my-pod",
			State: provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE,
			// No SSH set.
		}
		return nil
	})
	resp, err := svc.WaitForInstanceReady(context.Background(), &provisionerv1.WaitForInstanceReadyRequest{Id: "my-pod"})
	if err != nil {
		t.Fatalf("WaitForInstanceReady: %v", err)
	}
	if !resp.GetAlreadyReady() {
		t.Errorf("expected already_ready=true for provider w/o SSH capability; got false")
	}
}

func TestWaitForInstanceReady_PopulatesSSH(t *testing.T) {
	waiter := &mockSSHWaiter{
		mockProvider: &mockProvider{name: "mock"},
		waitTarget:   &provisionerv1.SshTarget{Host: "5.6.7.8", Port: 22, User: "root"},
	}
	svc, store := newSvc(t, waiter)
	_ = store.Update(func(f *provisioners.State) error {
		f.Instances["my-pod"] = &provisionerv1.Instance{
			Id: "my-pod", Provider: "mock", ProviderId: "mock:my-pod",
			State: provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE,
		}
		return nil
	})
	resp, err := svc.WaitForInstanceReady(context.Background(), &provisionerv1.WaitForInstanceReadyRequest{Id: "my-pod"})
	if err != nil {
		t.Fatalf("WaitForInstanceReady: %v", err)
	}
	if resp.GetAlreadyReady() {
		t.Errorf("expected already_ready=false after provider call; got true")
	}
	if ssh := resp.GetInstance().GetSsh(); ssh == nil || ssh.GetHost() != "5.6.7.8" {
		t.Errorf("ssh.host = %+v, want 5.6.7.8", ssh)
	}
	if waiter.waitCalls != 1 {
		t.Errorf("provider waiter called %d times; want 1", waiter.waitCalls)
	}
	// And the state file must reflect the new SSH.
	f, _ := store.Read()
	if f.Instances["my-pod"].GetSsh().GetHost() != "5.6.7.8" {
		t.Errorf("state file ssh not patched; got %+v", f.Instances["my-pod"].GetSsh())
	}
}

func TestWaitForInstanceReady_ProviderError_Surfaces(t *testing.T) {
	waiter := &mockSSHWaiter{
		mockProvider: &mockProvider{name: "mock"},
		waitErr:      errors.New("transient runpod failure"),
	}
	svc, store := newSvc(t, waiter)
	_ = store.Update(func(f *provisioners.State) error {
		f.Instances["my-pod"] = &provisionerv1.Instance{
			Id: "my-pod", Provider: "mock", ProviderId: "mock:my-pod",
			State: provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE,
		}
		return nil
	})
	_, err := svc.WaitForInstanceReady(context.Background(), &provisionerv1.WaitForInstanceReadyRequest{Id: "my-pod"})
	if err == nil {
		t.Fatal("expected error from provider waiter to surface")
	}
}

func TestGetInstanceSSHKey_NotFound(t *testing.T) {
	svc, _ := newSvc(t, &mockProvider{name: "mock"})
	_, err := svc.GetInstanceSSHKey(context.Background(), &provisionerv1.GetInstanceSSHKeyRequest{Id: "nope"})
	if err == nil {
		t.Fatal("expected NotFound for unknown id")
	}
}

func TestGetInstanceSSHKey_NoKeyStoreConfigured(t *testing.T) {
	// Service was constructed without WithKeyStore -- the key-fetch
	// RPC should refuse with a clear FailedPrecondition. Operators
	// can't extract a key from a Service that never managed one.
	svc, _ := newSvc(t, &mockProvider{name: "mock"})
	_, err := svc.GetInstanceSSHKey(context.Background(), &provisionerv1.GetInstanceSSHKeyRequest{Id: "my-pod"})
	if err == nil {
		t.Fatal("expected FailedPrecondition when keyStore is nil")
	}
	if !strings.Contains(err.Error(), "ssh key store not configured") {
		t.Errorf("error %q should mention missing key store", err)
	}
}

func TestGetInstanceSSHKey_HappyPath(t *testing.T) {
	store, err := file.Open(t.TempDir(), "default")
	if err != nil {
		t.Fatalf("file.Open: %v", err)
	}
	svc := provisioners.New([]provisioners.Provider{&mockProvider{name: "mock"}},
		store, "default",
		provisioners.WithKeyStore(newKeyStore(t)))

	_ = store.Update(func(f *provisioners.State) error {
		f.Instances["my-pod"] = &provisionerv1.Instance{
			Id: "my-pod", Provider: "mock", ProviderId: "mock:my-pod",
			State: provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE,
			Ssh:   &provisionerv1.SshTarget{Host: "1.2.3.4", Port: 22, User: "root"},
		}
		return nil
	})

	resp, err := svc.GetInstanceSSHKey(context.Background(), &provisionerv1.GetInstanceSSHKeyRequest{Id: "my-pod"})
	if err != nil {
		t.Fatalf("GetInstanceSSHKey: %v", err)
	}
	if len(resp.GetPrivateKeyPem()) == 0 {
		t.Error("expected private_key_pem bytes; got empty")
	}
	if !strings.HasPrefix(string(resp.GetPrivateKeyPem()), "-----BEGIN") {
		t.Errorf("private_key_pem should be PEM-encoded; got: %q", string(resp.GetPrivateKeyPem())[:40])
	}
	if len(resp.GetPublicKeyAuthorized()) == 0 {
		t.Error("expected public_key_authorized bytes; got empty")
	}
	if resp.GetUser() != "root" {
		t.Errorf("user = %q, want root", resp.GetUser())
	}
}

// mockDeployerProvider is mockProvider + the Deployer capability. Its
// Deploy/Destroy methods record that they were invoked AND emit a
// RUNNING / TERMINATED update so the Service's launchDeploy
// goroutine completes.
type mockDeployerProvider struct {
	*mockProvider
	deployCalls  int
	destroyCalls int
	// deployFn overrides the default healthy emit-RUNNING behavior;
	// nil means "use default."
	deployFn func(emit func(provisioners.DeployStateUpdate)) error
}

func (m *mockDeployerProvider) Deploy(ctx context.Context, dep *provisionerv1.Deployment, inst *provisionerv1.Instance, key *sshkeys.KeyPair, emit func(provisioners.DeployStateUpdate)) error {
	m.deployCalls++
	if m.deployFn != nil {
		return m.deployFn(emit)
	}
	emit(provisioners.DeployStateUpdate{
		State:          provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
		Phase:          "test:provider-deployer",
		ContainerID:    "mock-pod-1",
		EngineEndpoint: "http://1.2.3.4:8000",
	})
	return nil
}

func (m *mockDeployerProvider) Destroy(ctx context.Context, dep *provisionerv1.Deployment, inst *provisionerv1.Instance, key *sshkeys.KeyPair, emit func(provisioners.DeployStateUpdate)) error {
	m.destroyCalls++
	emit(provisioners.DeployStateUpdate{
		State: provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATED,
		Phase: "test:provider-deployer",
	})
	return nil
}

// recordingExecutor satisfies provisioners.DeploymentExecutor and
// records that its Deploy / Destroy methods were called. Used to
// verify that the Service dispatcher only falls back to the
// configured executor when the provider does NOT implement Deployer.
type recordingExecutor struct {
	deployCalls  int
	destroyCalls int
}

func (r *recordingExecutor) Deploy(ctx context.Context, dep *provisionerv1.Deployment, inst *provisionerv1.Instance, key *sshkeys.KeyPair, emit func(provisioners.DeployStateUpdate)) error {
	r.deployCalls++
	emit(provisioners.DeployStateUpdate{State: provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING})
	return nil
}

func (r *recordingExecutor) Destroy(ctx context.Context, dep *provisionerv1.Deployment, inst *provisionerv1.Instance, key *sshkeys.KeyPair, emit func(provisioners.DeployStateUpdate)) error {
	r.destroyCalls++
	emit(provisioners.DeployStateUpdate{State: provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATED})
	return nil
}

func TestDeployerDispatch_ProviderCapability_TakesPrecedence(t *testing.T) {
	// Provider implements Deployer; configured executor exists too.
	// Service must dispatch to provider.Deployer, NOT the fallback.
	deployer := &mockDeployerProvider{mockProvider: &mockProvider{name: "mock"}}
	fallback := &recordingExecutor{}

	store, err := file.Open(t.TempDir(), "default")
	if err != nil {
		t.Fatalf("file.Open: %v", err)
	}
	svc := provisioners.New([]provisioners.Provider{deployer},
		store, "default",
		provisioners.WithKeyStore(newKeyStore(t)),
		provisioners.WithDeploymentExecutor(fallback))

	// Seed an active instance the deployment can reference.
	_ = store.Update(func(f *provisioners.State) error {
		f.Instances["my-pod"] = &provisionerv1.Instance{
			Id: "my-pod", Provider: "mock", ProviderId: "mock:my-pod",
			State: provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE,
			Gpu:   &provisionerv1.GpuInfo{Sku: "mock-sku"},
			Ssh:   &provisionerv1.SshTarget{Host: "1.2.3.4", Port: 22, User: "root"},
		}
		return nil
	})

	resp, err := svc.CreateDeployment(context.Background(), &provisionerv1.CreateDeploymentRequest{
		Deployment: &provisionerv1.Deployment{
			Id:         "my-llama",
			InstanceId: "my-pod",
			Image:      "vllm/vllm-openai:v0.7.0",
			Model:      "Qwen/Qwen2.5-1.5B-Instruct",
			EnginePort: 8000,
		},
		Wait: true,
	})
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	if deployer.deployCalls != 1 {
		t.Errorf("provider.Deploy calls = %d, want 1", deployer.deployCalls)
	}
	if fallback.deployCalls != 0 {
		t.Errorf("fallback executor.Deploy calls = %d, want 0 (provider Deployer should win)", fallback.deployCalls)
	}
	if resp.GetDeployment().GetState() != provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING {
		t.Errorf("final state = %v, want RUNNING", resp.GetDeployment().GetState())
	}
}

func TestDeployerDispatch_NoCapability_FallsBackToConfiguredExecutor(t *testing.T) {
	// Provider does NOT implement Deployer; the Service must use the
	// configured executor (the v0.2 path for VM-style providers).
	plain := &mockProvider{name: "mock"}
	fallback := &recordingExecutor{}

	store, err := file.Open(t.TempDir(), "default")
	if err != nil {
		t.Fatalf("file.Open: %v", err)
	}
	svc := provisioners.New([]provisioners.Provider{plain},
		store, "default",
		provisioners.WithKeyStore(newKeyStore(t)),
		provisioners.WithDeploymentExecutor(fallback))

	_ = store.Update(func(f *provisioners.State) error {
		f.Instances["my-pod"] = &provisionerv1.Instance{
			Id: "my-pod", Provider: "mock", ProviderId: "mock:my-pod",
			State: provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE,
			Gpu:   &provisionerv1.GpuInfo{Sku: "mock-sku"},
			Ssh:   &provisionerv1.SshTarget{Host: "1.2.3.4", Port: 22, User: "root"},
		}
		return nil
	})

	_, err = svc.CreateDeployment(context.Background(), &provisionerv1.CreateDeploymentRequest{
		Deployment: &provisionerv1.Deployment{
			Id:         "my-llama",
			InstanceId: "my-pod",
			Image:      "vllm/vllm-openai:v0.7.0",
			Model:      "Qwen/Qwen2.5-1.5B-Instruct",
			EnginePort: 8000,
		},
		Wait: true,
	})
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	if fallback.deployCalls != 1 {
		t.Errorf("fallback executor.Deploy calls = %d, want 1", fallback.deployCalls)
	}
}

func TestCreateDeployment_AutoProvision_RecordsInstanceAnd1to1(t *testing.T) {
	// No instance_id: the scheduler seam auto-provisions a fresh
	// instance dedicated to this deployment. On an image-native (Deployer)
	// provider this must succeed WITHOUT any pre-existing SSH endpoint --
	// the dropped precondition. The synthesized instance shares the
	// deployment id (1:1) and is promoted to ACTIVE once RUNNING.
	deployer := &mockDeployerProvider{mockProvider: &mockProvider{name: "mock"}}

	store, err := file.Open(t.TempDir(), "default")
	if err != nil {
		t.Fatalf("file.Open: %v", err)
	}
	svc := provisioners.New([]provisioners.Provider{deployer},
		store, "default",
		provisioners.WithKeyStore(newKeyStore(t)))

	resp, err := svc.CreateDeployment(context.Background(), &provisionerv1.CreateDeploymentRequest{
		Deployment: &provisionerv1.Deployment{
			Id:         "my-llama",
			Image:      "vllm/vllm-openai:v0.7.0",
			Model:      "Qwen/Qwen2.5-1.5B-Instruct",
			EnginePort: 8000,
		},
		Provider: "mock",
		Requirements: &provisionerv1.ResourceRequirements{
			Class:    provisioners.GPUClassSmall,
			GpuCount: 1,
		},
		Wait: true,
	})
	if err != nil {
		t.Fatalf("CreateDeployment (auto-provision): %v", err)
	}
	if deployer.deployCalls != 1 {
		t.Errorf("provider.Deploy calls = %d, want 1", deployer.deployCalls)
	}
	if deployer.spawnCalls != 0 {
		t.Errorf("Spawn calls = %d, want 0 (image-native deploy provisions the pod itself, not via Spawn)", deployer.spawnCalls)
	}
	dep := resp.GetDeployment()
	if dep.GetState() != provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING {
		t.Errorf("deployment state = %v, want RUNNING", dep.GetState())
	}
	if dep.GetInstanceId() != "my-llama" {
		t.Errorf("deployment.instance_id = %q, want my-llama (1:1)", dep.GetInstanceId())
	}

	file, _ := store.Read()
	inst, ok := file.Instances["my-llama"]
	if !ok {
		t.Fatal("auto-provisioned instance was not recorded")
	}
	if inst.GetState() != provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE {
		t.Errorf("instance state = %v, want ACTIVE after RUNNING deploy", inst.GetState())
	}
	if inst.GetProvider() != "mock" {
		t.Errorf("instance provider = %q, want mock", inst.GetProvider())
	}
}

func TestDestroyDeployment_AutoProvisioned_CascadesToInstance(t *testing.T) {
	// Tearing down a 1:1 auto-provisioned deployment terminates the
	// engine pod, which IS the instance -- so the instance record is
	// terminated too (no orphaned ACTIVE instance left behind).
	deployer := &mockDeployerProvider{mockProvider: &mockProvider{name: "mock"}}

	store, err := file.Open(t.TempDir(), "default")
	if err != nil {
		t.Fatalf("file.Open: %v", err)
	}
	svc := provisioners.New([]provisioners.Provider{deployer},
		store, "default",
		provisioners.WithKeyStore(newKeyStore(t)))

	if _, err := svc.CreateDeployment(context.Background(), &provisionerv1.CreateDeploymentRequest{
		Deployment: &provisionerv1.Deployment{
			Id: "my-llama", Image: "vllm/vllm-openai:v0.7.0",
			Model: "Qwen/Qwen2.5-1.5B-Instruct", EnginePort: 8000,
		},
		Provider:     "mock",
		Requirements: &provisionerv1.ResourceRequirements{Class: provisioners.GPUClassSmall, GpuCount: 1},
		Wait:         true,
	}); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}

	if _, err := svc.DestroyDeployment(context.Background(), &provisionerv1.DestroyDeploymentRequest{Id: "my-llama"}); err != nil {
		t.Fatalf("DestroyDeployment: %v", err)
	}
	if deployer.destroyCalls != 1 {
		t.Errorf("provider.Destroy calls = %d, want 1", deployer.destroyCalls)
	}

	file, _ := store.Read()
	if got := file.Deployments["my-llama"].GetState(); got != provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATED {
		t.Errorf("deployment state = %v, want TERMINATED", got)
	}
	if got := file.Instances["my-llama"].GetState(); got != provisionerv1.InstanceState_INSTANCE_STATE_TERMINATED {
		t.Errorf("instance state = %v, want TERMINATED (cascade)", got)
	}
}

func TestDestroyInstance_AfterDeployFailure_StillTerminatesAtProvider(t *testing.T) {
	// Regression: when Deploy POSTs a pod (emitting ContainerID) but
	// then fails (e.g., engine /health never returns 2xx), finalize is
	// skipped -- the instance stayed PENDING with empty provider_id and
	// `iplane instance destroy` then skipped the provider.Terminate
	// call, leaking the real pod at the provider.
	//
	// Fix: patchDeployment stamps the pod id onto the 1:1 instance's
	// provider_id the moment we learn it (before finalize).
	deployer := &mockDeployerProvider{mockProvider: &mockProvider{name: "mock"}}
	deployer.deployFn = func(emit func(provisioners.DeployStateUpdate)) error {
		// Pod created at provider -- emit ContainerID, then fail.
		emit(provisioners.DeployStateUpdate{
			State:       provisionerv1.DeploymentState_DEPLOYMENT_STATE_CONFIGURING,
			Phase:       "test:pod-created",
			ContainerID: "mock-pod-leaked",
		})
		emit(provisioners.DeployStateUpdate{
			State:         provisionerv1.DeploymentState_DEPLOYMENT_STATE_FAILED,
			Phase:         "test:health-timeout",
			FailureReason: "engine /health never returned 2xx",
		})
		return errors.New("health timeout")
	}

	store, err := file.Open(t.TempDir(), "default")
	if err != nil {
		t.Fatalf("file.Open: %v", err)
	}
	svc := provisioners.New([]provisioners.Provider{deployer},
		store, "default",
		provisioners.WithKeyStore(newKeyStore(t)))

	// Deploy fails, but the pod exists at the provider.
	_, _ = svc.CreateDeployment(context.Background(), &provisionerv1.CreateDeploymentRequest{
		Deployment:   &provisionerv1.Deployment{Id: "my-llama", Image: "vllm/vllm-openai:v0.7.0", Model: "Qwen/Qwen2.5-1.5B-Instruct", EnginePort: 8000},
		Provider:     "mock",
		Requirements: &provisionerv1.ResourceRequirements{Class: provisioners.GPUClassSmall, GpuCount: 1},
		Wait:         true,
	})

	// Sanity: instance now carries the leaked pod id (the fix).
	file, _ := store.Read()
	if got := file.Instances["my-llama"].GetProviderId(); got != "mock-pod-leaked" {
		t.Fatalf("instance.provider_id = %q, want mock-pod-leaked (would leak the real pod if empty)", got)
	}

	// Destroy must reach provider.Terminate -- the whole point.
	if _, err := svc.DestroyInstance(context.Background(), &provisionerv1.DestroyInstanceRequest{Id: "my-llama"}); err != nil {
		t.Fatalf("DestroyInstance: %v", err)
	}
	if deployer.termCalls != 1 {
		t.Errorf("provider.Terminate calls = %d, want 1 (instance destroy must reach the provider, not just patch local state)", deployer.termCalls)
	}
}

func TestWaitForInstanceReady_NoSSHRequested_FailsFast(t *testing.T) {
	// A 1:1 auto-provisioned instance whose deployment was created with
	// debug_shell=false (the cost-aware default) was POSTed to the
	// provider WITHOUT supportPublicIp -- the publicIp is never coming.
	// WaitForInstanceReady must surface that as a permanent
	// FailedPrecondition, not poll a doomed-to-fail provider for 120s.
	mock := &mockProvider{name: "mock"}
	store, err := file.Open(t.TempDir(), "default")
	if err != nil {
		t.Fatalf("file.Open: %v", err)
	}
	svc := provisioners.New([]provisioners.Provider{mock},
		store, "default",
		provisioners.WithKeyStore(newKeyStore(t)))

	_ = store.Update(func(f *provisioners.State) error {
		f.Instances["my-llama"] = &provisionerv1.Instance{
			Id: "my-llama", Provider: "mock",
			State: provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE,
			// No Ssh -- never populated under proxy-only deploy.
		}
		f.Deployments["my-llama"] = &provisionerv1.Deployment{
			Id: "my-llama", InstanceId: "my-llama",
			DebugShell: false, // the cost-aware default
			State:      provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
		}
		return nil
	})

	_, err = svc.WaitForInstanceReady(context.Background(), &provisionerv1.WaitForInstanceReadyRequest{Id: "my-llama"})
	if err == nil {
		t.Fatal("expected FailedPrecondition; got nil error")
	}
	if !strings.Contains(err.Error(), "debug_shell") {
		t.Errorf("error message should mention debug_shell; got: %v", err)
	}
}

func TestDestroyInstance_BackfillsProviderIDFromLinkedDeployment(t *testing.T) {
	// Cleanup path for state files written by an older binary: an
	// auto-provisioned instance with empty provider_id is paired (1:1
	// id) with a deployment carrying the container_id. DestroyInstance
	// must backfill provider_id from the deployment so the provider
	// Terminate call actually fires.
	mock := &mockProvider{name: "mock"}
	store, err := file.Open(t.TempDir(), "default")
	if err != nil {
		t.Fatalf("file.Open: %v", err)
	}
	svc := provisioners.New([]provisioners.Provider{mock},
		store, "default",
		provisioners.WithKeyStore(newKeyStore(t)))

	// Seed: instance has NO provider_id; linked deployment has container_id.
	_ = store.Update(func(f *provisioners.State) error {
		f.Instances["my-llama"] = &provisionerv1.Instance{
			Id: "my-llama", Provider: "mock", ProviderId: "",
			State: provisionerv1.InstanceState_INSTANCE_STATE_PENDING,
		}
		f.Deployments["my-llama"] = &provisionerv1.Deployment{
			Id: "my-llama", InstanceId: "my-llama",
			ContainerId: "mock-pod-orphan",
			State:       provisionerv1.DeploymentState_DEPLOYMENT_STATE_FAILED,
		}
		return nil
	})

	if _, err := svc.DestroyInstance(context.Background(), &provisionerv1.DestroyInstanceRequest{Id: "my-llama"}); err != nil {
		t.Fatalf("DestroyInstance: %v", err)
	}
	if mock.termCalls != 1 {
		t.Errorf("provider.Terminate calls = %d, want 1 (backfill should make destroy reach the provider)", mock.termCalls)
	}
}

func TestValidateID(t *testing.T) {
	cases := []struct {
		id    string
		valid bool
	}{
		{"my-pod", true},
		{"qwen-demo-1", true},
		{"a", true},
		{"", false},
		{"iplane-foo", false},
		{"-leading", false},
		{"trailing-", false},
		{"With_Underscore", false},
		{"UPPER", false},
	}
	for _, c := range cases {
		err := provisioners.ValidateID(c.id)
		if (err == nil) != c.valid {
			t.Errorf("ValidateID(%q) valid=%v want %v (err=%v)", c.id, err == nil, c.valid, err)
		}
	}
}

func TestCreateInstance_KeyRegistrar_CalledBeforeSpawn(t *testing.T) {
	reg := &mockKeyRegistrarProvider{mockProvider: &mockProvider{name: "mock"}}
	store, err := file.Open(t.TempDir(), "default")
	if err != nil {
		t.Fatalf("file.Open: %v", err)
	}
	svc := provisioners.New([]provisioners.Provider{reg}, store, "default",
		provisioners.WithKeyStore(newKeyStore(t)),
	)

	_, err = svc.CreateInstance(context.Background(), &provisionerv1.CreateInstanceRequest{Spec: okSpec()})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if reg.registrarCalls != 1 {
		t.Errorf("EnsurePublicKey called %d times, want 1", reg.registrarCalls)
	}
	if reg.spawnCalls != 1 {
		t.Errorf("Spawn called %d times, want 1", reg.spawnCalls)
	}
	// The comment string should be iplane-flavored so a downstream
	// skip-if-present check works.
	if !sshkeys.IsIplaneComment(reg.lastComment) {
		t.Errorf("registered comment %q is not an iplane comment", reg.lastComment)
	}
	if len(reg.lastPubKey) == 0 || string(reg.lastPubKey[:11]) != "ssh-ed25519" {
		t.Errorf("registered public key bytes do not look like ssh-ed25519 authorized_keys line: %q", reg.lastPubKey)
	}
}

func TestCreateInstance_KeyRegistrar_SkippedWhenInterfaceAbsent(t *testing.T) {
	mock := &mockProvider{name: "mock"} // does NOT implement KeyRegistrar
	store, err := file.Open(t.TempDir(), "default")
	if err != nil {
		t.Fatalf("file.Open: %v", err)
	}
	svc := provisioners.New([]provisioners.Provider{mock}, store, "default",
		provisioners.WithKeyStore(newKeyStore(t)),
	)
	if _, err := svc.CreateInstance(context.Background(), &provisionerv1.CreateInstanceRequest{Spec: okSpec()}); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if mock.spawnCalls != 1 {
		t.Errorf("Spawn called %d times, want 1 (no key registration should still proceed to Spawn)", mock.spawnCalls)
	}
}

func TestCreateInstance_KeyRegistrar_SkippedWhenStoreAbsent(t *testing.T) {
	reg := &mockKeyRegistrarProvider{mockProvider: &mockProvider{name: "mock"}}
	store, err := file.Open(t.TempDir(), "default")
	if err != nil {
		t.Fatalf("file.Open: %v", err)
	}
	svc := provisioners.New([]provisioners.Provider{reg}, store, "default") // no WithKeyStore

	if _, err := svc.CreateInstance(context.Background(), &provisionerv1.CreateInstanceRequest{Spec: okSpec()}); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if reg.registrarCalls != 0 {
		t.Errorf("EnsurePublicKey called %d times, want 0 when no key store is wired", reg.registrarCalls)
	}
	if reg.spawnCalls != 1 {
		t.Errorf("Spawn called %d times, want 1", reg.spawnCalls)
	}
}

func TestCreateInstance_KeyRegistrar_ErrorAbortsBeforeSpawn(t *testing.T) {
	reg := &mockKeyRegistrarProvider{
		mockProvider: &mockProvider{name: "mock"},
		registrarErr: errors.New("forbidden: scoped key cannot mutate user settings"),
	}
	store, err := file.Open(t.TempDir(), "default")
	if err != nil {
		t.Fatalf("file.Open: %v", err)
	}
	svc := provisioners.New([]provisioners.Provider{reg}, store, "default",
		provisioners.WithKeyStore(newKeyStore(t)),
	)
	_, err = svc.CreateInstance(context.Background(), &provisionerv1.CreateInstanceRequest{Spec: okSpec()})
	if err == nil {
		t.Fatal("expected CreateInstance to fail when EnsurePublicKey errors")
	}
	if reg.spawnCalls != 0 {
		t.Errorf("Spawn should NOT have been called (got %d calls) -- cost gate before spawn must hold", reg.spawnCalls)
	}
}

// ── DeploymentService tests ─────────────────────────────────────────

type fakeExecutor struct {
	deployCalls  int
	destroyCalls int
	// Optional hook: if set, called instead of the default
	// PENDING -> RUNNING / TERMINATED progression.
	deployFn  func(emit func(sshdocker.StateUpdate)) error
	destroyFn func(emit func(sshdocker.StateUpdate)) error
}

func (f *fakeExecutor) Deploy(ctx context.Context, dep *provisionerv1.Deployment, inst *provisionerv1.Instance, key *sshkeys.KeyPair, emit func(sshdocker.StateUpdate)) error {
	f.deployCalls++
	if f.deployFn != nil {
		return f.deployFn(emit)
	}
	emit(sshdocker.StateUpdate{State: provisionerv1.DeploymentState_DEPLOYMENT_STATE_STARTING})
	emit(sshdocker.StateUpdate{State: provisionerv1.DeploymentState_DEPLOYMENT_STATE_CONFIGURING})
	emit(sshdocker.StateUpdate{State: provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING, EngineEndpoint: "http://1.2.3.4:8000"})
	return nil
}

func (f *fakeExecutor) Destroy(ctx context.Context, dep *provisionerv1.Deployment, inst *provisionerv1.Instance, key *sshkeys.KeyPair, emit func(sshdocker.StateUpdate)) error {
	f.destroyCalls++
	if f.destroyFn != nil {
		return f.destroyFn(emit)
	}
	emit(sshdocker.StateUpdate{State: provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATING})
	emit(sshdocker.StateUpdate{State: provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATED})
	return nil
}

// newSvcWithDeploy builds a Service wired with: an InstanceRef-style
// runpod mock, a key store, a fake executor. Also seeds an ACTIVE
// instance "my-pod" so deployment tests have something to target.
func newSvcWithDeploy(t *testing.T) (*provisioners.Service, *file.Store, *fakeExecutor) {
	t.Helper()
	mock := &mockProvider{name: "runpod"}
	store, err := file.Open(t.TempDir(), "default")
	if err != nil {
		t.Fatalf("file.Open: %v", err)
	}
	exec := &fakeExecutor{}
	svc := provisioners.New([]provisioners.Provider{mock}, store, "default",
		provisioners.WithKeyStore(newKeyStore(t)),
		provisioners.WithDeploymentExecutor(exec),
	)
	// Seed an ACTIVE instance with SSH endpoint.
	if err := store.Update(func(f *provisioners.State) error {
		f.Instances["my-pod"] = &provisionerv1.Instance{
			Id:       "my-pod",
			Provider: "runpod",
			State:    provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE,
			Ssh:      &provisionerv1.SshTarget{Host: "1.2.3.4", Port: 22, User: "root"},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return svc, store, exec
}

func okDep() *provisionerv1.Deployment {
	return &provisionerv1.Deployment{
		Id:         "my-llama",
		InstanceId: "my-pod",
		Image:      "vllm/vllm-openai:0.7.0",
		Model:      "Qwen/Qwen2.5-7B-Instruct",
		EnginePort: 8000,
	}
}

// stubModelStore records every Resolve call and returns whatever the
// test programmed it to. Verifies the Service: (1) calls Resolve with
// the right spec, (2) propagates the returned env into dep.Env,
// (3) surfaces a Resolve error as InvalidArgument before any provider
// touch.
type stubModelStore struct {
	called   atomic.Int32
	lastSpec atomic.Value // string
	respond  func(spec string) (modelstores.Resolved, error)
}

func (s *stubModelStore) Resolve(_ context.Context, spec string) (modelstores.Resolved, error) {
	s.called.Add(1)
	s.lastSpec.Store(spec)
	if s.respond != nil {
		return s.respond(spec)
	}
	return modelstores.Resolved{EngineModelArg: spec}, nil
}

func TestCreateDeployment_ResolveModelCalled_AndEnvMerged(t *testing.T) {
	mock := &mockProvider{name: "runpod"}
	store, err := file.Open(t.TempDir(), "default")
	if err != nil {
		t.Fatalf("file.Open: %v", err)
	}
	ms := &stubModelStore{
		respond: func(spec string) (modelstores.Resolved, error) {
			return modelstores.Resolved{
				EngineModelArg: spec,
				EnvOverrides:   map[string]string{"HF_TOKEN": "secret"},
			}, nil
		},
	}
	exec := &fakeExecutor{}
	svc := provisioners.New([]provisioners.Provider{mock}, store, "default",
		provisioners.WithKeyStore(newKeyStore(t)),
		provisioners.WithDeploymentExecutor(exec),
		provisioners.WithModelStore(ms),
	)
	_ = store.Update(func(f *provisioners.State) error {
		f.Instances["my-pod"] = &provisionerv1.Instance{
			Id: "my-pod", Provider: "runpod",
			State: provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE,
			Ssh:   &provisionerv1.SshTarget{Host: "1.2.3.4", Port: 22, User: "root"},
		}
		return nil
	})

	resp, err := svc.CreateDeployment(context.Background(), &provisionerv1.CreateDeploymentRequest{
		Deployment: okDep(),
		Wait:       true,
	})
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	if ms.called.Load() != 1 {
		t.Errorf("Resolve called %d times, want 1", ms.called.Load())
	}
	if ms.lastSpec.Load() != "Qwen/Qwen2.5-7B-Instruct" {
		t.Errorf("Resolve received %q, want the spec from okDep()", ms.lastSpec.Load())
	}
	// Env from ModelStore must land on the deployment record.
	if resp.GetDeployment().GetEnv()["HF_TOKEN"] != "secret" {
		t.Errorf("HF_TOKEN should be merged from Resolved.EnvOverrides; got %+v", resp.GetDeployment().GetEnv())
	}
}

func TestCreateDeployment_ResolveError_FailsWithInvalidArgument(t *testing.T) {
	mock := &mockProvider{name: "runpod"}
	store, err := file.Open(t.TempDir(), "default")
	if err != nil {
		t.Fatalf("file.Open: %v", err)
	}
	ms := &stubModelStore{
		respond: func(_ string) (modelstores.Resolved, error) {
			return modelstores.Resolved{}, fmt.Errorf("not found on huggingface.co (typo?)")
		},
	}
	exec := &fakeExecutor{}
	svc := provisioners.New([]provisioners.Provider{mock}, store, "default",
		provisioners.WithKeyStore(newKeyStore(t)),
		provisioners.WithDeploymentExecutor(exec),
		provisioners.WithModelStore(ms),
	)
	_, err = svc.CreateDeployment(context.Background(), &provisionerv1.CreateDeploymentRequest{
		Deployment: okDep(),
		Wait:       true,
	})
	if err == nil {
		t.Fatal("expected InvalidArgument from Resolve")
	}
	if exec.deployCalls != 0 {
		t.Errorf("provider deploy must not run when Resolve fails; got %d calls", exec.deployCalls)
	}
}

// TestCreateDeployment_Replicas_RejectsGreaterThanOne: v0.2
// ch7-beat3.2 ships proto + helpers + CLI flag scaffolding. The
// parallel-provisioning impl is a focused follow-up; values > 1
// must reject explicitly so operators don't silently get a single
// instance when they asked for several.
func TestCreateDeployment_Replicas_RejectsGreaterThanOne(t *testing.T) {
	svc, _, _ := newSvcWithDeploy(t)
	_, err := svc.CreateDeployment(context.Background(), &provisionerv1.CreateDeploymentRequest{
		Deployment: okDep(),
		Wait:       true,
		Replicas:   3,
	})
	if err == nil {
		t.Fatalf("expected error for replicas=3 (Unimplemented), got nil")
	}
	if c := status.Code(err); c != codes.Unimplemented {
		t.Errorf("expected Unimplemented, got %s: %v", c, err)
	}
}

// TestCreateDeployment_Replicas_ZeroNormalizesToOne: proto3 zero
// value (replicas=0) on the wire normalizes to 1 -- the
// single-instance default that matches Beat 1+2 behavior.
func TestCreateDeployment_Replicas_ZeroNormalizesToOne(t *testing.T) {
	svc, _, _ := newSvcWithDeploy(t)
	resp, err := svc.CreateDeployment(context.Background(), &provisionerv1.CreateDeploymentRequest{
		Deployment: okDep(),
		Wait:       true,
		Replicas:   0,
	})
	if err != nil {
		t.Fatalf("replicas=0 should succeed (normalize to 1): %v", err)
	}
	if resp.GetDeployment().GetState() != provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING {
		t.Errorf("state = %v, want RUNNING", resp.GetDeployment().GetState())
	}
}

// TestCreateDeployment_EngineEndpoints_SnapshotPopulated: a
// successful single-instance deploy populates the parallel
// engine_endpoints list with the singular endpoint. The router
// (#85) reads this list via EffectiveEndpoints to fan out.
func TestCreateDeployment_EngineEndpoints_SnapshotPopulated(t *testing.T) {
	svc, store, _ := newSvcWithDeploy(t)
	if _, err := svc.CreateDeployment(context.Background(), &provisionerv1.CreateDeploymentRequest{
		Deployment: okDep(),
		Wait:       true,
	}); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	f, _ := store.Read()
	dep := f.Deployments["my-llama"]
	if dep.GetEngineEndpoint() == "" {
		t.Fatalf("singular engine_endpoint missing")
	}
	eps := dep.GetEngineEndpoints()
	if len(eps) != 1 {
		t.Fatalf("engine_endpoints len = %d, want 1 (single-instance snapshot)", len(eps))
	}
	if eps[0] != dep.GetEngineEndpoint() {
		t.Errorf("engine_endpoints[0] = %q, want = singular engine_endpoint %q",
			eps[0], dep.GetEngineEndpoint())
	}
}

// TestCreateDeployment_InstanceIds_DefaultsEmpty: a CreateDeployment
// request without instance_ids persists a record with empty list.
// v0.2 ch7-beat3.1: the multi-instance list is empty for single-
// instance deployments (Beat 1+2 shape); readers fall back to
// the singular `instance_id` via EffectiveInstanceIDs (helper
// added in #84). #83 ships the field as passive scaffolding.
func TestCreateDeployment_InstanceIds_DefaultsEmpty(t *testing.T) {
	svc, store, _ := newSvcWithDeploy(t)
	if _, err := svc.CreateDeployment(context.Background(), &provisionerv1.CreateDeploymentRequest{
		Deployment: okDep(),
		Wait:       true,
	}); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	f, _ := store.Read()
	ids := f.Deployments["my-llama"].GetInstanceIds()
	if len(ids) != 0 {
		t.Errorf("persisted instance_ids = %v, want empty (Beat 3 fan-out not yet wired)", ids)
	}
}

// TestCreateDeployment_InstanceIds_PreservesOperatorList: an
// operator-supplied instance_ids list on the request survives to
// the persisted record. The heterogeneous-fleet path: operators
// pre-allocate Instances via #84's add-instance verb, then bind
// them to a Deployment by passing the list at create time.
func TestCreateDeployment_InstanceIds_PreservesOperatorList(t *testing.T) {
	svc, store, _ := newSvcWithDeploy(t)
	dep := okDep()
	dep.InstanceIds = []string{"runpod-a", "vast-b", "lambda-c"}
	if _, err := svc.CreateDeployment(context.Background(), &provisionerv1.CreateDeploymentRequest{
		Deployment: dep,
		Wait:       true,
	}); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	f, _ := store.Read()
	got := f.Deployments["my-llama"].GetInstanceIds()
	want := []string{"runpod-a", "vast-b", "lambda-c"}
	if len(got) != len(want) {
		t.Fatalf("persisted instance_ids = %v, want %v", got, want)
	}
	for i, v := range want {
		if got[i] != v {
			t.Errorf("instance_ids[%d] = %q, want %q", i, got[i], v)
		}
	}
}

func TestCreateDeployment_HappyPath_WaitsForRUNNING(t *testing.T) {
	svc, store, exec := newSvcWithDeploy(t)
	resp, err := svc.CreateDeployment(context.Background(), &provisionerv1.CreateDeploymentRequest{
		Deployment: okDep(),
		Wait:       true,
	})
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	if resp.GetDeployment().GetState() != provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING {
		t.Errorf("final state = %v, want RUNNING", resp.GetDeployment().GetState())
	}
	if resp.GetDeployment().GetEngineEndpoint() == "" {
		t.Error("engine endpoint should be populated on RUNNING")
	}
	if exec.deployCalls != 1 {
		t.Errorf("Deploy called %d times, want 1", exec.deployCalls)
	}
	// Verify state file persisted the terminal state.
	f, _ := store.Read()
	if f.Deployments["my-llama"].GetState() != provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING {
		t.Error("state file should reflect RUNNING")
	}
}

func TestCreateDeployment_Idempotent_OnMatchingExisting(t *testing.T) {
	svc, _, exec := newSvcWithDeploy(t)
	if _, err := svc.CreateDeployment(context.Background(), &provisionerv1.CreateDeploymentRequest{Deployment: okDep(), Wait: true}); err != nil {
		t.Fatalf("first: %v", err)
	}
	resp, err := svc.CreateDeployment(context.Background(), &provisionerv1.CreateDeploymentRequest{Deployment: okDep(), Wait: true})
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if !resp.GetAlreadyExisted() {
		t.Error("second call should report AlreadyExisted=true")
	}
	if exec.deployCalls != 1 {
		t.Errorf("Deploy should only fire once for matching idempotent re-create; got %d", exec.deployCalls)
	}
}

func TestCreateDeployment_InstanceMissing_Rejects(t *testing.T) {
	svc, _, _ := newSvcWithDeploy(t)
	dep := okDep()
	dep.InstanceId = "no-such-pod"
	_, err := svc.CreateDeployment(context.Background(), &provisionerv1.CreateDeploymentRequest{Deployment: dep, Wait: true})
	if err == nil {
		t.Fatal("expected error when target instance does not exist")
	}
}

func TestCreateDeployment_InstanceNoSSH_Rejects(t *testing.T) {
	svc, store, _ := newSvcWithDeploy(t)
	// Strip SSH from the seeded instance.
	if err := store.Update(func(f *provisioners.State) error {
		f.Instances["my-pod"].Ssh = nil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_, err := svc.CreateDeployment(context.Background(), &provisionerv1.CreateDeploymentRequest{Deployment: okDep(), Wait: true})
	if err == nil {
		t.Fatal("expected error when instance has no SSH endpoint")
	}
}

func TestCreateDeployment_ExecutorError_PatchesFAILED(t *testing.T) {
	svc, store, exec := newSvcWithDeploy(t)
	exec.deployFn = func(emit func(sshdocker.StateUpdate)) error {
		emit(sshdocker.StateUpdate{State: provisionerv1.DeploymentState_DEPLOYMENT_STATE_STARTING})
		emit(sshdocker.StateUpdate{
			State:         provisionerv1.DeploymentState_DEPLOYMENT_STATE_FAILED,
			FailureReason: "engine /health never returned 2xx",
		})
		return errors.New("health timeout")
	}
	resp, err := svc.CreateDeployment(context.Background(), &provisionerv1.CreateDeploymentRequest{Deployment: okDep(), Wait: true})
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	if resp.GetDeployment().GetState() != provisionerv1.DeploymentState_DEPLOYMENT_STATE_FAILED {
		t.Errorf("state = %v, want FAILED", resp.GetDeployment().GetState())
	}
	if resp.GetDeployment().GetFailureReason() == "" {
		t.Error("failure reason should be set")
	}
	// State-file confirmation.
	f, _ := store.Read()
	if f.Deployments["my-llama"].GetState() != provisionerv1.DeploymentState_DEPLOYMENT_STATE_FAILED {
		t.Error("state file should reflect FAILED")
	}
}

func TestDescribeDeployment_HappyPath(t *testing.T) {
	svc, _, _ := newSvcWithDeploy(t)
	if _, err := svc.CreateDeployment(context.Background(), &provisionerv1.CreateDeploymentRequest{Deployment: okDep(), Wait: true}); err != nil {
		t.Fatalf("create: %v", err)
	}
	resp, err := svc.DescribeDeployment(context.Background(), &provisionerv1.DescribeDeploymentRequest{Id: "my-llama"})
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if resp.GetDeployment().GetId() != "my-llama" {
		t.Errorf("id = %q", resp.GetDeployment().GetId())
	}
}

func TestDescribeDeployment_NotFound(t *testing.T) {
	svc, _, _ := newSvcWithDeploy(t)
	_, err := svc.DescribeDeployment(context.Background(), &provisionerv1.DescribeDeploymentRequest{Id: "ghost"})
	if err == nil {
		t.Fatal("describe of missing id should error")
	}
}

func TestListDeployments_FiltersByInstance(t *testing.T) {
	svc, store, _ := newSvcWithDeploy(t)
	// Add a second instance + a deployment on it.
	if err := store.Update(func(f *provisioners.State) error {
		f.Instances["other-pod"] = &provisionerv1.Instance{
			Id:       "other-pod",
			Provider: "runpod",
			State:    provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE,
			Ssh:      &provisionerv1.SshTarget{Host: "5.6.7.8", Port: 22, User: "root"},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	dep1 := okDep()
	if _, err := svc.CreateDeployment(context.Background(), &provisionerv1.CreateDeploymentRequest{Deployment: dep1, Wait: true}); err != nil {
		t.Fatalf("dep1: %v", err)
	}
	dep2 := okDep()
	dep2.Id = "other-llama"
	dep2.InstanceId = "other-pod"
	if _, err := svc.CreateDeployment(context.Background(), &provisionerv1.CreateDeploymentRequest{Deployment: dep2, Wait: true}); err != nil {
		t.Fatalf("dep2: %v", err)
	}

	// No filter -> both.
	all, _ := svc.ListDeployments(context.Background(), &provisionerv1.ListDeploymentsRequest{})
	if len(all.GetDeployments()) != 2 {
		t.Errorf("no filter = %d, want 2", len(all.GetDeployments()))
	}
	// Filter to first instance -> 1.
	filtered, _ := svc.ListDeployments(context.Background(), &provisionerv1.ListDeploymentsRequest{InstanceId: "my-pod"})
	if len(filtered.GetDeployments()) != 1 || filtered.GetDeployments()[0].GetId() != "my-llama" {
		t.Errorf("instance filter = %+v", filtered.GetDeployments())
	}
}

func TestDestroyDeployment_HappyPath(t *testing.T) {
	svc, store, exec := newSvcWithDeploy(t)
	if _, err := svc.CreateDeployment(context.Background(), &provisionerv1.CreateDeploymentRequest{Deployment: okDep(), Wait: true}); err != nil {
		t.Fatalf("create: %v", err)
	}
	resp, err := svc.DestroyDeployment(context.Background(), &provisionerv1.DestroyDeploymentRequest{Id: "my-llama"})
	if err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if resp.GetDeployment().GetState() != provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATED {
		t.Errorf("final state = %v, want TERMINATED", resp.GetDeployment().GetState())
	}
	if exec.destroyCalls != 1 {
		t.Errorf("Destroy called %d times, want 1", exec.destroyCalls)
	}
	f, _ := store.Read()
	if f.Deployments["my-llama"].GetState() != provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATED {
		t.Error("state file should reflect TERMINATED")
	}
}

func TestDestroyDeployment_NotFound(t *testing.T) {
	svc, _, _ := newSvcWithDeploy(t)
	_, err := svc.DestroyDeployment(context.Background(), &provisionerv1.DestroyDeploymentRequest{Id: "ghost"})
	if err == nil {
		t.Fatal("destroy of missing id should error")
	}
}
