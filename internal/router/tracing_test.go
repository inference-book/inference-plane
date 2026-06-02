package router

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// setupTracingCapture wires the global TracerProvider to an
// in-memory exporter and registers the W3C TraceContext propagator
// so the router's traceparent injection matches production wiring.
// Returns the exporter so tests can introspect ended spans.
//
// Each test gets a fresh provider so observations don't bleed across
// cases; the provider replaces otel's global until the test ends.
func setupTracingCapture(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(exp),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	prevTP := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	})
	return exp
}

// spanAttr returns the string value of the named attribute on a
// span, or "" if absent. Test helper.
func spanAttr(span sdktrace.ReadOnlySpan, key string) string {
	for _, a := range span.Attributes() {
		if string(a.Key) == key {
			return a.Value.AsString()
		}
	}
	return ""
}

func TestRouter_DeployID_EmitsSpanWithAttrs(t *testing.T) {
	exp := setupTracingCapture(t)

	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"ok"}`)
	}))
	defer engine.Close()

	r := New(&fakeDeploymentClient{
		describe: func(_ *provisionerv1.DescribeDeploymentRequest) (*provisionerv1.DescribeDeploymentResponse, error) {
			return &provisionerv1.DescribeDeploymentResponse{Deployment: &provisionerv1.Deployment{
				Id:             "my-llama",
				Model:          "Qwen/Qwen2.5-7B-Instruct",
				State:          provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
				EngineEndpoint: engine.URL,
			}}, nil
		},
	}, nil)
	srv := httptest.NewServer(serveThroughMux(r))
	defer srv.Close()

	resp, _ := http.Post(srv.URL+"/v1/my-llama/v1/chat/completions", "application/json", strings.NewReader(`{}`))
	resp.Body.Close()

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	span := spans[0]
	if span.Name != spanNameDispatch {
		t.Errorf("span name = %q, want %q", span.Name, spanNameDispatch)
	}
	if span.SpanKind != trace.SpanKindServer {
		t.Errorf("span kind = %v, want Server", span.SpanKind)
	}
	if got := spanAttrFromStub(span, AttrRouterMatch); got != routeMatchDeployID {
		t.Errorf("%s = %q, want %q", AttrRouterMatch, got, routeMatchDeployID)
	}
	if got := spanAttrFromStub(span, AttrRouterDeployID); got != "my-llama" {
		t.Errorf("%s = %q, want my-llama", AttrRouterDeployID, got)
	}
	if got := spanAttrFromStub(span, AttrRouterModel); got != "Qwen/Qwen2.5-7B-Instruct" {
		t.Errorf("%s = %q", AttrRouterModel, got)
	}
	if got := spanAttrFromStub(span, AttrRouterUpstream); got != engine.URL {
		t.Errorf("%s = %q, want %q", AttrRouterUpstream, got, engine.URL)
	}
	if got := spanAttrFromStub(span, AttrRouterStatus); got != "success" {
		t.Errorf("%s = %q, want success", AttrRouterStatus, got)
	}
	if span.Status.Code != codes.Ok {
		t.Errorf("span status code = %v, want Ok", span.Status.Code)
	}
}

func TestRouter_Flat_EmitsSpanWithMatchAttr(t *testing.T) {
	exp := setupTracingCapture(t)

	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}))
	defer engine.Close()

	r := New(&flatFakeClient{
		fakeDeploymentClient: &fakeDeploymentClient{},
		list: func(_ *provisionerv1.ListDeploymentsRequest) (*provisionerv1.ListDeploymentsResponse, error) {
			return &provisionerv1.ListDeploymentsResponse{Deployments: []*provisionerv1.Deployment{
				{Id: "x", Model: "m", State: provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING, EngineEndpoint: engine.URL},
			}}, nil
		},
	}, nil)
	srv := httptest.NewServer(serveThroughMux(r))
	defer srv.Close()

	resp, _ := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"m"}`))
	resp.Body.Close()

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	if got := spanAttrFromStub(spans[0], AttrRouterMatch); got != routeMatchFlat {
		t.Errorf("flat URL should emit match=%q; got %q", routeMatchFlat, got)
	}
}

func TestRouter_ErrorResponse_SpanStatusError(t *testing.T) {
	exp := setupTracingCapture(t)

	// PENDING deployment -> 503 -> span.Status should be Error.
	r := New(&fakeDeploymentClient{
		describe: func(_ *provisionerv1.DescribeDeploymentRequest) (*provisionerv1.DescribeDeploymentResponse, error) {
			return &provisionerv1.DescribeDeploymentResponse{Deployment: &provisionerv1.Deployment{
				Id: "warmup", Model: "m",
				State: provisionerv1.DeploymentState_DEPLOYMENT_STATE_PENDING,
			}}, nil
		},
	}, nil)
	srv := httptest.NewServer(serveThroughMux(r))
	defer srv.Close()

	resp, _ := http.Post(srv.URL+"/v1/warmup/v1/chat/completions", "application/json", strings.NewReader(`{}`))
	resp.Body.Close()

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	span := spans[0]
	if span.Status.Code != codes.Error {
		t.Errorf("span status code = %v, want Error (response was 503)", span.Status.Code)
	}
	if got := spanAttrFromStub(span, AttrRouterStatus); got != "engine_error" {
		t.Errorf("%s = %q, want engine_error", AttrRouterStatus, got)
	}
}

func TestRouter_InjectsTraceparentOnEngineRequest(t *testing.T) {
	setupTracingCapture(t)

	// Capture the traceparent header the engine sees. If the router
	// is propagating correctly, this is non-empty and parseable as
	// a W3C traceparent value.
	var engineTraceparent atomic.Value
	engineTraceparent.Store("")
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tp := r.Header.Get("traceparent"); tp != "" {
			engineTraceparent.Store(tp)
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	defer engine.Close()

	r := New(&fakeDeploymentClient{
		describe: func(_ *provisionerv1.DescribeDeploymentRequest) (*provisionerv1.DescribeDeploymentResponse, error) {
			return &provisionerv1.DescribeDeploymentResponse{Deployment: &provisionerv1.Deployment{
				Id: "x", State: provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING, EngineEndpoint: engine.URL,
			}}, nil
		},
	}, nil)
	srv := httptest.NewServer(serveThroughMux(r))
	defer srv.Close()

	resp, _ := http.Post(srv.URL+"/v1/x/v1/chat/completions", "application/json", strings.NewReader(`{}`))
	resp.Body.Close()

	tp, _ := engineTraceparent.Load().(string)
	if tp == "" {
		t.Fatal("engine did not receive a traceparent header; router is not propagating")
	}
	// W3C traceparent format: version-traceid-spanid-flags
	// 00-<32 hex>-<16 hex>-<2 hex>
	parts := strings.Split(tp, "-")
	if len(parts) != 4 {
		t.Errorf("traceparent %q does not match W3C 4-part format", tp)
	}
	if len(parts[0]) != 2 || parts[0] != "00" {
		t.Errorf("traceparent version = %q, want 00", parts[0])
	}
	if len(parts[1]) != 32 {
		t.Errorf("traceparent trace_id = %q (len=%d), want 32-hex", parts[1], len(parts[1]))
	}
}

func TestRouter_EngineTraceparentChainsToRouterSpan(t *testing.T) {
	exp := setupTracingCapture(t)

	// Capture the trace_id from the traceparent header so the test
	// can compare it to the router span's trace_id -- if they match,
	// an engine emitting a span under that traceparent would chain
	// correctly under our router span.
	var engineTraceID atomic.Value
	engineTraceID.Store("")
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tp := r.Header.Get("traceparent"); tp != "" {
			parts := strings.Split(tp, "-")
			if len(parts) >= 2 {
				engineTraceID.Store(parts[1])
			}
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	defer engine.Close()

	r := New(&fakeDeploymentClient{
		describe: func(_ *provisionerv1.DescribeDeploymentRequest) (*provisionerv1.DescribeDeploymentResponse, error) {
			return &provisionerv1.DescribeDeploymentResponse{Deployment: &provisionerv1.Deployment{
				Id: "x", State: provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING, EngineEndpoint: engine.URL,
			}}, nil
		},
	}, nil)
	srv := httptest.NewServer(serveThroughMux(r))
	defer srv.Close()

	resp, _ := http.Post(srv.URL+"/v1/x/v1/chat/completions", "application/json", strings.NewReader(`{}`))
	resp.Body.Close()

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	routerTraceID := spans[0].SpanContext.TraceID().String()
	engineTID, _ := engineTraceID.Load().(string)
	if engineTID == "" {
		t.Fatal("engine never received traceparent")
	}
	if engineTID != routerTraceID {
		t.Errorf("trace_id mismatch: router span has %q, engine received %q -- chaining broken", routerTraceID, engineTID)
	}
}

// spanAttrFromStub is a wrapper over the SDK's SpanStub.Attributes
// for the in-memory exporter. tracetest returns []sdktrace.SpanStub
// rather than ReadOnlySpan, so the helper has to walk its
// KeyValue slice instead of using ReadOnlySpan.Attributes.
func spanAttrFromStub(stub tracetest.SpanStub, key string) string {
	for _, a := range stub.Attributes {
		if string(a.Key) == key {
			return a.Value.AsString()
		}
	}
	return ""
}

// spanInt64AttrFromStub is the Int64 counterpart of spanAttrFromStub.
// Returns (value, true) on hit; (0, false) if the attribute is
// missing -- distinguishes "absent" from "present-with-value-0",
// which matters for the queue-wait test (direct-forward path leaves
// the attribute absent, NOT zero).
func spanInt64AttrFromStub(stub tracetest.SpanStub, key string) (int64, bool) {
	for _, a := range stub.Attributes {
		if string(a.Key) == key {
			return a.Value.AsInt64(), true
		}
	}
	return 0, false
}
