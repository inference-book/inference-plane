package router

import (
	"net/http"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// The credential is read from the daemon's own environment at forward time.
// Only the variable's name is ever persisted, because the deployment record
// goes to the state file and to every DescribeDeployment response.
func TestUpstreamAuthResolvesFromTheEnvironment(t *testing.T) {
	t.Setenv("TEST_UPSTREAM_KEY", "sk-secret")
	h := http.Header{}

	applyUpstreamAuth(h, &provisionerv1.UpstreamAuth{
		Header: "Authorization", ValueEnv: "TEST_UPSTREAM_KEY", ValuePrefix: "Bearer ",
	})

	if got := h.Get("Authorization"); got != "Bearer sk-secret" {
		t.Errorf("Authorization = %q, want the prefixed credential", got)
	}
}

// Set, not Add, and applied after the inbound headers are copied. Otherwise a
// caller could smuggle their own Authorization through to a paid upstream by
// sending one to us.
func TestUpstreamAuthOverwritesACallerSuppliedHeader(t *testing.T) {
	t.Setenv("TEST_UPSTREAM_KEY", "ours")
	h := http.Header{}
	h.Set("Authorization", "Bearer theirs")

	applyUpstreamAuth(h, &provisionerv1.UpstreamAuth{
		Header: "Authorization", ValueEnv: "TEST_UPSTREAM_KEY", ValuePrefix: "Bearer ",
	})

	if got := h.Values("Authorization"); len(got) != 1 || got[0] != "Bearer ours" {
		t.Errorf("Authorization = %v, want exactly our credential", got)
	}
}

// Everything iplane provisions itself is reachable because we rented it. A
// deployment with no credential must get no header at all, not an empty one.
func TestUpstreamAuthAbsentForOwnedEngines(t *testing.T) {
	h := http.Header{}

	applyUpstreamAuth(h, nil)

	if _, present := h["Authorization"]; present {
		t.Errorf("set a header for a deployment that named no credential: %v", h)
	}
}

// A credential rotated out from under a running router should surface as a 401
// from the upstream, which is accurate and debuggable, rather than as a
// synthetic error from us. The mistake is caught at deploy time instead.
func TestUpstreamAuthSkipsAMissingVariableRatherThanSendingEmpty(t *testing.T) {
	h := http.Header{}

	applyUpstreamAuth(h, &provisionerv1.UpstreamAuth{
		Header: "Authorization", ValueEnv: "TEST_UPSTREAM_DEFINITELY_UNSET", ValuePrefix: "Bearer ",
	})

	if _, present := h["Authorization"]; present {
		t.Errorf("sent a header built from an unset variable: %v", h)
	}
}

// Some gateways want the bare token under their own header name.
func TestUpstreamAuthSupportsANonBearerHeader(t *testing.T) {
	t.Setenv("TEST_UPSTREAM_KEY", "raw-token")
	h := http.Header{}

	applyUpstreamAuth(h, &provisionerv1.UpstreamAuth{
		Header: "X-Api-Key", ValueEnv: "TEST_UPSTREAM_KEY",
	})

	if got := h.Get("X-Api-Key"); got != "raw-token" {
		t.Errorf("X-Api-Key = %q, want the bare token with no prefix", got)
	}
	if h.Get("Authorization") != "" {
		t.Error("also set Authorization, which the operator did not ask for")
	}
}
