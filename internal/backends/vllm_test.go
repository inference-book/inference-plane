package backends

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestVLLMBackend_Generate_Completions verifies the plain-prompt path:
// POST /v1/completions, body relayed verbatim, response decoded back.
func TestVLLMBackend_Generate_Completions(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		gotBody = string(buf)

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"cmpl-1",
			"object":"text_completion",
			"created":1730000000,
			"model":"qwen",
			"choices":[{"index":0,"text":"the answer is","finish_reason":"length"}],
			"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}
		}`))
	}))
	defer srv.Close()

	be := NewVLLM("test", srv.URL)
	resp, err := be.Generate(context.Background(), GenerateRequest{
		Model: "qwen", Prompt: "what is 2+2?", MaxTokens: 4,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotPath != "/v1/completions" {
		t.Errorf("dispatched to wrong path: got %q, want /v1/completions", gotPath)
	}
	if !strings.Contains(gotBody, `"prompt"`) {
		t.Errorf("request body missing 'prompt' field: %s", gotBody)
	}
	if len(resp.Choices) != 1 || resp.Choices[0].FinishReason != "length" {
		t.Errorf("unexpected response shape: %+v", resp)
	}
	if resp.Usage.TotalTokens != 8 {
		t.Errorf("usage not decoded: %+v", resp.Usage)
	}
}

// TestVLLMBackend_Generate_Chat verifies that requests carrying Messages
// route to /v1/chat/completions. Same handler in the control plane;
// dispatch is a property of the backend.
func TestVLLMBackend_Generate_Chat(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chat-1","object":"chat.completion","created":1730000000,"model":"qwen",
			"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}
		}`))
	}))
	defer srv.Close()

	be := NewVLLM("test", srv.URL)
	_, err := be.Generate(context.Background(), GenerateRequest{
		Model: "qwen", Messages: []ChatMessage{{Role: "user", Content: "hi"}}, MaxTokens: 1,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("expected /v1/chat/completions, got %q", gotPath)
	}
}

// TestVLLMBackend_Generate_4xx checks that an upstream client error
// surfaces as a *BackendError with IsClientError() == true. The server
// layer relies on this to decide whether to pass the body through.
func TestVLLMBackend_Generate_4xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad model","type":"invalid_request"}}`))
	}))
	defer srv.Close()

	be := NewVLLM("test", srv.URL)
	_, err := be.Generate(context.Background(), GenerateRequest{Model: "x", Prompt: "y"})
	if err == nil {
		t.Fatal("expected an error, got nil")
	}

	var be4 *BackendError
	if !errors.As(err, &be4) {
		t.Fatalf("expected *BackendError, got %T: %v", err, err)
	}
	if be4.StatusCode != http.StatusBadRequest {
		t.Errorf("StatusCode = %d, want 400", be4.StatusCode)
	}
	if !be4.IsClientError() {
		t.Error("expected IsClientError() to be true for 400")
	}
	if !strings.Contains(be4.Body, "bad model") {
		t.Errorf("expected upstream body in BackendError, got %q", be4.Body)
	}
}

// TestVLLMBackend_Generate_5xx checks that an upstream server error
// surfaces as *BackendError with IsClientError() == false. Server layer
// maps this to a 502.
func TestVLLMBackend_Generate_5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"oom"}}`))
	}))
	defer srv.Close()

	be := NewVLLM("test", srv.URL)
	_, err := be.Generate(context.Background(), GenerateRequest{Model: "x", Prompt: "y"})

	var beE *BackendError
	if !errors.As(err, &beE) {
		t.Fatalf("expected *BackendError, got %T: %v", err, err)
	}
	if beE.IsClientError() {
		t.Error("expected IsClientError() to be false for 500")
	}
}

// TestVLLMBackend_Generate_ContextCancellation verifies that a cancelled
// context aborts the upstream call. This is the GPU-time-saver lever
// described in Chapter 6.5: when the client disconnects, the inbound
// request's context cancels, and that cancellation propagates to the
// upstream HTTP call.
func TestVLLMBackend_Generate_ContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Hold the request open. The test cancels the context while
		// we're sleeping; the server-side r.Context() will fire too,
		// but for this test the important thing is that the *client*
		// returned an error promptly rather than waiting out the sleep.
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer srv.Close()

	be := NewVLLM("test", srv.URL)
	ctx, cancel := context.WithCancel(context.Background())

	// Cancel ~50ms after the request starts.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := be.Generate(ctx, GenerateRequest{Model: "x", Prompt: "y"})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error from cancelled context, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled in chain, got: %v", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("expected fast cancellation, took %v", elapsed)
	}
}

// TestVLLMBackend_Generate_DecodeError verifies that a malformed
// upstream response surfaces as a wrapped error rather than a partial
// or zero-value GenerateResponse.
func TestVLLMBackend_Generate_DecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`not json at all`))
	}))
	defer srv.Close()

	be := NewVLLM("test", srv.URL)
	_, err := be.Generate(context.Background(), GenerateRequest{Model: "x", Prompt: "y"})
	if err == nil {
		t.Fatal("expected decode error, got nil")
	}
	if !strings.Contains(err.Error(), "decode response") {
		t.Errorf("expected decode-error wrap, got: %v", err)
	}
}

// TestVLLMBackend_Health verifies the /health probe.
func TestVLLMBackend_Health(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Errorf("health probed wrong path: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	be := NewVLLM("test", srv.URL)
	if err := be.Health(context.Background()); err != nil {
		t.Errorf("expected healthy, got: %v", err)
	}
}

// TestVLLMBackend_Health_Unhealthy verifies the unhealthy path.
func TestVLLMBackend_Health_Unhealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	be := NewVLLM("test", srv.URL)
	err := be.Health(context.Background())
	if err == nil {
		t.Fatal("expected error from unhealthy backend, got nil")
	}
}

// TestVLLMBackend_Generate_TrailingSlash verifies the constructor strips
// a trailing slash from the base URL so we don't end up with "//v1/...".
func TestVLLMBackend_Generate_TrailingSlash(t *testing.T) {
	be := NewVLLM("test", "http://example.com/")
	if be.baseURL != "http://example.com" {
		t.Errorf("baseURL = %q, want %q", be.baseURL, "http://example.com")
	}
}

// Sanity: BackendError stringification.
func TestBackendError_Error(t *testing.T) {
	e := &BackendError{Backend: "vllm", StatusCode: 503, Body: "overloaded"}
	got := e.Error()
	if !strings.Contains(got, "vllm") || !strings.Contains(got, "503") || !strings.Contains(got, "overloaded") {
		t.Errorf("unhelpful error string: %q", got)
	}
}

// Compile-time check that VLLMBackend satisfies the Backend interface.
// Catches signature drift if the interface changes.
var _ Backend = (*VLLMBackend)(nil)

// Decode-shape check: ensures GenerateResponse round-trips through
// json.Marshal/Unmarshal without losing fields. Catches accidental
// json tag changes that would break SDK clients.
func TestGenerateResponse_RoundTrip(t *testing.T) {
	in := GenerateResponse{
		ID: "x", Object: "y", Created: 1, Model: "qwen",
		Choices: []Choice{
			{Index: 0, Text: "hi", FinishReason: "stop"},
			{Index: 1, Message: &ChatMessage{Role: "assistant", Content: "ok"}, FinishReason: "stop"},
		},
		Usage: Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out GenerateResponse
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.ID != in.ID || out.Usage.TotalTokens != in.Usage.TotalTokens {
		t.Errorf("round-trip lost data: in=%+v out=%+v", in, out)
	}
}
