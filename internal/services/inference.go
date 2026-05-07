// Package services implements the gRPC handlers that the connect-rpc
// and grpc-gateway code generates expect. Implementations wrap a
// backends.Backend and translate between proto types and the backend's
// transport-agnostic Go types.
package services

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	inferencev1 "github.com/inference-book/inference-plane/gen/go/inferenceplane/v1"
	"github.com/inference-book/inference-plane/gen/go/inferenceplane/v1/inferenceplanev1connect"
	"github.com/inference-book/inference-plane/internal/backends"
	"github.com/inference-book/inference-plane/internal/telemetry"
)

// tracer for handler-level spans. Lazy via the global TracerProvider --
// when telemetry.Init has not run, this is the no-op tracer.
var tracer = otel.Tracer("inference-plane/services")

// InferenceServer implements inferenceplanev1connect.InferenceServiceHandler
// by translating between proto types and the backend interface. The
// backend instance is shared across both Complete and ChatComplete --
// vLLM (and most other backends) speak both shapes through the same
// HTTP client; the dispatch happens inside backends.VLLMBackend.Generate
// based on whether the request carries Messages or a Prompt.
type InferenceServer struct {
	backend backends.Backend
}

// NewInferenceServer constructs an InferenceServer over the given backend.
func NewInferenceServer(b backends.Backend) *InferenceServer {
	return &InferenceServer{backend: b}
}

// compile-time check that InferenceServer satisfies the connect handler
// interface. Catches signature drift if buf regen changes the interface.
var _ inferenceplanev1connect.InferenceServiceHandler = (*InferenceServer)(nil)

// Complete handles a plain prompt completion request. Maps to OpenAI's
// POST /v1/completions via the grpc-gateway annotation on the proto.
func (s *InferenceServer) Complete(
	ctx context.Context,
	req *connect.Request[inferencev1.CompleteRequest],
) (*connect.Response[inferencev1.CompleteResponse], error) {
	in := req.Msg
	if in.Model == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("model: required"))
	}
	if in.Prompt == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("prompt: required"))
	}

	ctx, span := tracer.Start(ctx, "backend.generate",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String(telemetry.AttrInferenceModel, in.Model),
			attribute.Int(telemetry.AttrInferenceMaxTokens, int(in.MaxTokens)),
		),
	)
	defer span.End()

	bResp, err := s.backend.Generate(ctx, backends.GenerateRequest{
		Model:       in.Model,
		Prompt:      in.Prompt,
		MaxTokens:   int(in.MaxTokens),
		Temperature: in.Temperature,
		TopP:        in.TopP,
		Stop:        in.Stop,
		Stream:      in.Stream,
	})
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, mapBackendError(s.backend.Name(), err)
	}

	annotateSuccess(span, bResp)

	out := &inferencev1.CompleteResponse{
		Id:      bResp.ID,
		Object:  bResp.Object,
		Created: bResp.Created,
		Model:   bResp.Model,
		Usage: &inferencev1.Usage{
			PromptTokens:     int32(bResp.Usage.PromptTokens),
			CompletionTokens: int32(bResp.Usage.CompletionTokens),
			TotalTokens:      int32(bResp.Usage.TotalTokens),
		},
	}
	for _, c := range bResp.Choices {
		out.Choices = append(out.Choices, &inferencev1.CompleteChoice{
			Index:        int32(c.Index),
			Text:         c.Text,
			FinishReason: c.FinishReason,
		})
	}
	return connect.NewResponse(out), nil
}

// ChatComplete handles chat-style completion. Maps to OpenAI's
// POST /v1/chat/completions.
func (s *InferenceServer) ChatComplete(
	ctx context.Context,
	req *connect.Request[inferencev1.ChatCompleteRequest],
) (*connect.Response[inferencev1.ChatCompleteResponse], error) {
	in := req.Msg
	if in.Model == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("model: required"))
	}
	if len(in.Messages) == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("messages: at least one required"))
	}

	ctx, span := tracer.Start(ctx, "backend.generate",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String(telemetry.AttrInferenceModel, in.Model),
			attribute.Int(telemetry.AttrInferenceMaxTokens, int(in.MaxTokens)),
		),
	)
	defer span.End()

	msgs := make([]backends.ChatMessage, 0, len(in.Messages))
	for _, m := range in.Messages {
		msgs = append(msgs, backends.ChatMessage{Role: m.Role, Content: m.Content})
	}

	bResp, err := s.backend.Generate(ctx, backends.GenerateRequest{
		Model:       in.Model,
		Messages:    msgs,
		MaxTokens:   int(in.MaxTokens),
		Temperature: in.Temperature,
		TopP:        in.TopP,
		Stop:        in.Stop,
		Stream:      in.Stream,
	})
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, mapBackendError(s.backend.Name(), err)
	}

	annotateSuccess(span, bResp)

	out := &inferencev1.ChatCompleteResponse{
		Id:      bResp.ID,
		Object:  bResp.Object,
		Created: bResp.Created,
		Model:   bResp.Model,
		Usage: &inferencev1.Usage{
			PromptTokens:     int32(bResp.Usage.PromptTokens),
			CompletionTokens: int32(bResp.Usage.CompletionTokens),
			TotalTokens:      int32(bResp.Usage.TotalTokens),
		},
	}
	for _, c := range bResp.Choices {
		choice := &inferencev1.ChatChoice{
			Index:        int32(c.Index),
			FinishReason: c.FinishReason,
		}
		if c.Message != nil {
			choice.Message = &inferencev1.ChatMessage{Role: c.Message.Role, Content: c.Message.Content}
		}
		out.Choices = append(out.Choices, choice)
	}
	return connect.NewResponse(out), nil
}

// annotateSuccess attaches response-derived attributes to the active
// span. The trace becomes searchable by token counts and finish reason
// (Chapter 6.6.3 promise).
func annotateSuccess(span trace.Span, bResp backends.GenerateResponse) {
	span.SetAttributes(
		attribute.Int(telemetry.AttrInferencePromptTokens, bResp.Usage.PromptTokens),
		attribute.Int(telemetry.AttrInferenceCompletionTokens, bResp.Usage.CompletionTokens),
		attribute.Int(telemetry.AttrInferenceTotalTokens, bResp.Usage.TotalTokens),
	)
	if len(bResp.Choices) > 0 {
		span.SetAttributes(attribute.String(telemetry.AttrInferenceFinishReason, bResp.Choices[0].FinishReason))
	}
	span.SetStatus(codes.Ok, "")
}

// mapBackendError translates a backend error to a connect.Error with
// the right code per Chapter 6.5: client cancellation -> Canceled,
// upstream 4xx -> InvalidArgument (the body carries useful detail),
// upstream 5xx and anything else -> Unavailable.
func mapBackendError(backendName string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return connect.NewError(connect.CodeCanceled, err)
	}
	var be *backends.BackendError
	if errors.As(err, &be) {
		if be.IsClientError() {
			return connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("upstream %s: %s", be.Backend, be.Body))
		}
		return connect.NewError(connect.CodeUnavailable,
			fmt.Errorf("backend %s returned status %d", be.Backend, be.StatusCode))
	}
	return connect.NewError(connect.CodeUnavailable, err)
}

// completionID generates a unique-enough completion ID for backends
// that don't provide one. Format mirrors OpenAI's "cmpl-..." style.
// Currently unused -- vLLM provides IDs -- but kept here for future
// backends that might not.
func completionID() string {
	return "cmpl-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}
