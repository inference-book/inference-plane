package provisioners

import (
	"context"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// plainProvider implements Provider and nothing else: the ordinary case of a
// provider whose API cannot say whether an instance is dead.
type plainProvider struct{}

func (plainProvider) Name() string { return "plain" }
func (plainProvider) Spawn(context.Context, *provisionerv1.Spec) (*provisionerv1.Instance, error) {
	return nil, nil
}
func (plainProvider) Terminate(context.Context, string) error { return nil }
func (plainProvider) Describe(context.Context, string) (*provisionerv1.Instance, error) {
	return nil, nil
}
func (plainProvider) List(context.Context, map[string]string) ([]*provisionerv1.InstanceRef, error) {
	return nil, nil
}

type reportingProvider struct {
	plainProvider
	failed bool
	reason string
	gotID  string
}

func (r *reportingProvider) TerminalFailure(_ context.Context, providerID string) (bool, string) {
	r.gotID = providerID
	return r.failed, r.reason
}

// The default has to be "keep waiting". A provider that cannot answer must not
// have its silence read as a fault, or every deploy on RunPod, Lambda and
// local would abort the moment this shipped.
func TestTerminalFailureDefaultsToKeepWaiting(t *testing.T) {
	failed, reason := TerminalFailure(context.Background(), plainProvider{}, "abc")
	if failed {
		t.Error("a provider without the capability reported a failure")
	}
	if reason != "" {
		t.Errorf("reason = %q, want empty", reason)
	}
}

func TestTerminalFailureConsultsTheCapability(t *testing.T) {
	p := &reportingProvider{failed: true, reason: "OCI runtime create failed"}

	failed, reason := TerminalFailure(context.Background(), p, "pod-7")

	if !failed {
		t.Fatal("capability reported a failure and the guard swallowed it")
	}
	if reason != "OCI runtime create failed" {
		t.Errorf("reason = %q; the provider's own words must reach the operator", reason)
	}
	if p.gotID != "pod-7" {
		t.Errorf("provider id = %q, want pod-7", p.gotID)
	}
}

// An unprovisioned record has no id to ask about. Asking anyway would make
// every adapter handle an empty id, which is the sort of thing one of them
// eventually gets wrong.
func TestTerminalFailureSkipsAnEmptyProviderID(t *testing.T) {
	p := &reportingProvider{failed: true, reason: "should not be consulted"}

	if failed, _ := TerminalFailure(context.Background(), p, ""); failed {
		t.Error("consulted the provider with no instance id")
	}
	if p.gotID != "" {
		t.Errorf("provider was called with %q despite there being no instance", p.gotID)
	}
}

// A provider that implements the capability and answers "still working" must
// be indistinguishable from one that cannot answer, so a healthy slow deploy
// is treated the same either way.
func TestTerminalFailureNegativeAnswerKeepsWaiting(t *testing.T) {
	p := &reportingProvider{failed: false}
	if failed, reason := TerminalFailure(context.Background(), p, "pod-7"); failed || reason != "" {
		t.Errorf("= (%v, %q), want (false, \"\")", failed, reason)
	}
}
