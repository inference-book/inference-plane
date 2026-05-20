package provisioners_test

import (
	"context"
	"errors"
	"testing"
	"time"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/state"
	"google.golang.org/protobuf/types/known/timestamppb"
)

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

func newSvc(t *testing.T, provs ...provisioners.Provider) (*provisioners.Service, *state.Store) {
	t.Helper()
	store, err := state.Open(t.TempDir(), "default")
	if err != nil {
		t.Fatalf("state.Open: %v", err)
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
	if err := store.Update(func(f *state.File) error {
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
