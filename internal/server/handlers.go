package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/inference-book/inference-plane/internal/backend"
)

// completionsHandler serves both /v1/completions and /v1/chat/completions.
//
// Both OpenAI shapes deserialize into the same backend.GenerateRequest;
// the difference (Prompt vs Messages) is preserved in the body and the
// VLLMBackend dispatches to the right vLLM endpoint.
//
// The flow follows Chapter 6.5: decode -> validate -> backend.Generate ->
// encode + status mapping. Request ID and structured logging happen in
// the middleware chain; tracing and metrics will land in the next increment.
type completionsHandler struct {
	backend backend.Backend
	logger  *slog.Logger
}

func (h *completionsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
		return
	}

	var req backend.GenerateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "request body is not valid JSON: "+err.Error())
		return
	}

	if err := validate(req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	resp, err := h.backend.Generate(r.Context(), req)
	if err != nil {
		writeBackendError(w, h.logger, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		// Header is already sent; best we can do is log.
		h.logger.Error("response encode failed", "err", err)
	}
}

// validate enforces the minimum the OpenAI v1 API requires from a
// completion request. Backend-specific limits (max prompt length, etc.)
// are the backend's responsibility; the control plane just makes sure
// we have a recognizable request shape before we burn a network round-trip.
func validate(req backend.GenerateRequest) error {
	if req.Model == "" {
		return errors.New("model: required")
	}
	if req.Prompt == "" && len(req.Messages) == 0 {
		return errors.New("prompt or messages: at least one is required")
	}
	if req.Prompt != "" && len(req.Messages) > 0 {
		return errors.New("prompt and messages: only one may be set")
	}
	return nil
}

// writeBackendError maps a backend.Backend error to a client-facing
// HTTP status, following the rule from Chapter 6.5: 4xx upstream
// passes through (the client's request was rejected), 5xx upstream
// becomes 502 (signaling "upstream trouble" rather than "we are
// broken"), and context cancellation becomes 499 (client closed
// request) so we can distinguish caller-cancelled work from real
// backend failures in metrics and logs.
func writeBackendError(w http.ResponseWriter, logger *slog.Logger, err error) {
	// Client gave up before the backend returned. The connection is
	// likely already closed; we still write 499 so middleware logging
	// records the disposition correctly. Distinct from a real backend
	// failure: this is "didn't matter" rather than "broken upstream."
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		writeError(w, 499, "client_closed_request", "client cancelled the request")
		return
	}

	var be *backend.BackendError
	if errors.As(err, &be) {
		if be.IsClientError() {
			// Pass the upstream 4xx through. Body may carry useful detail
			// (token-limit messages, etc.) the client should see.
			writeError(w, be.StatusCode, "upstream_client_error", be.Body)
			return
		}
		// Upstream 5xx -> 502 to the client.
		logger.Warn("backend returned error", "backend", be.Backend, "status", be.StatusCode, "body", be.Body)
		writeError(w, http.StatusBadGateway, "backend_error", "upstream backend returned an error")
		return
	}

	// Anything else (network failure, decode failure, unknown) is a 502.
	logger.Error("backend call failed", "err", err)
	writeError(w, http.StatusBadGateway, "backend_error", err.Error())
}

// errorBody is the OpenAI-shaped error envelope. We mirror the shape
// even on errors so existing OpenAI client SDKs surface useful detail.
type errorBody struct {
	Error errorDetails `json:"error"`
}
type errorDetails struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}

func writeError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorBody{
		Error: errorDetails{Message: message, Type: errType},
	})
}
