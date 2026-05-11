package local

import (
	"context"
	"errors"
	"testing"

	"github.com/inference-book/inference-plane/internal/provisioners"
	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

func newSpec() *provisionerv1.Spec {
	return &provisionerv1.Spec{
		Id:       "my-pod",
		Provider: provisioners.ProviderLocal,
		Region:   "laptop",
		Gpu:      &provisionerv1.GpuSpec{Class: provisioners.GPUClassSmall, Count: 1},
	}
}

func TestProvider_Name(t *testing.T) {
	p := New()
	if got := p.Name(); got != provisioners.ProviderLocal {
		t.Errorf("Name() = %q, want %q", got, provisioners.ProviderLocal)
	}
}

func TestProvider_Spawn_ReturnsActiveInstance(t *testing.T) {
	p := New()
	inst, err := p.Spawn(context.Background(), newSpec())
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if inst.GetId() != "my-pod" {
		t.Errorf("Id = %q, want my-pod", inst.GetId())
	}
	if inst.GetProvider() != provisioners.ProviderLocal {
		t.Errorf("Provider = %q, want %q", inst.GetProvider(), provisioners.ProviderLocal)
	}
	if inst.GetProviderId() != "local:my-pod" {
		t.Errorf("ProviderId = %q, want local:my-pod", inst.GetProviderId())
	}
	if inst.GetState() != provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE {
		t.Errorf("State = %v, want ACTIVE", inst.GetState())
	}
	if inst.GetHourlyRateUsd() != 0 {
		t.Errorf("HourlyRateUsd = %v, want 0 (local is free)", inst.GetHourlyRateUsd())
	}
	if inst.GetSsh().GetHost() != "" {
		t.Errorf("Ssh.Host = %q, want empty (no remote shell on laptop)", inst.GetSsh().GetHost())
	}
	if inst.GetCreatedAt() == nil {
		t.Error("CreatedAt should be set")
	}
	if inst.GetActivatedAt() == nil {
		t.Error("ActivatedAt should be set (state is active immediately)")
	}
}

func TestProvider_Spawn_NilSpec(t *testing.T) {
	p := New()
	_, err := p.Spawn(context.Background(), nil)
	if err == nil {
		t.Fatal("Spawn(nil) should error")
	}
	var pe *provisioners.ProviderError
	if !errors.As(err, &pe) {
		t.Errorf("expected *ProviderError, got %T", err)
	}
}

func TestProvider_Spawn_CancelledContext(t *testing.T) {
	p := New()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := p.Spawn(ctx, newSpec())
	if err == nil {
		t.Fatal("Spawn should respect cancelled context")
	}
}

func TestProvider_Terminate_IsNoop(t *testing.T) {
	p := New()
	if err := p.Terminate(context.Background(), "local:my-pod"); err != nil {
		t.Errorf("Terminate: %v", err)
	}
	// Idempotent: second call also returns nil.
	if err := p.Terminate(context.Background(), "local:my-pod"); err != nil {
		t.Errorf("Terminate (second call): %v", err)
	}
}

func TestProvider_Describe_AlwaysNotFound(t *testing.T) {
	p := New()
	_, err := p.Describe(context.Background(), "local:my-pod")
	if err == nil {
		t.Fatal("Describe should return ErrNotFound for local (no provider-side state)")
	}
	if !errors.Is(err, provisioners.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestProvider_List_AlwaysEmpty(t *testing.T) {
	p := New()
	refs, err := p.List(context.Background(), map[string]string{provisioners.TagOperator: "default"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("List should return empty (no provider-side state), got %d", len(refs))
	}
}

func TestClassifyByVRAM(t *testing.T) {
	cases := []struct {
		vramGB int
		want   string
	}{
		{0, provisioners.GPUClassSmall},
		{24, provisioners.GPUClassSmall},
		{40, provisioners.GPUClassMedium},
		{48, provisioners.GPUClassMedium},
		{80, provisioners.GPUClassLarge},
		{96, provisioners.GPUClassXLarge},
		{192, provisioners.GPUClassXLarge},
	}
	for _, c := range cases {
		if got := classifyByVRAM(c.vramGB); got != c.want {
			t.Errorf("classifyByVRAM(%d) = %q, want %q", c.vramGB, got, c.want)
		}
	}
}

// Compile-time check the test fixtures still satisfy the interface.
var _ provisioners.Provider = (*Provider)(nil)
