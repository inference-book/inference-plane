// Package services implements the gRPC service interfaces generated
// from the protos in inferenceplane.v1. Implementations satisfy the
// gRPC server interface (bare proto types in/out); the connect-rpc
// surface and the grpc-gateway HTTP surface are bindings on top --
// each is a thin adapter that calls the gRPC service in-process.
//
// One gRPC implementation, multiple transport bindings -- the same
// pattern used in lilbattle and other projects in this stack.
package services

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otelcodes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	gcodes "google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	inferencev1 "github.com/inference-book/inference-plane/gen/go/inferenceplane/v1"
	"github.com/inference-book/inference-plane/internal/backends"
	"github.com/inference-book/inference-plane/internal/metrics"
	"github.com/inference-book/inference-plane/internal/telemetry"
)

// tracer for handler-level spans. Lazy via the global TracerProvider --
// the no-op tracer is the default when telemetry.Init has not run.
var tracer = otel.Tracer("inference-plane/services")

// InferenceServer implements inferencev1.InferenceServiceServer. The
// connect-rpc and grpc-gateway adapters in internal/web/server/ wrap
// it for their respective transports.
//
// The backend instance is shared across both Complete and ChatComplete:
// vLLM (and most other backends) speak both shapes through the same
// HTTP client; the dispatch happens inside backends.VLLMBackend.Generate
// based on whether the request carries Messages or a Prompt.
type InferenceServer struct {
	inferencev1.UnimplementedInferenceServiceServer
	backend backends.Backend
	metrics *metrics.Recorder
	cost    *metrics.CostRecorder
}

// NewInferenceServer constructs an InferenceServer over the given backend.
// metrics and cost may be nil (no-op recordings) for tests that don't
// initialize the OTel SDK.
func NewInferenceServer(b backends.Backend, m *metrics.Recorder, cost *metrics.CostRecorder) *InferenceServer {
	return &InferenceServer{backend: b, metrics: m, cost: cost}
}

// compile-time check that InferenceServer satisfies the gRPC interface.
var _ inferencev1.InferenceServiceServer = (*InferenceServer)(nil)

// Complete handles plain prompt completion. Maps to OpenAI's
// POST /v1/completions via the proto's google.api.http annotation.
func (s *InferenceServer) Complete(ctx context.Context, in *inferencev1.CompleteRequest) (*inferencev1.CompleteResponse, error) {
	// Metric record deferred so every exit path (validation failure,
	// backend error, success) emits the same counter+histogram pair.
	// Cost records the same duration into inference.active.seconds.total.
	start := time.Now()
	status_ := "success"
	model := "unknown"
	var completionTokens int32
	defer func() {
		dur := time.Since(start).Seconds()
		s.metrics.RecordRequest(ctx, model, status_, dur)
		s.cost.RecordActive(ctx, model, dur)
		if status_ == "success" && completionTokens > 0 {
			s.metrics.RecordTokens(ctx, model, int64(completionTokens))
		}
	}()

	if in.Model == "" {
		status_ = "validation_error"
		return nil, status.Error(gcodes.InvalidArgument, "model: required")
	}
	model = in.Model
	if in.Prompt == "" {
		status_ = "validation_error"
		return nil, status.Error(gcodes.InvalidArgument, "prompt: required")
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
		span.SetStatus(otelcodes.Error, err.Error())
		status_ = backendErrorStatus(err)
		return nil, mapBackendError(s.backend.Name(), err)
	}

	annotateSuccess(span, bResp)
	completionTokens = int32(bResp.Usage.CompletionTokens)

	out := &inferencev1.CompleteResponse{
		Id:      bResp.ID,
		Object:  bResp.Object,
		Created: bResp.Created,
		Model:   bResp.Model,
		Usage: &inferencev1.Usage{
			PromptTokens:     int32(bResp.Usage.PromptTokens),
			CompletionTokens: completionTokens,
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
	return out, nil
}

// ChatComplete handles chat-style completion. Maps to OpenAI's
// POST /v1/chat/completions.
func (s *InferenceServer) ChatComplete(ctx context.Context, in *inferencev1.ChatCompleteRequest) (*inferencev1.ChatCompleteResponse, error) {
	start := time.Now()
	status_ := "success"
	model := "unknown"
	var completionTokens int32
	defer func() {
		dur := time.Since(start).Seconds()
		s.metrics.RecordRequest(ctx, model, status_, dur)
		s.cost.RecordActive(ctx, model, dur)
		if status_ == "success" && completionTokens > 0 {
			s.metrics.RecordTokens(ctx, model, int64(completionTokens))
		}
	}()

	if in.Model == "" {
		status_ = "validation_error"
		return nil, status.Error(gcodes.InvalidArgument, "model: required")
	}
	model = in.Model
	if len(in.Messages) == 0 {
		status_ = "validation_error"
		return nil, status.Error(gcodes.InvalidArgument, "messages: at least one required")
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
		span.SetStatus(otelcodes.Error, err.Error())
		status_ = backendErrorStatus(err)
		return nil, mapBackendError(s.backend.Name(), err)
	}

	annotateSuccess(span, bResp)
	completionTokens = int32(bResp.Usage.CompletionTokens)

	out := &inferencev1.ChatCompleteResponse{
		Id:      bResp.ID,
		Object:  bResp.Object,
		Created: bResp.Created,
		Model:   bResp.Model,
		Usage: &inferencev1.Usage{
			PromptTokens:     int32(bResp.Usage.PromptTokens),
			CompletionTokens: completionTokens,
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
	return out, nil
}

// backendErrorStatus returns the metric status label for a backend
// error. Mirrors the codeToHTTP mapping in internal/web/server so
// metrics and HTTP statuses describe the same outcome consistently.
func backendErrorStatus(err error) string {
	if errors.Is(err, context.Canceled) {
		return "client_closed"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	var be *backends.BackendError
	if errors.As(err, &be) {
		if be.IsClientError() {
			return "upstream_client_error"
		}
	}
	return "backend_error"
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
	span.SetStatus(otelcodes.Ok, "")
}

// mapBackendError translates a backend error to a gRPC status with the
// right code per Chapter 6.5: client cancellation -> Canceled, upstream
// 4xx -> InvalidArgument (the body carries useful detail like
// token-limit messages), upstream 5xx -> Unavailable. The web layer
// translates these to HTTP statuses (and to OpenAI's error envelope)
// or to connect.Code (which connect-rpc maps to HTTP statuses itself).
func mapBackendError(backendName string, err error) error {
	if errors.Is(err, context.Canceled) {
		return status.Error(gcodes.Canceled, err.Error())
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return status.Error(gcodes.DeadlineExceeded, err.Error())
	}
	var be *backends.BackendError
	if errors.As(err, &be) {
		if be.IsClientError() {
			return status.Errorf(gcodes.InvalidArgument, "upstream %s: %s", be.Backend, be.Body)
		}
		return status.Errorf(gcodes.Unavailable, "backend %s returned status %d", be.Backend, be.StatusCode)
	}
	return status.Error(gcodes.Unavailable, err.Error())
}

// completionID generates a unique-enough completion ID for backends
// that don't provide one. Format mirrors OpenAI's "cmpl-..." style.
// Currently unused -- vLLM provides IDs -- but kept here for backends
// that might not.
func completionID() string {
	return fmt.Sprintf("cmpl-%s", strconv.FormatInt(time.Now().UnixNano(), 36))
}
