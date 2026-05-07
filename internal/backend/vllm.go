package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// VLLMBackend talks to a vLLM container's OpenAI-compatible HTTP API.
//
// vLLM speaks OpenAI natively, so this is mostly a relay: forward the
// JSON, propagate the request context for cancellation, decode the
// response, translate transport errors into typed errors so the server
// layer can map them to client-facing HTTP statuses.
type VLLMBackend struct {
	name       string
	baseURL    string
	httpClient *http.Client
}

// NewVLLM constructs a VLLMBackend pointing at the given OpenAI-compatible
// base URL (e.g. "http://vllm:8000"). Trailing slashes are stripped.
//
// The client timeout (5 minutes) is the upper bound for a single inference
// call. Long generations on a busy GPU can take this long; the actual
// cancellation lever is the request context, which the inbound HTTP
// handler derives from the client connection. When the client disconnects,
// the context cancels and this call aborts -- vLLM stops the inference
// instead of finishing work nobody is waiting for.
func NewVLLM(name, baseURL string) *VLLMBackend {
	return &VLLMBackend{
		name:    name,
		baseURL: strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{
			Timeout: 5 * time.Minute,
		},
	}
}

func (v *VLLMBackend) Name() string { return v.name }

// Generate forwards the request to vLLM and returns the decoded response.
//
// Endpoint selection mirrors the OpenAI SDK convention: if Messages is
// populated, route to /v1/chat/completions; otherwise route to
// /v1/completions. Both shapes are part of the OpenAI v1 API and the
// caller doesn't need to care which one vLLM uses internally.
func (v *VLLMBackend) Generate(ctx context.Context, req GenerateRequest) (GenerateResponse, error) {
	endpoint := v.baseURL + "/v1/completions"
	if len(req.Messages) > 0 {
		endpoint = v.baseURL + "/v1/chat/completions"
	}

	body, err := json.Marshal(req)
	if err != nil {
		return GenerateResponse{}, fmt.Errorf("vllm: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return GenerateResponse{}, fmt.Errorf("vllm: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")

	httpResp, err := v.httpClient.Do(httpReq)
	if err != nil {
		// Includes context.Canceled / context.DeadlineExceeded if the
		// caller's context fired. Wrapping with %w preserves the chain
		// so server-side errors.Is(err, context.Canceled) still works.
		return GenerateResponse{}, fmt.Errorf("vllm: call %s: %w", endpoint, err)
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		// Read body for diagnostic context. Cap at 4 KiB so a misbehaving
		// upstream can't blow up our memory or our log lines.
		bodyBytes, _ := io.ReadAll(io.LimitReader(httpResp.Body, 4096))
		return GenerateResponse{}, &BackendError{
			Backend:    v.name,
			StatusCode: httpResp.StatusCode,
			Body:       string(bodyBytes),
		}
	}

	var resp GenerateResponse
	if err := json.NewDecoder(httpResp.Body).Decode(&resp); err != nil {
		return GenerateResponse{}, fmt.Errorf("vllm: decode response: %w", err)
	}
	return resp, nil
}

// Health probes vLLM's /health endpoint. Returns nil for healthy, an
// error otherwise. The server's HealthCheck handler wraps this in a
// CheckResult with timing and message.
func (v *VLLMBackend) Health(ctx context.Context) error {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, v.baseURL+"/health", nil)
	if err != nil {
		return fmt.Errorf("vllm: build health request: %w", err)
	}

	httpResp, err := v.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("vllm: health call: %w", err)
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode != http.StatusOK {
		return fmt.Errorf("vllm: unhealthy (status %d)", httpResp.StatusCode)
	}
	return nil
}

// BackendError is returned when the upstream backend (vLLM in v0.1)
// responds with a non-2xx status. The server layer inspects StatusCode
// to map upstream failure modes to appropriate client-facing statuses
// per the rules in Chapter 6.5: 5xx upstream becomes 502 to the client
// (signaling "upstream trouble" rather than "we are broken"), 4xx is
// passed through unchanged (the client's request was rejected).
type BackendError struct {
	Backend    string
	StatusCode int
	Body       string
}

func (e *BackendError) Error() string {
	return fmt.Sprintf("backend %q returned status %d: %s", e.Backend, e.StatusCode, e.Body)
}

// IsClientError returns true for 4xx upstream statuses, which the
// server layer should pass through to the client unchanged.
func (e *BackendError) IsClientError() bool {
	return e.StatusCode >= 400 && e.StatusCode < 500
}
