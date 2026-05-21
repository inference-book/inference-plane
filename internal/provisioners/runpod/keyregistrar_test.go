package runpod

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeGraphQLServer captures the requests + lets each test wire its
// own pubKey value and HTTP status. The original keyregistrar
// hard-codes the URL to api.runpod.io/graphql; we redirect by
// constructing the Provider with a custom http.Client whose Transport
// rewrites Host to point at the httptest server.
//
// (We do not change the const graphqlEndpoint to a var; production
// behavior should not be a moving target. The Transport rewrite is
// test-only.)
type fakeGraphQLServer struct {
	server         *httptest.Server
	currentPubKey  string
	status         int
	updates        []string // captures the pubKey value each updateUserSettings sets
	requestHeaders []http.Header
}

func newFakeGraphQL(t *testing.T) *fakeGraphQLServer {
	t.Helper()
	f := &fakeGraphQLServer{status: http.StatusOK}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requestHeaders = append(f.requestHeaders, r.Header.Clone())
		if f.status != http.StatusOK {
			w.WriteHeader(f.status)
			_, _ = w.Write([]byte(`{"error":"forbidden"}`))
			return
		}
		body, _ := io.ReadAll(r.Body)
		var env struct {
			Query string `json:"query"`
		}
		_ = json.Unmarshal(body, &env)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(env.Query, "myself"):
			resp := map[string]any{"data": map[string]any{"myself": map[string]any{"pubKey": f.currentPubKey}}}
			_ = json.NewEncoder(w).Encode(resp)
		case strings.Contains(env.Query, "updateUserSettings"):
			// Extract pubKey arg via a forgiving substring match -- the
			// production code formats with fmt.Sprintf `pubKey: %q `,
			// producing `pubKey: "<escaped>" }) { id }`.
			i := strings.Index(env.Query, `pubKey: "`)
			start := i + len(`pubKey: "`)
			end := strings.Index(env.Query[start:], `" })`)
			if i >= 0 && end >= 0 {
				raw := env.Query[start : start+end]
				// Unescape what production %q produces.
				raw = strings.ReplaceAll(raw, `\n`, "\n")
				raw = strings.ReplaceAll(raw, `\"`, `"`)
				f.updates = append(f.updates, raw)
				f.currentPubKey = raw
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"updateUserSettings": map[string]any{"id": "user_1"}}})
		default:
			http.Error(w, "unexpected query", http.StatusBadRequest)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

// providerForTest builds a runpod.Provider whose http.Client rewrites
// any request bound for api.runpod.io to the httptest server.
func providerForTest(t *testing.T, f *fakeGraphQLServer) *Provider {
	t.Helper()
	rt := &rewriteTransport{toHost: strings.TrimPrefix(f.server.URL, "http://")}
	httpClient := &http.Client{Transport: rt}
	return New(NewClient("test-api-key", WithHTTPClient(httpClient)))
}

// rewriteTransport rewrites api.runpod.io -> the httptest host so
// production keyregistrar.go's hardcoded URL still works in tests.
type rewriteTransport struct {
	toHost string
}

func (t *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = "http"
	req.URL.Host = t.toHost
	req.Host = t.toHost
	return http.DefaultTransport.RoundTrip(req)
}

const iplaneKey1 = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIN1qOJSfqQS1J0CKvqg7n0X2Kvz5kZbAVfAHvKvYrPLB iplane-default-runpod-2026-05-20T15:30:00Z\n"
const iplaneKey2 = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIMR1qOJSfqQS1J0CKvqg7n0X2Kvz5kZbAVfAHvKvYrPLB iplane-default-runpod-2026-05-20T15:30:00Z\n"
const userKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBBQQbcAVcAVcAVcAVcAVcAVcAVcAVcAVcAVcAVcAVcA user@laptop\n"

func TestEnsurePublicKey_UploadsWhenAbsent(t *testing.T) {
	f := newFakeGraphQL(t)
	f.currentPubKey = "" // empty account
	p := providerForTest(t, f)

	if err := p.EnsurePublicKey(context.Background(), []byte(iplaneKey1), "iplane-default-runpod-2026-05-20T15:30:00Z"); err != nil {
		t.Fatalf("EnsurePublicKey: %v", err)
	}
	if len(f.updates) != 1 {
		t.Fatalf("updateUserSettings called %d times, want 1", len(f.updates))
	}
	if !strings.Contains(f.updates[0], "iplane-default-runpod") {
		t.Errorf("uploaded blob missing the iplane key: %q", f.updates[0])
	}
}

func TestEnsurePublicKey_SkipsWhenAlreadyPresent(t *testing.T) {
	f := newFakeGraphQL(t)
	f.currentPubKey = iplaneKey1 // same iplane key already there
	p := providerForTest(t, f)

	if err := p.EnsurePublicKey(context.Background(), []byte(iplaneKey1), "iplane-default-runpod-2026-05-20T15:30:00Z"); err != nil {
		t.Fatalf("EnsurePublicKey: %v", err)
	}
	if len(f.updates) != 0 {
		t.Errorf("updateUserSettings called %d times, want 0 (key already present)", len(f.updates))
	}
}

func TestEnsurePublicKey_PreservesOtherKeys(t *testing.T) {
	f := newFakeGraphQL(t)
	f.currentPubKey = userKey // user's own pre-existing key
	p := providerForTest(t, f)

	if err := p.EnsurePublicKey(context.Background(), []byte(iplaneKey1), "iplane-default-runpod-2026-05-20T15:30:00Z"); err != nil {
		t.Fatalf("EnsurePublicKey: %v", err)
	}
	if len(f.updates) != 1 {
		t.Fatalf("updateUserSettings called %d times, want 1", len(f.updates))
	}
	blob := f.updates[0]
	if !strings.Contains(blob, "user@laptop") {
		t.Errorf("uploaded blob dropped the user's existing key: %q", blob)
	}
	if !strings.Contains(blob, "iplane-default-runpod") {
		t.Errorf("uploaded blob missing the iplane key: %q", blob)
	}
}

func TestEnsurePublicKey_ReplacesStaleIplaneKey(t *testing.T) {
	// Old iplane key with the same comment-prefix but DIFFERENT bytes
	// (e.g. operator's key got regenerated). EnsurePublicKey should
	// append the new one; cleanup of the old is a separate concern
	// (rotation), tracked for v0.2.
	f := newFakeGraphQL(t)
	f.currentPubKey = iplaneKey2 + userKey
	p := providerForTest(t, f)

	if err := p.EnsurePublicKey(context.Background(), []byte(iplaneKey1), "iplane-default-runpod-2026-05-20T15:30:00Z"); err != nil {
		t.Fatalf("EnsurePublicKey: %v", err)
	}
	if len(f.updates) != 1 {
		t.Fatalf("updateUserSettings called %d times, want 1 (different bytes, should still upload)", len(f.updates))
	}
}

func TestEnsurePublicKey_403SurfacesScopedKeyMessage(t *testing.T) {
	f := newFakeGraphQL(t)
	f.status = http.StatusForbidden
	p := providerForTest(t, f)

	err := p.EnsurePublicKey(context.Background(), []byte(iplaneKey1), "iplane-default-runpod-2026-05-20T15:30:00Z")
	if err == nil {
		t.Fatal("expected error on 403")
	}
	if !strings.Contains(err.Error(), "Full scope") {
		t.Errorf("error message %q should mention the Full-scope requirement", err)
	}
}
