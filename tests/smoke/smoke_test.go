//go:build smoke

// Package smoke is the v0.1 verification suite. It exercises the
// running stack end-to-end: health probe, plain completion, chat
// completion. Decodes responses into the same backend types the
// control plane uses internally, so a schema drift here surfaces as
// a typed test failure rather than a string-grep miss.
//
// Run via `make smoke` after `make up`. The build tag keeps these
// tests out of the default `go test ./...` (which doesn't need a
// running stack).
package smoke

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/inference-book/inference-plane/internal/backends"
)

// envOr returns the value of env var k, or fallback if unset.
func envOr(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}

// cpURL is the control plane base URL under test.
func cpURL() string { return envOr("CP_URL", "http://localhost:8080") }

// modelID is the model identifier used in completion/chat requests.
// Must match a model the backend (vLLM) is currently serving.
func modelID() string { return envOr("MODEL", "Qwen/Qwen3.5-8B-Instruct") }

// httpClient is shared across tests with a generous timeout for slow
// generations. The control plane's WriteTimeout is 10 minutes; we cap
// the test client at 90 seconds since smoke checks use small max_tokens.
var httpClient = &http.Client{Timeout: 90 * time.Second}

// TestHealth verifies the /health endpoint reports the backend ready.
// servicekit's HealthCheck handler returns {"status":"ok"} when the
// configured ReadyFunc returns true, {"status":"not ready"} (503) otherwise.
func TestHealth(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cpURL()+"/health", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("call /health: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(body))
	}
	if !strings.Contains(string(body), `"status":"ok"`) {
		t.Fatalf("health body did not contain status:ok: %s", string(body))
	}
}

// TestPlainCompletion sends a small completion request and verifies
// the response shape. Asserts on the typed GenerateResponse rather
// than string-grepping the JSON, so a schema change surfaces as a
// compile error instead of a silent test pass.
func TestPlainCompletion(t *testing.T) {
	req := backends.GenerateRequest{
		Model:     modelID(),
		Prompt:    "The capital of France is",
		MaxTokens: 8,
	}
	resp := postCompletion(t, "/v1/completions", req)

	if len(resp.Choices) == 0 {
		t.Fatalf("expected at least one choice, got zero")
	}
	if resp.Choices[0].Text == "" {
		t.Fatalf("first choice has empty text")
	}
	if resp.Choices[0].FinishReason == "" {
		t.Fatalf("first choice has empty finish_reason")
	}
	if resp.Usage.TotalTokens == 0 {
		t.Fatalf("response carries no token usage")
	}
}

// TestChatCompletion exercises the /v1/chat/completions path. Same
// handler in the control plane; the VLLMBackend dispatches to a
// different vLLM endpoint based on the presence of Messages.
func TestChatCompletion(t *testing.T) {
	req := backends.GenerateRequest{
		Model: modelID(),
		Messages: []backends.ChatMessage{
			{Role: "user", Content: "Say hi in five words."},
		},
		MaxTokens: 16,
	}
	resp := postCompletion(t, "/v1/chat/completions", req)

	if len(resp.Choices) == 0 {
		t.Fatalf("expected at least one choice, got zero")
	}
	if resp.Choices[0].Message == nil {
		t.Fatalf("chat response has no message")
	}
	if resp.Choices[0].Message.Role != "assistant" {
		t.Fatalf("expected assistant role, got %q", resp.Choices[0].Message.Role)
	}
	if resp.Choices[0].Message.Content == "" {
		t.Fatalf("chat response has empty content")
	}
}

// postCompletion is the shared POST helper for both completion paths.
func postCompletion(t *testing.T, path string, req backends.GenerateRequest) backends.GenerateResponse {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, cpURL()+path, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(httpReq)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from %s, got %d: %s", path, resp.StatusCode, string(respBody))
	}

	var out backends.GenerateResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		t.Fatalf("decode response from %s: %v\nbody: %s", path, err, string(respBody))
	}
	return out
}
