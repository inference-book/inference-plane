package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	inferencev1 "github.com/inference-book/inference-plane/gen/go/inferenceplane/v1"
)

// This file owns the OpenAI public wire format. Each handler decodes
// the incoming JSON into a Go struct, calls the in-process gRPC
// service via the local client, and re-encodes the response into the
// JSON shape OpenAI clients expect. Stdlib encoding/json is used for
// both directions -- it emits int64 as a JSON number (matching
// OpenAI's "created" field) and gives us full control over the wire
// shape, neither of which protojson would.

// openAICompletionRequest is the OpenAI POST /v1/completions request body.
type openAICompletionRequest struct {
	Model       string   `json:"model"`
	Prompt      string   `json:"prompt"`
	MaxTokens   int32    `json:"max_tokens,omitempty"`
	Temperature float64  `json:"temperature,omitempty"`
	TopP        float64  `json:"top_p,omitempty"`
	Stop        []string `json:"stop,omitempty"`
	Stream      bool     `json:"stream,omitempty"`
}

// openAIChatCompletionRequest is the OpenAI POST /v1/chat/completions
// request body. Messages instead of a single prompt; otherwise the
// same parameters.
type openAIChatCompletionRequest struct {
	Model       string             `json:"model"`
	Messages    []openAIChatMessage `json:"messages"`
	MaxTokens   int32              `json:"max_tokens,omitempty"`
	Temperature float64            `json:"temperature,omitempty"`
	TopP        float64            `json:"top_p,omitempty"`
	Stop        []string           `json:"stop,omitempty"`
	Stream      bool               `json:"stream,omitempty"`
}

// openAIChatMessage matches OpenAI's message shape: role + content.
type openAIChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// openAICompletionResponse is the OpenAI completion response shape.
// Note: created is an int64 emitted as a JSON number by stdlib json,
// which is what OpenAI clients expect. (protojson would emit it as a
// string -- the original reason this layer exists.)
type openAICompletionResponse struct {
	ID      string                   `json:"id"`
	Object  string                   `json:"object"`
	Created int64                    `json:"created"`
	Model   string                   `json:"model"`
	Choices []openAICompletionChoice `json:"choices"`
	Usage   openAIUsage              `json:"usage"`
}

// openAIChatCompletionResponse is the OpenAI chat completion response shape.
type openAIChatCompletionResponse struct {
	ID      string                       `json:"id"`
	Object  string                       `json:"object"`
	Created int64                        `json:"created"`
	Model   string                       `json:"model"`
	Choices []openAIChatCompletionChoice `json:"choices"`
	Usage   openAIUsage                  `json:"usage"`
}

// openAICompletionChoice is one generated completion in completions[].
type openAICompletionChoice struct {
	Index        int32  `json:"index"`
	Text         string `json:"text"`
	FinishReason string `json:"finish_reason"`
}

// openAIChatCompletionChoice is one generated message in chat completions[].
type openAIChatCompletionChoice struct {
	Index        int32              `json:"index"`
	Message      *openAIChatMessage `json:"message,omitempty"`
	FinishReason string             `json:"finish_reason"`
}

// openAIUsage is OpenAI's per-request token accounting.
type openAIUsage struct {
	PromptTokens     int32 `json:"prompt_tokens"`
	CompletionTokens int32 `json:"completion_tokens"`
	TotalTokens      int32 `json:"total_tokens"`
}

// openAIErrorEnvelope is OpenAI's error response shape:
//
//	{"error": {"message": "...", "type": "..."}}
//
// All non-2xx responses on the OpenAI surface use this envelope so
// existing OpenAI SDKs surface the right exception types client-side.
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

// handleComplete implements POST /v1/completions.
func (a *API) handleComplete(w http.ResponseWriter, r *http.Request) {
	var req openAICompletionRequest
	if err := decodeJSON(r, &req); err != nil {
		a.writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	protoReq := &inferencev1.CompleteRequest{
		Model:       req.Model,
		Prompt:      req.Prompt,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stop:        req.Stop,
		Stream:      req.Stream,
	}

	protoResp, err := a.inferenceClient.Complete(r.Context(), protoReq)
	if err != nil {
		a.writeError(w, status.Convert(err))
		return
	}

	out := openAICompletionResponse{
		ID:      protoResp.GetId(),
		Object:  protoResp.GetObject(),
		Created: protoResp.GetCreated(),
		Model:   protoResp.GetModel(),
		Usage: openAIUsage{
			PromptTokens:     protoResp.GetUsage().GetPromptTokens(),
			CompletionTokens: protoResp.GetUsage().GetCompletionTokens(),
			TotalTokens:      protoResp.GetUsage().GetTotalTokens(),
		},
	}
	for _, c := range protoResp.GetChoices() {
		out.Choices = append(out.Choices, openAICompletionChoice{
			Index:        c.GetIndex(),
			Text:         c.GetText(),
			FinishReason: c.GetFinishReason(),
		})
	}

	a.writeJSON(w, http.StatusOK, out)
}

// handleChatComplete implements POST /v1/chat/completions.
func (a *API) handleChatComplete(w http.ResponseWriter, r *http.Request) {
	var req openAIChatCompletionRequest
	if err := decodeJSON(r, &req); err != nil {
		a.writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	protoReq := &inferencev1.ChatCompleteRequest{
		Model:       req.Model,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stop:        req.Stop,
		Stream:      req.Stream,
	}
	for _, m := range req.Messages {
		protoReq.Messages = append(protoReq.Messages, &inferencev1.ChatMessage{
			Role:    m.Role,
			Content: m.Content,
		})
	}

	protoResp, err := a.inferenceClient.ChatComplete(r.Context(), protoReq)
	if err != nil {
		a.writeError(w, status.Convert(err))
		return
	}

	out := openAIChatCompletionResponse{
		ID:      protoResp.GetId(),
		Object:  protoResp.GetObject(),
		Created: protoResp.GetCreated(),
		Model:   protoResp.GetModel(),
		Usage: openAIUsage{
			PromptTokens:     protoResp.GetUsage().GetPromptTokens(),
			CompletionTokens: protoResp.GetUsage().GetCompletionTokens(),
			TotalTokens:      protoResp.GetUsage().GetTotalTokens(),
		},
	}
	for _, c := range protoResp.GetChoices() {
		choice := openAIChatCompletionChoice{
			Index:        c.GetIndex(),
			FinishReason: c.GetFinishReason(),
		}
		if m := c.GetMessage(); m != nil {
			choice.Message = &openAIChatMessage{
				Role:    m.GetRole(),
				Content: m.GetContent(),
			}
		}
		out.Choices = append(out.Choices, choice)
	}

	a.writeJSON(w, http.StatusOK, out)
}

// decodeJSON reads the request body into v, rejecting unknown fields
// so client typos surface as 400s rather than silent drops.
func decodeJSON(r *http.Request, v any) error {
	if r.Body == nil {
		return errors.New("empty request body")
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
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
// Used by the OpenAI handlers; Connect-RPC handlers translate gRPC
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
