package provisioners

import (
	"context"
	"errors"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otelcodes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/metrics"
)

// deployTracer is the control-plane tracer for the provision/teardown
// lifecycle. Grabbed from the global provider (the same convention the
// services and router packages use) rather than injected, so it works
// whether or not telemetry.Init has run -- before Init it's a no-op
// tracer and the spans go nowhere.
var deployTracer = otel.Tracer("inference-plane/provisioners")

// Span/attribute names for the deployment lifecycle. Kept as inline
// consts (mirroring the router's local span-attr consts) because these
// are span-only keys, not metric label names -- the metric-names.yaml
// vocabulary governs the histogram/label names the dashboards read.
const (
	spanDeployProvision = "deployment.provision"
	spanDeployTeardown  = "deployment.teardown"

	attrDeployID = "iplane.deploy_id"
	attrProvider = "iplane.provider"
	attrClass    = "iplane.class"
	attrPhase    = "iplane.phase"
	attrResult   = "iplane.result"
)

// deployKind selects which overall instrument finish() records into.
type deployKind int

const (
	deployKindProvision deployKind = iota
	deployKindTeardown
)

// deployObserver turns an executor's emit phase stream into OTel spans
// and duration metrics, without the provider adapters knowing anything
// about telemetry. The Service wraps the emit closure it already passes
// to Deploy/Destroy; every DeployStateUpdate flows through observe(),
// and finish() closes out the root span + records the end-to-end
// duration once the executor returns.
//
// Design: the phase strings the adapters emit (e.g. "runpod:image-pull",
// "engine:init") ARE the instrumentation seam. Deriving spans + phase
// durations here keeps the CP/DP-1 boundary intact -- data-plane and
// provider code stay free of OTel imports -- and gives every provider
// the same lifecycle observability for free the moment it emits phases.
//
// Not safe for concurrent use: a single deploy/destroy emits serially
// from one goroutine, so no locking is needed. Each replica in a
// fan-out gets its own observer.
type deployObserver struct {
	rec      *metrics.Recorder
	kind     deployKind
	provider string
	class    string
	clock    func() time.Time

	rootCtx  context.Context
	rootSpan trace.Span
	started  time.Time

	// Current open phase, if any. curPhase == "" means no phase is
	// open (either nothing emitted yet, or the last update was
	// terminal and closed the phase).
	curPhase     string
	phaseStarted time.Time
	phaseSpan    trace.Span

	terminalSeen bool
}

// newDeployObserver starts the root span and returns an observer whose
// rootCtx should be handed to the executor so anything it traces nests
// under the lifecycle span. kind picks the end-to-end instrument.
func (s *Service) newDeployObserver(ctx context.Context, kind deployKind, deployID string, inst *provisionerv1.Instance) *deployObserver {
	provider := inst.GetProvider()
	class := inst.GetSpec().GetRequirements().GetClass()

	spanName := spanDeployProvision
	if kind == deployKindTeardown {
		spanName = spanDeployTeardown
	}
	rootCtx, span := deployTracer.Start(ctx, spanName,
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			attribute.String(attrDeployID, deployID),
			attribute.String(attrProvider, provider),
			attribute.String(attrClass, class),
		),
	)
	return &deployObserver{
		rec:      s.recorder,
		kind:     kind,
		provider: provider,
		class:    class,
		clock:    s.clock,
		rootCtx:  rootCtx,
		rootSpan: span,
		started:  s.clock(),
	}
}

// ctx returns the context carrying the root span. Executors run under
// it so cancellation/deadline propagate and any future adapter spans
// nest correctly.
func (o *deployObserver) ctx() context.Context {
	if o == nil {
		return context.Background()
	}
	return o.rootCtx
}

// observe records one DeployStateUpdate. A phase-name change closes the
// prior phase (recording its duration + ending its span) and opens the
// new one. A terminal state closes the current phase and stops opening
// new ones -- finish() then only has the end-to-end duration left.
func (o *deployObserver) observe(u DeployStateUpdate) {
	if o == nil {
		return
	}
	now := o.clock()
	if isTerminalDeployState(u.State) {
		o.closePhase(now)
		o.terminalSeen = true
		return
	}
	if u.Phase == "" || u.Phase == o.curPhase {
		return
	}
	o.closePhase(now)
	o.openPhase(u.Phase, now)
}

// finish closes any still-open phase and records the end-to-end
// provision/teardown duration + outcome, then ends the root span. err
// is the executor's return value; the derived result label is one of
// "running"/"terminated" (success), "timeout", or "failed".
func (o *deployObserver) finish(err error) {
	if o == nil {
		return
	}
	now := o.clock()
	o.closePhase(now)
	result := deployResult(o.kind, err)
	total := now.Sub(o.started).Seconds()

	switch o.kind {
	case deployKindTeardown:
		o.rec.RecordDeployTeardown(o.rootCtx, o.provider, result, total)
	default:
		o.rec.RecordDeployProvision(o.rootCtx, o.provider, result, o.class, total)
	}

	o.rootSpan.SetAttributes(attribute.String(attrResult, result))
	if err != nil {
		o.rootSpan.RecordError(err)
		o.rootSpan.SetStatus(otelcodes.Error, result)
	}
	o.rootSpan.End()
}

// openPhase starts a phase span + timer. The child span is named by the
// phase string so a Tempo waterfall reads "runpod:image-pull",
// "engine:init", etc. directly.
func (o *deployObserver) openPhase(phase string, at time.Time) {
	_, span := deployTracer.Start(o.rootCtx, phase,
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(attribute.String(attrPhase, phase)),
	)
	o.curPhase = phase
	o.phaseStarted = at
	o.phaseSpan = span
}

// closePhase records the current phase's duration + ends its span. The
// phase-duration label set carries the eventual outcome so a dashboard
// can split "image-pull seconds on deploys that succeeded" from "on
// deploys that timed out." No-op when no phase is open.
func (o *deployObserver) closePhase(at time.Time) {
	if o.curPhase == "" {
		return
	}
	sec := at.Sub(o.phaseStarted).Seconds()
	// result is not final until finish(); label the phase with the
	// running outcome so far. The overwhelmingly common query is
	// "phase p95 regardless of result", which sums over the label.
	o.rec.RecordDeployPhase(o.rootCtx, o.curPhase, o.provider, resultInProgress, sec)
	if o.phaseSpan != nil {
		o.phaseSpan.End()
	}
	o.curPhase = ""
	o.phaseSpan = nil
}

// Result label values. resultInProgress tags phase durations (the
// terminal outcome isn't known until the executor returns); the
// terminal values tag the end-to-end provision/teardown record.
const (
	resultInProgress = "in_progress"
	resultRunning    = "running"
	resultTerminated = "terminated"
	resultTimeout    = "timeout"
	resultFailed     = "failed"
)

// deployResult maps the executor's return error to a terminal result
// label. A deadline-exceeded error is the cold-start timeout (the
// dominant failure mode operators care about); everything else with an
// error is "failed"; nil is success.
func deployResult(kind deployKind, err error) string {
	if err == nil {
		if kind == deployKindTeardown {
			return resultTerminated
		}
		return resultRunning
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return resultTimeout
	}
	return resultFailed
}

// isTerminalDeployState reports whether a state ends the lifecycle. The
// in-flight states (STARTING/CONFIGURING/TERMINATING) keep phases open;
// RUNNING/DEGRADED/FAILED/TERMINATED close them out.
func isTerminalDeployState(s provisionerv1.DeploymentState) bool {
	switch s {
	case provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
		provisionerv1.DeploymentState_DEPLOYMENT_STATE_DEGRADED,
		provisionerv1.DeploymentState_DEPLOYMENT_STATE_FAILED,
		provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATED:
		return true
	}
	return false
}
