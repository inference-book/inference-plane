package server

import (
	"encoding/json"
	"net/http"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	inferencev1 "github.com/inference-book/inference-plane/gen/go/inferenceplane/v1"
)

// This file owns the hand-written GET /health probe and the OpenAI
// error-envelope helpers. The v0.2 data-plane router owns the
// OpenAI-compatible POST paths (/v1/chat/completions etc.) and streams
// directly between the operator and the engine pod; this layer no
// longer transcodes inference traffic.

// openAIErrorEnvelope is OpenAI's error response shape:
//
//	{"error": {"message": "...", "type": "..."}}
//
// All non-2xx responses on the daemon's public surface use this
// envelope so existing OpenAI SDKs surface the right exception types
// client-side.
type openAIErrorEnvelope struct {
	Error openAIErrorBody `json:"error"`
}

type openAIErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

// handleHealth implements GET /health. Calls the in-process
// HealthService.Check, then translates the proto enum into the simple
// {"status":"ok"} / {"status":"unhealthy"} shape operators expect.
// Maps SERVING -> 200, anything else -> 503.
func (a *API) handleHealth(w http.ResponseWriter, r *http.Request) {
	resp, err := a.healthClient.Check(r.Context(), &inferencev1.CheckRequest{})
	if err != nil {
		a.writeError(w, status.Convert(err))
		return
	}

	body := map[string]string{}
	httpStatus := http.StatusOK

	switch resp.GetStatus() {
	case inferencev1.CheckResponse_STATUS_SERVING:
		body["status"] = "ok"
	case inferencev1.CheckResponse_STATUS_DEGRADED:
		body["status"] = "degraded"
		if msg := resp.GetMessage(); msg != "" {
			body["message"] = msg
		}
	default:
		body["status"] = "unhealthy"
		if msg := resp.GetMessage(); msg != "" {
			body["message"] = msg
		}
		httpStatus = http.StatusServiceUnavailable
	}

	a.writeJSON(w, httpStatus, body)
}

// writeJSON encodes v as JSON and writes it with the given status code.
// Logs (but does not surface) any encoding failure -- by the time the
// response writer has been touched, the status code is already on the
// wire.
func (a *API) writeJSON(w http.ResponseWriter, statusCode int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		a.logger.Warn("openai: encode response", "err", err)
	}
}

// writeOpenAIError emits a single error in the OpenAI envelope shape.
func (a *API) writeOpenAIError(w http.ResponseWriter, httpStatus int, errType, message string) {
	a.writeJSON(w, httpStatus, openAIErrorEnvelope{
		Error: openAIErrorBody{Message: message, Type: errType},
	})
}

// writeError translates a gRPC status into the OpenAI error envelope.
// Mirrors the codeToHTTP table from chapter 6.5: 4xx upstream passes
// through, 5xx upstream becomes 502, ctx cancel becomes 499.
func (a *API) writeError(w http.ResponseWriter, s *status.Status) {
	httpStatus, errType := codeToHTTP(s.Code())
	a.logger.Warn("openai: gRPC error", "code", s.Code().String(), "http", httpStatus, "msg", s.Message())
	a.writeOpenAIError(w, httpStatus, errType, s.Message())
}

// codeToHTTP maps gRPC status code to HTTP status + OpenAI error type.
// Used by the /health handler; Connect-RPC handlers translate gRPC
// codes to Connect codes natively.
func codeToHTTP(c codes.Code) (int, string) {
	switch c {
	case codes.OK:
		return http.StatusOK, ""
	case codes.InvalidArgument, codes.FailedPrecondition, codes.OutOfRange:
		return http.StatusBadRequest, "invalid_request_error"
	case codes.Unauthenticated:
		return http.StatusUnauthorized, "authentication_error"
	case codes.PermissionDenied:
		return http.StatusForbidden, "permission_error"
	case codes.NotFound:
		return http.StatusNotFound, "not_found_error"
	case codes.AlreadyExists:
		return http.StatusConflict, "invalid_request_error"
	case codes.ResourceExhausted:
		return http.StatusTooManyRequests, "rate_limit_error"
	case codes.Canceled:
		return 499, "client_closed_request"
	case codes.DeadlineExceeded:
		return http.StatusGatewayTimeout, "timeout_error"
	case codes.Unavailable:
		return http.StatusBadGateway, "api_error"
	case codes.Unimplemented:
		return http.StatusNotImplemented, "api_error"
	default:
		return http.StatusInternalServerError, "api_error"
	}
}
