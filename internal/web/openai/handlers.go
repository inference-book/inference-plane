// Package openai serves OpenAI-compatible HTTP routes by translating
// OpenAI JSON to proto, calling the connect-rpc service in-process,
// and translating proto responses back to OpenAI JSON. Connect-rpc
// errors map to OpenAI's error-envelope shape so existing OpenAI
// SDKs see the responses they expect on failure paths.
//
// In-process call (not loopback HTTP) keeps the cost low: no
// serialization round-trip, no extra network hop, and the trace
// context propagates naturally through the same Go context.Context
// the inbound HTTP request carries.
package openai

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"

	inferencev1 "github.com/inference-book/inference-plane/gen/go/inferenceplane/v1"
	"github.com/inference-book/inference-plane/internal/services"
)

// Handler serves the OpenAI-compatible HTTP routes:
//
//	POST /v1/completions       -> InferenceService.Complete
//	POST /v1/chat/completions  -> InferenceService.ChatComplete
//	GET  /health               -> HealthService.Check
//
// Wire it onto a *http.ServeMux via Handler.Register, or call the
// individual ServeHTTP-style methods directly.
type Handler struct {
	inference *services.InferenceServer
	health    *services.HealthServer
	logger    *slog.Logger
}

// New constructs a Handler over the given service implementations.
func New(inference *services.InferenceServer, health *services.HealthServer, logger *slog.Logger) *Handler {
	return &Handler{inference: inference, health: health, logger: logger}
}

// Register mounts the OpenAI routes on the given mux. The connect-rpc
// handler (mounted separately by the entrypoint) handles the typed
// /inferenceplane.v1.* paths; this only handles the OpenAI-shaped HTTP.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/completions", h.completions)
	mux.HandleFunc("POST /v1/chat/completions", h.chatCompletions)
	mux.HandleFunc("GET /health", h.healthCheck)
}

// ── /v1/completions ────────────────────────────────────────────────

func (h *Handler) completions(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20)) // 1 MiB cap
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "request body too large or unreadable: "+err.Error())
		return
	}

	req := &inferencev1.CompleteRequest{}
	if err := unmarshalOpts.Unmarshal(body, req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON: "+err.Error())
		return
	}

	resp, err := h.inference.Complete(r.Context(), connect.NewRequest(req))
	if err != nil {
		h.writeConnectError(w, err)
		return
	}

	out, err := marshalOpts.Marshal(resp.Msg)
	if err != nil {
		h.logger.Error("openai: marshal completions response", "err", err)
		writeError(w, http.StatusInternalServerError, "api_error", "failed to marshal response")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// ── /v1/chat/completions ───────────────────────────────────────────

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "request body too large or unreadable: "+err.Error())
		return
	}

	req := &inferencev1.ChatCompleteRequest{}
	if err := unmarshalOpts.Unmarshal(body, req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON: "+err.Error())
		return
	}

	resp, err := h.inference.ChatComplete(r.Context(), connect.NewRequest(req))
	if err != nil {
		h.writeConnectError(w, err)
		return
	}

	out, err := marshalOpts.Marshal(resp.Msg)
	if err != nil {
		h.logger.Error("openai: marshal chat response", "err", err)
		writeError(w, http.StatusInternalServerError, "api_error", "failed to marshal response")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// ── /health ────────────────────────────────────────────────────────

func (h *Handler) healthCheck(w http.ResponseWriter, r *http.Request) {
	resp, err := h.health.Check(r.Context(), connect.NewRequest(&inferencev1.CheckRequest{}))
	if err != nil {
		// Health.Check returns nil error -- a non-nil here is an
		// implementation bug, surface it explicitly so we notice.
		h.logger.Error("openai: health check returned error", "err", err)
		writeError(w, http.StatusInternalServerError, "api_error", err.Error())
		return
	}

	status := http.StatusOK
	if resp.Msg.Status != inferencev1.CheckResponse_STATUS_SERVING {
		status = http.StatusServiceUnavailable
	}

	out, _ := marshalOpts.Marshal(resp.Msg)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(out)
}

// ── error envelope ─────────────────────────────────────────────────

// writeConnectError maps a connect.Error to an OpenAI-shaped error
// envelope and the matching HTTP status. Per Chapter 6.5 rules:
// 4xx upstream passes through, 5xx upstream becomes 502, ctx cancel
// becomes 499. We translate connect.Code to those statuses here.
func (h *Handler) writeConnectError(w http.ResponseWriter, err error) {
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) {
		h.logger.Error("openai: non-connect error escaped service", "err", err)
		writeError(w, http.StatusInternalServerError, "api_error", err.Error())
		return
	}

	status, errType := connectCodeToHTTP(connectErr.Code())
	writeError(w, status, errType, connectErr.Message())
}

// connectCodeToHTTP maps connect.Code to HTTP status + OpenAI error type.
func connectCodeToHTTP(code connect.Code) (int, string) {
	switch code {
	case connect.CodeInvalidArgument, connect.CodeFailedPrecondition, connect.CodeOutOfRange:
		return http.StatusBadRequest, "invalid_request_error"
	case connect.CodeUnauthenticated:
		return http.StatusUnauthorized, "authentication_error"
	case connect.CodePermissionDenied:
		return http.StatusForbidden, "permission_error"
	case connect.CodeNotFound:
		return http.StatusNotFound, "not_found_error"
	case connect.CodeAlreadyExists:
		return http.StatusConflict, "invalid_request_error"
	case connect.CodeResourceExhausted:
		return http.StatusTooManyRequests, "rate_limit_error"
	case connect.CodeCanceled:
		return 499, "client_closed_request"
	case connect.CodeDeadlineExceeded:
		return http.StatusGatewayTimeout, "timeout_error"
	case connect.CodeUnavailable:
		return http.StatusBadGateway, "api_error"
	case connect.CodeUnimplemented:
		return http.StatusNotImplemented, "api_error"
	default:
		return http.StatusInternalServerError, "api_error"
	}
}

// errorEnvelope mirrors OpenAI's HTTP error response shape so existing
// OpenAI SDKs surface useful detail on failure paths.
type errorEnvelope struct {
	Error errorDetail `json:"error"`
}
type errorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}

func writeError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorEnvelope{
		Error: errorDetail{Message: message, Type: errType},
	})
}

// JSON marshaling options. UseProtoNames=true emits snake_case
// (matching OpenAI's wire format) instead of protojson's default
// lowerCamelCase. EmitUnpopulated=false keeps default-zero fields
// out of the response, matching OpenAI's "absent if not set" pattern.
var (
	marshalOpts = protojson.MarshalOptions{
		UseProtoNames:   true,
		EmitUnpopulated: false,
	}
	unmarshalOpts = protojson.UnmarshalOptions{
		DiscardUnknown: true, // tolerate fields the OpenAI SDK adds in the future
	}
)
