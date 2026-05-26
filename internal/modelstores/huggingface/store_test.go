package huggingface

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// hfFixture stands up an httptest server that mimics the HF model-info
// endpoint. Each test configures the response per id.
type hfFixture struct {
	t        *testing.T
	srv      *httptest.Server
	lastAuth string
	respond  func(id string) (status int, body string)
}

func newFixture(t *testing.T, respond func(id string) (int, string)) *hfFixture {
	t.Helper()
	f := &hfFixture{t: t, respond: respond}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.lastAuth = r.Header.Get("Authorization")
		// Path shape: /api/models/<org>/<name>
		const prefix = "/api/models/"
		if !strings.HasPrefix(r.URL.Path, prefix) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, prefix)
		status, body := f.respond(id)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *hfFixture) store(token string) *Store {
	return &Store{BaseURL: f.srv.URL, Token: token, HTTP: f.srv.Client()}
}

func TestResolve_HappyPath_PassesThroughSpec(t *testing.T) {
	f := newFixture(t, func(id string) (int, string) {
		if id != "Qwen/Qwen2.5-1.5B-Instruct" {
			t.Errorf("HF called with id=%q, want Qwen/Qwen2.5-1.5B-Instruct", id)
		}
		return 200, `{"id":"Qwen/Qwen2.5-1.5B-Instruct","gated":false,"disabled":false}`
	})
	res, err := f.store("").Resolve(context.Background(), "Qwen/Qwen2.5-1.5B-Instruct")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if res.EngineModelArg != "Qwen/Qwen2.5-1.5B-Instruct" {
		t.Errorf("EngineModelArg = %q, want pass-through of the spec", res.EngineModelArg)
	}
	if len(res.EnvOverrides) != 0 {
		t.Errorf("EnvOverrides should be empty when no HF_TOKEN; got %+v", res.EnvOverrides)
	}
}

func TestResolve_PropagatesHFToken(t *testing.T) {
	f := newFixture(t, func(_ string) (int, string) {
		return 200, `{"id":"x/y","gated":false}`
	})
	res, err := f.store("hf_test_token").Resolve(context.Background(), "x/y")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if res.EnvOverrides["HF_TOKEN"] != "hf_test_token" {
		t.Errorf("HF_TOKEN not propagated: %+v", res.EnvOverrides)
	}
	if f.lastAuth != "Bearer hf_test_token" {
		t.Errorf("Authorization header = %q, want Bearer hf_test_token", f.lastAuth)
	}
}

func TestResolve_PassesRevisionThrough(t *testing.T) {
	f := newFixture(t, func(id string) (int, string) {
		// HF API path uses the id WITHOUT the :revision suffix.
		if strings.Contains(id, ":") {
			t.Errorf("HF path got %q; should have stripped :revision before the call", id)
		}
		return 200, `{"id":"x/y","gated":false}`
	})
	res, err := f.store("").Resolve(context.Background(), "x/y:abc123")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if res.EngineModelArg != "x/y:abc123" {
		t.Errorf("revision should pass through to the engine; got %q", res.EngineModelArg)
	}
}

func TestResolve_404TypoMessage(t *testing.T) {
	f := newFixture(t, func(_ string) (int, string) { return 404, `{"error":"Repository not found"}` })
	_, err := f.store("").Resolve(context.Background(), "Qwen/Qwen2-1.5B-Instruct")
	if err == nil {
		t.Fatal("expected error for 404")
	}
	if !strings.Contains(err.Error(), "not found") || !strings.Contains(err.Error(), "typo") {
		t.Errorf("error should hint at typo; got: %v", err)
	}
}

func TestResolve_401Gated_TellsOperatorAboutHFToken(t *testing.T) {
	f := newFixture(t, func(_ string) (int, string) { return 401, `{"error":"Unauthorized"}` })
	_, err := f.store("").Resolve(context.Background(), "meta-llama/Meta-Llama-3-8B")
	if err == nil {
		t.Fatal("expected error for 401")
	}
	if !strings.Contains(err.Error(), "gated") || !strings.Contains(err.Error(), "HF_TOKEN") {
		t.Errorf("error should mention 'gated' AND 'HF_TOKEN'; got: %v", err)
	}
}

func TestResolve_403TokenLacksPerms(t *testing.T) {
	f := newFixture(t, func(_ string) (int, string) { return 403, `{"error":"You have not accepted the license"}` })
	_, err := f.store("hf_token").Resolve(context.Background(), "meta-llama/Meta-Llama-3-8B")
	if err == nil {
		t.Fatal("expected error for 403")
	}
	if !strings.Contains(err.Error(), "lacks access") {
		t.Errorf("error should mention 'lacks access' / license; got: %v", err)
	}
}

func TestResolve_DisabledFlag(t *testing.T) {
	f := newFixture(t, func(_ string) (int, string) {
		return 200, `{"id":"x/y","disabled":true}`
	})
	_, err := f.store("").Resolve(context.Background(), "x/y")
	if err == nil {
		t.Fatal("expected error for disabled model")
	}
	if !strings.Contains(err.Error(), "disabled") {
		t.Errorf("error should mention 'disabled'; got: %v", err)
	}
}

func TestResolve_NetworkError_BypassHint(t *testing.T) {
	// Server that immediately closes the connection -> network error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			w.WriteHeader(500)
			return
		}
		conn, _, _ := hj.Hijack()
		_ = conn.Close()
	}))
	t.Cleanup(srv.Close)
	s := &Store{BaseURL: srv.URL, HTTP: srv.Client()}
	_, err := s.Resolve(context.Background(), "x/y")
	if err == nil {
		t.Fatal("expected network error")
	}
	if !strings.Contains(err.Error(), "skip-model-validation") {
		t.Errorf("error should mention the bypass flag; got: %v", err)
	}
}

func TestResolve_BadSpecShape_NoNetworkCall(t *testing.T) {
	// Server should never be called; any hit fails the test.
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("HF should not be called for malformed specs")
	}))
	t.Cleanup(srv.Close)
	s := &Store{BaseURL: srv.URL, HTTP: srv.Client()}

	for _, spec := range []string{"", "no-slash", "/leading-slash", "trailing-slash/", "/", "a/b/c"} {
		_, err := s.Resolve(context.Background(), spec)
		if err == nil {
			t.Errorf("expected error for spec %q", spec)
		}
	}
}
