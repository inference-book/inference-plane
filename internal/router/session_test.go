package router

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

func msg(role, content string) flatMessage {
	return flatMessage{Role: role, Content: json.RawMessage(strconv.Quote(content))}
}

// TestDeriveSessionKey_StableAcrossTurns: the opening (system + first
// user) hashes to the same key as the conversation grows, so every turn
// of one conversation shares a key.
func TestDeriveSessionKey_StableAcrossTurns(t *testing.T) {
	turn1 := []flatMessage{msg("system", "sys"), msg("user", "hi")}
	turn3 := []flatMessage{msg("system", "sys"), msg("user", "hi"), msg("assistant", "yo"), msg("user", "more")}
	k1, k3 := deriveSessionKey(turn1), deriveSessionKey(turn3)
	if k1 == "" {
		t.Fatal("key empty for a valid opening")
	}
	if k1 != k3 {
		t.Errorf("opening key changed across turns: %q vs %q", k1, k3)
	}
}

// TestDeriveSessionKey_DistinctAndEmpty: different first-user turns get
// different keys; no user message gets no key.
func TestDeriveSessionKey_DistinctAndEmpty(t *testing.T) {
	a := deriveSessionKey([]flatMessage{msg("system", "sys"), msg("user", "hello A")})
	b := deriveSessionKey([]flatMessage{msg("system", "sys"), msg("user", "hello B")})
	if a == b {
		t.Errorf("distinct first-user turns hashed the same: %q", a)
	}
	if got := deriveSessionKey(nil); got != "" {
		t.Errorf("no messages: got %q, want empty", got)
	}
	if got := deriveSessionKey([]flatMessage{msg("system", "only sys")}); got != "" {
		t.Errorf("no user message: got %q, want empty", got)
	}
}

// TestSessionKey_Precedence: explicit header > body-derived > none.
func TestSessionKey_Precedence(t *testing.T) {
	withHeader, _ := http.NewRequest(http.MethodPost, "/", nil)
	withHeader.Header.Set(SessionHeader, "explicit")
	*withHeader = *withHeader.WithContext(withDerivedSession(withHeader.Context(), "derived"))
	if got := sessionKey(withHeader); got != "explicit" {
		t.Errorf("header present: got %q, want explicit", got)
	}

	derivedOnly, _ := http.NewRequest(http.MethodPost, "/", nil)
	*derivedOnly = *derivedOnly.WithContext(withDerivedSession(derivedOnly.Context(), "derived"))
	if got := sessionKey(derivedOnly); got != "derived" {
		t.Errorf("no header: got %q, want derived", got)
	}

	neither, _ := http.NewRequest(http.MethodPost, "/", nil)
	if got := sessionKey(neither); got != "" {
		t.Errorf("neither: got %q, want empty", got)
	}
}

// TestRouter_Flat_DerivedKeyDrivesAffinity: two header-less chat requests
// with the same opening, through the flat URL, record an affinity miss
// then hit -- proving the body-derived key threads into the affinity
// path exactly like an explicit header would.
func TestRouter_Flat_DerivedKeyDrivesAffinity(t *testing.T) {
	reader, rec := setupMetricsCapture(t)
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"ok"}`)
	}))
	defer engine.Close()

	r := New(&flatFakeClient{
		fakeDeploymentClient: &fakeDeploymentClient{},
		list: func(_ *provisionerv1.ListDeploymentsRequest) (*provisionerv1.ListDeploymentsResponse, error) {
			return &provisionerv1.ListDeploymentsResponse{Deployments: []*provisionerv1.Deployment{{
				Id: "d", Model: "m", State: provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING, EngineEndpoint: engine.URL,
			}}}, nil
		},
	}, rec)
	srv := httptest.NewServer(serveThroughMux(r))
	defer srv.Close()

	const body = `{"model":"m","messages":[{"role":"system","content":"sys"},{"role":"user","content":"hi"}]}`
	for range 2 {
		resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		resp.Body.Close()
	}

	if got := affinityOutcomes(t, reader); got["hit"] != 1 || got["miss"] != 1 {
		t.Errorf("outcomes = %v, want hit=1 miss=1 (derived key drove affinity)", got)
	}
}
