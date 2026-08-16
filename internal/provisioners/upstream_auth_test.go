package provisioners_test

import (
	"strings"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
)

// A deployment registered against a credential that is not set looks perfectly
// healthy and 401s every request, so the operator finds out from production
// traffic. Failing the create is the cheaper place to learn it.
func TestValidateUpstreamAuthRejectsAnUnsetVariable(t *testing.T) {
	_, err := provisioners.ValidateUpstreamAuth(&provisionerv1.UpstreamAuth{
		ValueEnv: "TEST_DEFINITELY_UNSET_KEY",
	})

	if err == nil {
		t.Fatal("accepted a credential naming a variable that is not set")
	}
	if !strings.Contains(err.Error(), "TEST_DEFINITELY_UNSET_KEY") {
		t.Errorf("error does not name the variable to export: %v", err)
	}
}

// Naming only the variable should produce a working bearer credential, since
// that is what almost every hosted OpenAI-compatible API wants.
func TestValidateUpstreamAuthFillsTheBearerDefaults(t *testing.T) {
	t.Setenv("TEST_KEY", "sk-x")

	got, err := provisioners.ValidateUpstreamAuth(&provisionerv1.UpstreamAuth{ValueEnv: "TEST_KEY"})
	if err != nil {
		t.Fatalf("ValidateUpstreamAuth: %v", err)
	}

	if got.GetHeader() != "Authorization" || got.GetValuePrefix() != "Bearer " {
		t.Errorf("defaults = %q / %q, want Authorization / \"Bearer \"", got.GetHeader(), got.GetValuePrefix())
	}
}

// A gateway wanting the bare token under its own header is a real case, so a
// custom header must not acquire a bearer prefix it never asked for.
func TestValidateUpstreamAuthLeavesACustomHeaderUnprefixed(t *testing.T) {
	t.Setenv("TEST_KEY", "sk-x")

	got, err := provisioners.ValidateUpstreamAuth(&provisionerv1.UpstreamAuth{
		Header: "X-Api-Key", ValueEnv: "TEST_KEY",
	})
	if err != nil {
		t.Fatalf("ValidateUpstreamAuth: %v", err)
	}
	if got.GetValuePrefix() != "" {
		t.Errorf("prefix = %q, want empty for a non-Authorization header", got.GetValuePrefix())
	}
}

// The credential itself must never enter the record, which is persisted to the
// state file and returned by DescribeDeployment.
func TestValidateUpstreamAuthStoresTheNameNotTheSecret(t *testing.T) {
	t.Setenv("TEST_KEY", "sk-super-secret")

	got, err := provisioners.ValidateUpstreamAuth(&provisionerv1.UpstreamAuth{ValueEnv: "TEST_KEY"})
	if err != nil {
		t.Fatalf("ValidateUpstreamAuth: %v", err)
	}

	if strings.Contains(got.String(), "sk-super-secret") {
		t.Errorf("the credential leaked into the persisted record: %s", got.String())
	}
}

// Nothing iplane provisions itself carries a credential, and asking for one
// with no variable named is a mistake rather than a no-op.
func TestValidateUpstreamAuthNilIsFineButEmptyIsNot(t *testing.T) {
	if got, err := provisioners.ValidateUpstreamAuth(nil); err != nil || got != nil {
		t.Errorf("nil auth = %v, %v; want nil, nil", got, err)
	}
	if _, err := provisioners.ValidateUpstreamAuth(&provisionerv1.UpstreamAuth{Header: "Authorization"}); err == nil {
		t.Error("accepted an auth block that names no variable")
	}
}
