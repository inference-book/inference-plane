// Package router is the v0.2 data-plane primitive: an HTTP handler
// that puts iplane back into the inference request path. The router
// is mounted on `iplane serve`'s HTTP listener and forwards
// OpenAI-shaped requests to the engine endpoint registered for the
// target deployment.
//
// Operator-facing URLs become path-based:
//
//	POST http://<iplane>/v1/<deploy-id>/v1/chat/completions
//	POST http://<iplane>/v1/<deploy-id>/v1/completions
//
// The engine's provider proxy URL becomes an internal implementation
// detail; the iplane URL is the contract.
//
// # CP/DP-1
//
// This package is the first data-plane code in the repo. Per
// CONSTRAINTS.md's CP/DP-1 rule, it reaches control-plane state
// ONLY through the generated gRPC client interface
// (provisionerv1connect.DeploymentServiceClient). No
// internal/provisioners import.
//
// In `iplane serve` the deployment client loopback-dials the daemon's
// own HTTP listener. The localhost round-trip costs ~1ms, which is
// noise on a chat-completion path that takes 100ms+. The benefit:
// when the data plane eventually splits into a separate process
// (per-cluster routers, edge proxies), the same client wiring works
// against a remote control plane with no router-code refactor.
package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"log/slog"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/gen/go/provisioner/v1/provisionerv1connect"
	"github.com/inference-book/inference-plane/internal/metrics"
	"github.com/inference-book/inference-plane/internal/stores/queue"
)

// tracerName is the instrumentation library name attached to every
// span this package emits. Operators filter on this in Tempo to see
// only iplane router spans (vs the v0.1 backend.generate spans, the
// engine's own spans, etc.).
const tracerName = "inference-plane/router"

// Span attribute keys. Hardcoded rather than generated through
// metric-names.yaml because chapter prose does not reference them by
// name in v0.2; promote to the YAML pairing when the chapter starts
// quoting specific attribute names in print.
const (
	AttrRouterMatch    = "iplane.router.match"     // "deploy_id" | "flat"
	AttrRouterDeployID = "iplane.router.deploy_id" // chosen deployment id
	AttrRouterModel    = "iplane.router.model"     // deployment.model
	AttrRouterUpstream = "iplane.router.upstream"  // engine endpoint URL
	AttrRouterStatus   = "iplane.router.status"    // status label string (success | engine_error | ...)
	AttrRouterTenantID = "iplane.router.tenant_id" // operator-asserted tenant; "default" when unannotated
)

// Span name for the router's request-dispatch span. Single name across
// both URL families; the AttrRouterMatch attribute disambiguates. Low
// cardinality on purpose -- per-deploy or per-model cardinality on
// span names blows up Tempo's index without giving operators useful
// filtering they can't get from attributes.
const spanNameDispatch = "iplane.router.dispatch"

// routeMatchDeployID and routeMatchFlat are the values of
// AttrRouterMatch for the two URL families.
const (
	routeMatchDeployID = "deploy_id"
	routeMatchFlat     = "flat"
)

// DescribeTimeout caps the lookup of a deployment's engine endpoint.
// Set generously: the in-daemon DescribeDeployment is a read against
// the in-memory state of record; the only reason it would block for
// any meaningful duration is contention with a state-mutating call,
// which itself completes quickly. 5s gives clear failure surface
// without taping over a real hang.
const DescribeTimeout = 5 * time.Second

// Router forwards OpenAI-compatible HTTP requests to the engine
// endpoint of the named deployment. Construct with New and mount the
// returned *Router as an http.Handler.
//
// The optional *metrics.Recorder is the v0.2 ch7-beat1.5 metrics
// surface: when supplied, each request records duration, status, and
// completion-token counts via the inference.* instrument family.
// nil-safe: tests that don't init telemetry can pass nil and the
// router's emission becomes a no-op.
//
// The tracer and propagator are captured from their globals at
// construction time, matching the canonical OTel Go pattern (mirrors
// what metrics.NewRecorder does for its meter). When no provider is
// set the SDK returns no-op implementations and Start / Inject
// become no-ops.
type Router struct {
	client     provisionerv1connect.DeploymentServiceClient
	recorder   *metrics.Recorder
	tracer     trace.Tracer
	propagator propagation.TextMapPropagator

	// pool is the v0.2 Beat 2 M/M/k waiting room. When nil (the
	// Beat 1 default), incoming requests forward inline -- the
	// existing direct-dispatch behavior. When non-nil, requests
	// submit to the pool's bounded queue; pool servicers Pop and
	// run handleWithObservability. ErrQueueFull surfaces as a
	// 503 + Retry-After at the route level.
	//
	// Construction lives behind WithQueue(servicers, capacity).
	// servicers <= 0 (or the option not supplied) skips Pool
	// construction; that's the Beat 1 path and the recommended
	// default for v0.2 release/v0.2 snapshots until demo 05 lands.
	pool *queue.Pool[*queueEntry]
}

// Option is the functional-option type for New. Existing callers
// using New(client, recorder) keep working; the queue path is opt-in
// via WithQueue.
type Option func(*Router)

// WithQueue activates the v0.2 Beat 2 M/M/k waiting room in front of
// the engine. servicers is k (the number of parallel dispatcher
// goroutines); capacity is the bounded waiting room size N.
//
// Semantics:
//
//   - servicers <= 0 -> no-op; router stays on the direct-forward
//     path (Beat 1 behavior).
//   - capacity <= 0 -> no-op; an unbounded waiting room is not
//     supported in v0.2 (the whole point is bounded backpressure).
//   - servicers > 0 AND capacity > 0 -> Pool is constructed but NOT
//     started. Caller must invoke Router.Start(ctx) to spawn the
//     servicer goroutines.
//
// The (construct, then start) split lets the daemon register the
// router with its mux before the servicers begin processing.
func WithQueue(servicers, capacity int) Option {
	return func(r *Router) {
		if servicers <= 0 || capacity <= 0 {
			return
		}
		r.pool = newPool(servicers, capacity, r.dispatchEntry)
	}
}

// New constructs a Router backed by the supplied DeploymentService
// Connect client. The client is the only seam this package depends on
// from the control plane -- the import graph carries no
// internal/provisioners reference, which is what CP/DP-1 enforces.
//
// In `iplane serve` the client loopback-dials the daemon's own HTTP
// URL; in a future split-plane topology it can dial a remote
// control plane unchanged.
//
// recorder may be nil for tests or daemons that omit telemetry init.
// When set, the router instruments each request via RecordRouterRequest
// and RecordRouterTokens.
//
// opts apply functional options. Today the only option is WithQueue;
// future Beat 2 / Beat 3 wiring (per-tenant routing, replica
// selection) will land as additional options.
func New(client provisionerv1connect.DeploymentServiceClient, recorder *metrics.Recorder, opts ...Option) *Router {
	r := &Router{
		client:     client,
		recorder:   recorder,
		tracer:     otel.Tracer(tracerName),
		propagator: otel.GetTextMapPropagator(),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Start activates the queue path's servicer goroutines if WithQueue
// configured one. parent is the daemon's lifetime context — when it
// cancels, servicers drain in-flight requests and exit.
//
// Safe to call when no pool is configured (Beat 1 path) — Start
// becomes a no-op. Idempotency / re-start guards live on Pool.Start.
func (r *Router) Start(parent context.Context) {
	if r.pool != nil {
		r.pool.Start(parent)
	}
}

// Shutdown stops the pool's servicer goroutines and waits for them
// to drain in-flight items. After Shutdown, the router rejects new
// queued requests with ErrClosed (mapped to 503 + "router shutting
// down" in the queue submit path).
//
// Safe to call when no pool is configured — Shutdown is a no-op.
func (r *Router) Shutdown() {
	if r.pool != nil {
		r.pool.Stop()
	}
}

// serveDeployID handles the explicit-deployment URL family:
//
//	POST /v1/{deploy-id}/v1/chat/completions
//	POST /v1/{deploy-id}/v1/completions
//
// The deploy-id is extracted via Go 1.22's PathValue (ServeMux does
// the extraction before this handler fires). The path's iplane prefix
// is stripped before forwarding so the engine sees the OpenAI tail.
//
// This URL family is the escape hatch for explicit-deployment routing
// (A/B testing, debugging). The default operator-facing URL is the
// flat OpenAI shape served by serveFlat.
func (r *Router) serveDeployID(w http.ResponseWriter, req *http.Request) {
	deployID := req.PathValue("deploy_id")
	if deployID == "" {
		writeOpenAIError(w, http.StatusBadRequest, "deploy_id missing from request path", "invalid_request_error")
		return
	}

	ctx, cancel := context.WithTimeout(req.Context(), DescribeTimeout)
	defer cancel()
	resp, err := r.client.DescribeDeployment(ctx, connect.NewRequest(&provisionerv1.DescribeDeploymentRequest{
		Id: deployID,
	}))
	if err != nil {
		r.handleDescribeError(w, deployID, err)
		return
	}
	dep := resp.Msg.GetDeployment()
	if dep == nil {
		writeOpenAIError(w, http.StatusInternalServerError, "daemon returned nil deployment for "+deployID, "internal_error")
		return
	}

	r.enqueueOrServe(w, req, dep, true /* stripDeployPrefix */)
}

// handleWithObservability is the shared instrumented tail used by
// both serveDeployID and serveFlat. By the time it fires, the
// deployment is resolved, so it can label both metrics and the span
// with deploy_id / model.
//
// Three pieces of instrumentation:
//
//   1. OTel span (v0.2 ch7-beat1.6). Wraps the dispatch with a span
//      named iplane.router.dispatch. The span context flows down to
//      proxyTo, which injects W3C traceparent into the engine
//      request -- engines configured with OTel chain their spans
//      under ours, producing a single trace tree in Tempo.
//
//   2. Metrics (v0.2 ch7-beat1.5). RecordRouterRequest + optional
//      RecordRouterTokens at request close. Recording inside the
//      span's ctx makes the OTel SDK attach trace_id exemplars to
//      the metric observations -- operators can click a slow
//      histogram bucket and jump straight to the trace.
//
//   3. Response wrap (v0.2 ch7-beat1.5). tokenCountingWriter
//      observes bytes flowing through, exposes status code +
//      completion-token count at handler exit.
//
// tenant_id is v0.2 Beat-1 scaffold: emitted as the empty string
// (both span attribute and metric label) until Beat 2 wires
// per-tenant identification through the router.
func (r *Router) handleWithObservability(w http.ResponseWriter, req *http.Request, dep *provisionerv1.Deployment, stripDeployPrefix bool) {
	routeMatch := routeMatchDeployID
	if !stripDeployPrefix {
		routeMatch = routeMatchFlat
	}
	// v0.2 Beat 2.2: tenant resolved by withTenant middleware before
	// the request reaches here. Read it once; thread it into the span
	// attribute and metric labels. Tenant intentionally does NOT
	// flow into OTel baggage -- engines stay tenant-agnostic;
	// correlation across the engine boundary uses trace_id alone.
	tenantID := tenantFromContext(req.Context())
	ctx, span := r.tracer.Start(req.Context(), spanNameDispatch,
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(
			attribute.String(AttrRouterMatch, routeMatch),
			attribute.String(AttrRouterDeployID, dep.GetId()),
			attribute.String(AttrRouterModel, dep.GetModel()),
			attribute.String(AttrRouterUpstream, dep.GetEngineEndpoint()),
			attribute.String(AttrRouterTenantID, tenantID),
		),
	)
	defer span.End()
	req = req.WithContext(ctx)

	start := time.Now()
	tcw := newTokenCountingWriter(w)
	defer func() {
		// Reflect the outcome on the span first so test exporters
		// observing span end see the final attribute set.
		statusLabel := tcw.StatusLabel()
		span.SetAttributes(attribute.String(AttrRouterStatus, statusLabel))
		if tcw.statusCode >= 500 {
			span.SetStatus(codes.Error, http.StatusText(tcw.statusCode))
		} else if tcw.statusCode >= 200 && tcw.statusCode < 400 {
			span.SetStatus(codes.Ok, "")
		}

		r.recorder.RecordRouterRequest(ctx,
			dep.GetId(), dep.GetModel(), tenantID, statusLabel,
			time.Since(start).Seconds())
		if tokens := tcw.CompletionTokens(); tokens > 0 {
			r.recorder.RecordRouterTokens(ctx,
				dep.GetId(), dep.GetModel(), tenantID, tokens)
		}
	}()
	if !r.forwardable(tcw, dep) {
		return
	}
	// v0.2 ch7-beat1.7: mark this deployment as actively serving
	// traffic so the idle-TTL reaper doesn't clean it up while
	// requests are still flowing. Best-effort -- a touch failure
	// is logged but never blocks the proxy or fails the request.
	r.touchActivity(ctx, dep.GetId())
	r.proxyTo(tcw, req, dep, stripDeployPrefix)
}

// touchActivity fires a TouchDeployment RPC against the control
// plane. Synchronous because the Connect roundtrip is a localhost-
// loopback ~1ms hop on top of a 100ms+ chat-completion request --
// the linear-flow code wins over the goroutine bookkeeping.
//
// Best-effort: a touch failure is logged but does NOT propagate to
// the caller. last_activity is leak-protection metadata; missing one
// touch causes at worst a one-tick-late reap.
func (r *Router) touchActivity(ctx context.Context, deployID string) {
	if _, err := r.client.TouchDeployment(ctx, connect.NewRequest(&provisionerv1.TouchDeploymentRequest{
		Id: deployID,
	})); err != nil {
		slog.Default().Warn("router: TouchDeployment failed",
			"deploy_id", deployID, "err", err)
	}
}

// Handle returns the (pattern, handler) pairs the caller mounts on
// its ServeMux. Four patterns covering two URL families:
//
//   - Flat (OpenAI exact): /v1/chat/completions and /v1/completions
//     keyed on the `model` field in the request body. This is the
//     primary operator-facing URL; existing OpenAI SDKs work with
//     `base_url=http://<iplane>/v1` unchanged.
//   - Explicit-deployment: /v1/{deploy-id}/v1/... where the operator
//     wants deterministic dispatch to a specific deployment (A/B
//     testing, debugging). Escape hatch, not the default.
//
// Mounting pattern from the daemon's perspective:
//
//	mux := http.NewServeMux()
//	router := router.New(client)
//	for pattern, h := range router.Handle() {
//	    mux.Handle(pattern, h)
//	}
//
// Patterns use Go 1.22+ method+wildcard syntax. ServeMux extracts
// {deploy_id} into PathValue; handlers do not parse the path
// themselves.
func (r *Router) Handle() map[string]http.Handler {
	// v0.2 Beat 2.2: every route is wrapped in withTenant so the
	// tenant ID is on the request ctx before either dispatch path
	// (direct or queued) runs. The middleware is centralized here
	// rather than at each serve* entry point so a new URL family
	// added later automatically inherits tenant resolution.
	deployIDHandler := withTenant(http.HandlerFunc(r.serveDeployID))
	flatHandler := withTenant(http.HandlerFunc(r.serveFlat))
	return map[string]http.Handler{
		"POST /v1/{deploy_id}/v1/chat/completions": deployIDHandler,
		"POST /v1/{deploy_id}/v1/completions":      deployIDHandler,
		"POST /v1/chat/completions":                flatHandler,
		"POST /v1/completions":                     flatHandler,
	}
}

// handleDescribeError maps the daemon's DescribeDeployment error into
// an HTTP response. NotFound from Connect surfaces as 404; anything
// else is a 502 because the daemon (the upstream we depend on) failed
// in a way the client did not cause.
func (r *Router) handleDescribeError(w http.ResponseWriter, deployID string, err error) {
	var connectErr *connect.Error
	if errors.As(err, &connectErr) && connectErr.Code() == connect.CodeNotFound {
		writeOpenAIError(w, http.StatusNotFound, fmt.Sprintf("deployment %q not found", deployID), "deployment_not_found")
		return
	}
	writeOpenAIError(w, http.StatusBadGateway, fmt.Sprintf("daemon lookup failed for %q: %v", deployID, err), "daemon_error")
}

// forwardable inspects deployment state and writes an appropriate
// error response if the deployment cannot serve traffic right now.
// Returns true if the caller should proceed to proxy.
//
// State mapping (matches the documented contract in CONSTRAINTS.md
// and the chapter-7 narrative):
//
//   - RUNNING with engine_endpoint -> forward
//   - RUNNING without engine_endpoint -> 503 (rare race window)
//   - PENDING / STARTING / CONFIGURING -> 503 + Retry-After
//   - DEGRADED -> 502 (engine unhealthy)
//   - TERMINATING / TERMINATED -> 410 Gone
//   - FAILED -> 502 with failure_reason
//   - UNSPECIFIED -> 503 (defensive; should never happen)
func (r *Router) forwardable(w http.ResponseWriter, dep *provisionerv1.Deployment) bool {
	switch dep.GetState() {
	case provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING:
		if dep.GetEngineEndpoint() == "" {
			writeOpenAIError(w, http.StatusServiceUnavailable, "deployment is running but engine endpoint not yet stamped; retry shortly", "deployment_not_ready")
			w.Header().Set("Retry-After", "2")
			return false
		}
		return true
	case provisionerv1.DeploymentState_DEPLOYMENT_STATE_PENDING,
		provisionerv1.DeploymentState_DEPLOYMENT_STATE_STARTING,
		provisionerv1.DeploymentState_DEPLOYMENT_STATE_CONFIGURING:
		w.Header().Set("Retry-After", "5")
		writeOpenAIError(w, http.StatusServiceUnavailable, fmt.Sprintf("deployment %q is %s; retry shortly", dep.GetId(), stateLabel(dep.GetState())), "deployment_not_ready")
		return false
	case provisionerv1.DeploymentState_DEPLOYMENT_STATE_DEGRADED:
		writeOpenAIError(w, http.StatusBadGateway, fmt.Sprintf("deployment %q engine is unhealthy", dep.GetId()), "engine_unhealthy")
		return false
	case provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATING,
		provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATED:
		writeOpenAIError(w, http.StatusGone, fmt.Sprintf("deployment %q is %s", dep.GetId(), stateLabel(dep.GetState())), "deployment_gone")
		return false
	case provisionerv1.DeploymentState_DEPLOYMENT_STATE_FAILED:
		reason := dep.GetFailureReason()
		if reason == "" {
			reason = "deployment failed (no reason recorded)"
		}
		writeOpenAIError(w, http.StatusBadGateway, fmt.Sprintf("deployment %q failed: %s", dep.GetId(), reason), "deployment_failed")
		return false
	default:
		writeOpenAIError(w, http.StatusServiceUnavailable, fmt.Sprintf("deployment %q has unknown state %v", dep.GetId(), dep.GetState()), "deployment_unknown_state")
		return false
	}
}

// proxyTo reverse-proxies the inbound request to the deployment's
// engine endpoint. stripDeployPrefix tells the proxy whether to
// remove the /v1/<deploy-id>/ prefix before forwarding:
//
//   - serveDeployID passes true: the inbound path is
//     /v1/<deploy-id>/v1/chat/completions and the engine wants only
//     /v1/chat/completions.
//   - serveFlat passes false: the inbound path is already
//     /v1/chat/completions (no iplane-side prefix), forward as-is.
//
// SSE streaming (v0.2 ch7-beat1.4): non-streaming and streaming
// (`stream: true`) responses both flow through this same path with
// no special-casing in router code. httputil.ReverseProxy detects
// `Content-Type: text/event-stream` on the engine's response and
// auto-flushes after each write -- the client sees tokens in
// real-time as the engine emits them. Client disconnect propagates
// to upstream via context cancellation (also default ReverseProxy
// behavior), so killing a chat REPL mid-stream terminates the
// engine's compute rather than leaking it. Both properties are
// asserted in stream_test.go.
func (r *Router) proxyTo(w http.ResponseWriter, req *http.Request, dep *provisionerv1.Deployment, stripDeployPrefix bool) {
	target, err := url.Parse(dep.GetEngineEndpoint())
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, fmt.Sprintf("deployment %q has malformed engine endpoint %q: %v", dep.GetId(), dep.GetEngineEndpoint(), err), "engine_unreachable")
		return
	}

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			if stripDeployPrefix {
				pr.Out.URL.Path = openAITail(pr.In.URL.Path)
			}
			pr.Out.URL.RawPath = ""
			pr.Out.Host = target.Host
			// v0.2 ch7-beat2.2: strip the iplane-internal tenant
			// header. Engines stay tenant-agnostic -- the operator
			// asserted the tenant for iplane's queueing /
			// metrics / (Part V) auth, but vLLM has no business
			// branching on it. Correlation across iplane→engine
			// goes through OTel trace_id (injected below), not via
			// replicating tenant on every layer.
			pr.Out.Header.Del(TenantHeader)
			// v0.2 ch7-beat1.6: inject W3C traceparent + baggage on
			// the outbound request. Engines running with OTel SDK
			// configured (Ch 6 phase 3 plumbs OTEL_EXPORTER_OTLP_*
			// onto the pod) pick this up and chain their spans
			// under ours, producing a single trace tree in Tempo.
			// Engines without OTel just ignore the header.
			r.propagator.Inject(
				pr.In.Context(),
				propagation.HeaderCarrier(pr.Out.Header),
			)
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			writeOpenAIError(w, http.StatusBadGateway, fmt.Sprintf("upstream engine call failed: %v", err), "engine_unreachable")
		},
	}
	proxy.ServeHTTP(w, req)
}

// openAITail strips the /v1/<deploy-id>/ prefix and returns the
// OpenAI-shaped tail (e.g. "/v1/chat/completions"). Operates on the
// path string directly rather than recomputing from the matched
// pattern because httputil's request has already been mutated by
// Director composition.
func openAITail(p string) string {
	// Path is guaranteed to match /v1/{deploy_id}/v1/<rest> by the
	// ServeMux registration in Handle(). Strip the first two
	// segments after the leading slash.
	if len(p) < 4 || p[:4] != "/v1/" {
		return p
	}
	// Skip past "/v1/"
	rest := p[4:]
	// Skip past deploy-id (next segment up to '/')
	for i := 0; i < len(rest); i++ {
		if rest[i] == '/' {
			return rest[i:]
		}
	}
	return p
}

// stateLabel strips the DEPLOYMENT_STATE_ prefix so error messages
// read naturally (e.g. "deployment foo is STARTING").
func stateLabel(s provisionerv1.DeploymentState) string {
	const prefix = "DEPLOYMENT_STATE_"
	name := s.String()
	if len(name) > len(prefix) && name[:len(prefix)] == prefix {
		return name[len(prefix):]
	}
	return name
}

// openAIError matches the OpenAI error envelope ({"error": {...}}).
// SDKs surface error.message and error.type as the operator-visible
// failure; matching their shape means existing client libraries
// don't need to learn iplane-specific error parsing.
type openAIError struct {
	Error openAIErrorBody `json:"error"`
}

type openAIErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

func writeOpenAIError(w http.ResponseWriter, status int, msg, errType string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(openAIError{
		Error: openAIErrorBody{Message: msg, Type: errType},
	})
}
