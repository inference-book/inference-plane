package provisioners

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/metrics"
	"github.com/inference-book/inference-plane/internal/telemetry"
)

// advancingClock returns a clock that steps forward by step on every
// call, so phase/provision durations come out deterministic and > 0.
func advancingClock(step time.Duration) func() time.Time {
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	n := 0
	return func() time.Time {
		t := base.Add(time.Duration(n) * step)
		n++
		return t
	}
}

func obsTestInstance() *provisionerv1.Instance {
	return &provisionerv1.Instance{
		Id:       "dep-1",
		Provider: "runpod",
		Spec: &provisionerv1.Spec{
			Requirements: &provisionerv1.ResourceRequirements{Class: "small"},
		},
	}
}

func configuring(phase string) DeployStateUpdate {
	return DeployStateUpdate{State: provisionerv1.DeploymentState_DEPLOYMENT_STATE_CONFIGURING, Phase: phase}
}

// collectMetrics runs a manual reader and returns the ResourceMetrics.
func collectMetrics(t *testing.T, reader sdkmetric.Reader) metricdata.ResourceMetrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	return rm
}

// histCount returns the number of histogram data points recorded for the
// named instrument, and the total observation count across them.
func histCount(rm metricdata.ResourceMetrics, name string) (points int, observations uint64) {
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			if h, ok := m.Data.(metricdata.Histogram[float64]); ok {
				for _, dp := range h.DataPoints {
					points++
					observations += dp.Count
				}
			}
		}
	}
	return points, observations
}

// sumInt returns the summed value of an int64 counter across data points.
func sumInt(rm metricdata.ResourceMetrics, name string) int64 {
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			if s, ok := m.Data.(metricdata.Sum[int64]); ok {
				for _, dp := range s.DataPoints {
					total += dp.Value
				}
			}
		}
	}
	return total
}

// newTestRecorder installs an in-memory MeterProvider and builds a
// Recorder against it, returning the reader to collect from.
func newTestRecorder(t *testing.T) (*metrics.Recorder, sdkmetric.Reader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	otel.SetMeterProvider(mp)
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	rec, err := metrics.NewRecorder()
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	return rec, reader
}

// TestDeployObserver_RecordsPhaseAndProvisionMetrics drives the observer
// through the cold-start phase ladder and asserts it records one
// phase-duration observation per closed phase plus one end-to-end
// provision observation. This is the payoff of the emit-stream seam:
// the phases the RunPod adapter emits become attributable metrics with
// no telemetry code in the adapter.
func TestDeployObserver_RecordsPhaseAndProvisionMetrics(t *testing.T) {
	rec, reader := newTestRecorder(t)
	s := &Service{recorder: rec, clock: advancingClock(time.Second)}

	obs := s.newDeployObserver(context.Background(), deployKindProvision, "dep-1", obsTestInstance())
	obs.observe(configuring("runpod:scheduling"))
	obs.observe(configuring("runpod:scheduling")) // repeat -> no new phase
	obs.observe(configuring("runpod:image-pull"))
	obs.observe(configuring("engine:init"))
	obs.observe(DeployStateUpdate{State: provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING, Phase: "engine:serving"})
	obs.finish(nil)

	rm := collectMetrics(t, reader)

	// Three phases closed on transition: scheduling, image-pull,
	// engine-init. (engine:serving arrives as the terminal state, which
	// closes engine-init but opens no new phase.)
	if pts, obsc := histCount(rm, telemetry.MetricDeploymentPhaseDuration); obsc != 3 {
		t.Errorf("phase-duration observations = %d (across %d points), want 3", obsc, pts)
	}
	if _, obsc := histCount(rm, telemetry.MetricDeploymentProvisionDuration); obsc != 1 {
		t.Errorf("provision-duration observations = %d, want 1", obsc)
	}
	if got := sumInt(rm, telemetry.MetricDeploymentProvisionsTotal); got != 1 {
		t.Errorf("provisions counter = %d, want 1", got)
	}
}

// TestDeployObserver_TimeoutResult maps a deadline-exceeded return to the
// "timeout" result label -- the dominant cold-start failure operators
// slice the dashboard by.
func TestDeployObserver_TimeoutResult(t *testing.T) {
	if got := deployResult(deployKindProvision, context.DeadlineExceeded); got != resultTimeout {
		t.Errorf("deployResult(deadline) = %q, want %q", got, resultTimeout)
	}
	if got := deployResult(deployKindProvision, errors.New("boom")); got != resultFailed {
		t.Errorf("deployResult(err) = %q, want %q", got, resultFailed)
	}
	if got := deployResult(deployKindProvision, nil); got != resultRunning {
		t.Errorf("deployResult(nil) = %q, want %q", got, resultRunning)
	}
	if got := deployResult(deployKindTeardown, nil); got != resultTerminated {
		t.Errorf("deployResult(teardown,nil) = %q, want %q", got, resultTerminated)
	}
}

// TestDeployObserver_TeardownRecordsTeardownDuration confirms the
// teardown kind routes to the teardown histogram, not the provision one.
func TestDeployObserver_TeardownRecordsTeardownDuration(t *testing.T) {
	rec, reader := newTestRecorder(t)
	s := &Service{recorder: rec, clock: advancingClock(time.Second)}

	obs := s.newDeployObserver(context.Background(), deployKindTeardown, "dep-1", obsTestInstance())
	obs.observe(DeployStateUpdate{State: provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATING, Phase: "runpod:terminate"})
	obs.observe(DeployStateUpdate{State: provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATED, Phase: "runpod:terminate"})
	obs.finish(nil)

	rm := collectMetrics(t, reader)
	if _, obsc := histCount(rm, telemetry.MetricDeploymentTeardownDuration); obsc != 1 {
		t.Errorf("teardown-duration observations = %d, want 1", obsc)
	}
	if _, obsc := histCount(rm, telemetry.MetricDeploymentProvisionDuration); obsc != 0 {
		t.Errorf("provision-duration observations = %d, want 0 for a teardown", obsc)
	}
}
