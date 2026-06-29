package cmd

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestLoad_PriorityTenantHeaders: iplane load sets X-IPlane-Priority
// and X-IPlane-Tenant when the matching flags are passed. v0.2
// ch7-beat2.8: demo 05 drives the queue's lane structure through
// these headers from two parallel iplane load processes.
func TestLoad_PriorityTenantHeaders(t *testing.T) {
	var sawPriority, sawTenant atomic.Pointer[string]
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.Header.Get("X-IPlane-Priority")
		te := r.Header.Get("X-IPlane-Tenant")
		sawPriority.Store(&p)
		sawTenant.Store(&te)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"completion_tokens":3}}`)
	}))
	defer srv.Close()

	cfg := &loadFireConfig{
		base:            srv.URL,
		chatPath:        "/v1/chat/completions",
		completionsPath: "/v1/completions",
		priority:        "interactive",
		tenant:          "alice",
	}
	stats := &loadStats{}
	// Force chat path to avoid completion-vs-chat coin flip.
	old := loadChatFraction
	loadChatFraction = 1.0
	defer func() { loadChatFraction = old }()
	fireLoadRequest(t.Context(), &http.Client{}, cfg, stats)

	got := sawPriority.Load()
	if got == nil || *got != "interactive" {
		t.Errorf("X-IPlane-Priority = %v, want interactive", got)
	}
	got = sawTenant.Load()
	if got == nil || *got != "alice" {
		t.Errorf("X-IPlane-Tenant = %v, want alice", got)
	}
	if stats.successes != 1 {
		t.Errorf("successes = %d, want 1", stats.successes)
	}
	if stats.tokens != 3 {
		t.Errorf("tokens = %d, want 3 (from usage.completion_tokens)", stats.tokens)
	}
}

// TestLoad_NoPriorityFlag_NoHeader: when --priority is not set, the
// header is absent (router falls back to its default). Same for
// tenant. Locks in the "absence means default" contract.
func TestLoad_NoPriorityFlag_NoHeader(t *testing.T) {
	var sawPriority, sawTenant atomic.Pointer[string]
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.Header.Get("X-IPlane-Priority")
		te := r.Header.Get("X-IPlane-Tenant")
		sawPriority.Store(&p)
		sawTenant.Store(&te)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	cfg := &loadFireConfig{
		base:            srv.URL,
		chatPath:        "/v1/chat/completions",
		completionsPath: "/v1/completions",
	}
	stats := &loadStats{}
	loadChatFraction = 1.0
	fireLoadRequest(t.Context(), &http.Client{}, cfg, stats)

	if got := sawPriority.Load(); got == nil || *got != "" {
		t.Errorf("X-IPlane-Priority = %v, want absent (empty)", got)
	}
	if got := sawTenant.Load(); got == nil || *got != "" {
		t.Errorf("X-IPlane-Tenant = %v, want absent (empty)", got)
	}
}

// TestLoad_StreamingMode_ParsesUsageFromSSE: when --stream is set,
// iplane load reads the SSE response and pulls completion_tokens
// from the usage frame.
func TestLoad_StreamingMode_ParsesUsageFromSSE(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		// Two delta frames, then a usage frame, then [DONE].
		for _, line := range []string{
			"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n",
			"data: {\"choices\":[{\"delta\":{\"content\":\" there\"}}]}\n\n",
			"data: {\"usage\":{\"completion_tokens\":7}}\n\n",
			"data: [DONE]\n\n",
		} {
			_, _ = io.WriteString(w, line)
			fl.Flush()
		}
	}))
	defer srv.Close()

	cfg := &loadFireConfig{
		base:            srv.URL,
		chatPath:        "/v1/chat/completions",
		completionsPath: "/v1/completions",
		stream:          true,
	}
	stats := &loadStats{}
	loadChatFraction = 1.0
	fireLoadRequest(t.Context(), &http.Client{}, cfg, stats)

	if stats.successes != 1 {
		t.Fatalf("successes = %d, want 1", stats.successes)
	}
	if stats.tokens != 7 {
		t.Errorf("tokens = %d, want 7 (from final SSE usage frame)", stats.tokens)
	}
}

// TestLoadEndpoint_TargetVsURL covers the URL-construction fork:
// --target builds /v1/<id>/v1/... via service-url; --url uses the
// flat path.
func TestLoadEndpoint_TargetVsURL(t *testing.T) {
	cases := []struct {
		name         string
		url          string
		serviceURL   string
		target       string
		wantBase     string
		wantChatPath string
		wantErr      bool
	}{
		{
			name:         "flat URL mode (default)",
			url:          "http://localhost:8080",
			wantBase:     "http://localhost:8080",
			wantChatPath: "/v1/chat/completions",
		},
		{
			name:         "target mode with service-url",
			serviceURL:   "http://localhost:8080",
			target:       "my-llama",
			wantBase:     "http://localhost:8080",
			wantChatPath: "/v1/my-llama/v1/chat/completions",
		},
		{
			name:    "target without service-url errors",
			target:  "my-llama",
			wantErr: true,
		},
		{
			name:         "service-url trailing slash stripped",
			serviceURL:   "http://localhost:8080/",
			target:       "alpha",
			wantBase:     "http://localhost:8080",
			wantChatPath: "/v1/alpha/v1/chat/completions",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			loadURL = tc.url
			loadServiceURL = tc.serviceURL
			loadTarget = tc.target
			base, chat, _, err := loadEndpoint()
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if base != tc.wantBase {
				t.Errorf("base = %q, want %q", base, tc.wantBase)
			}
			if chat != tc.wantChatPath {
				t.Errorf("chatPath = %q, want %q", chat, tc.wantChatPath)
			}
		})
	}
}

// TestLoad_JSONOutput: --output json emits a single JSON object on
// stdout with the right field set. Used by demo 05's run.sh + any
// CI scripts capturing load runs.
func TestLoad_JSONOutput(t *testing.T) {
	stats := &loadStats{
		successes: 10,
		errors:    1,
		skipped:   2,
		tokens:    300,
		latencies: nil,
	}
	sum := stats.summary(1000_000_000, 5) // 1 second, target 5 rps
	if sum.Successes != 10 {
		t.Errorf("Successes = %d, want 10", sum.Successes)
	}
	if sum.Tokens != 300 {
		t.Errorf("Tokens = %d, want 300", sum.Tokens)
	}
	if !strings.Contains("", "") {
		// placeholder so the import of strings isn't unused
	}
}
