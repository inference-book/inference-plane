package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/inference-book/inference-plane/internal/backends"
)

// TestMockEngine_ChatReturnsOpenAIShape: the chat handler decodes an
// OpenAI-shaped request and returns an OpenAI-shaped response with usage,
// so the router (and iplane load session) can treat it like any engine.
func TestMockEngine_ChatReturnsOpenAIShape(t *testing.T) {
	mux := newMockEngineMux(backends.NewMock("t"), "t")
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"mock","messages":[{"role":"user","content":"hi"}],"max_tokens":8}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Choices []struct {
			Message *struct{ Content string } `json:"message"`
		} `json:"choices"`
		Usage struct {
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Choices) == 0 || body.Choices[0].Message == nil {
		t.Fatalf("response missing chat choice/message: %+v", body)
	}
	if body.Usage.CompletionTokens <= 0 {
		t.Errorf("completion_tokens = %d, want > 0", body.Usage.CompletionTokens)
	}
}

// TestMockEngine_Health: /health returns 200 so the router's health-poll
// loop keeps the attached replica in rotation.
func TestMockEngine_Health(t *testing.T) {
	mux := newMockEngineMux(backends.NewMock("t"), "t")
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}
