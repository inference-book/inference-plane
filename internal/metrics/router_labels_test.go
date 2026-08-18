package metrics

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/inference-book/inference-plane/internal/telemetry"
)

// collectRouter installs a manual reader, builds a Recorder over it,
// lets the caller emit, and returns what one scrape produced.
func collectRouter(t *testing.T, emit func(*Recorder)) metricdata.ResourceMetrics {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(prev) })

	r, err := NewRecorder()
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	emit(r)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	return rm
}

// attrSetsFor returns every data point's attribute set for one metric,
// across whichever aggregation the instrument happens to use.
func attrSetsFor(rm metricdata.ResourceMetrics, name string) []attribute.Set {
	var out []attribute.Set
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, dp := range d.DataPoints {
					out = append(out, dp.Attributes)
				}
			case metricdata.Sum[float64]:
				for _, dp := range d.DataPoints {
					out = append(out, dp.Attributes)
				}
			case metricdata.Gauge[int64]:
				for _, dp := range d.DataPoints {
					out = append(out, dp.Attributes)
				}
			case metricdata.Gauge[float64]:
				for _, dp := range d.DataPoints {
					out = append(out, dp.Attributes)
				}
			case metricdata.Histogram[float64]:
				for _, dp := range d.DataPoints {
					out = append(out, dp.Attributes)
				}
			}
		}
	}
	return out
}

// TestRouterSeriesCarryInstanceIDNotReplicaID pins the join key. The
// router labelled the replica it served from as replica_id while the
// cost series labelled the same value as instance_id, so cost per token
// could only be expressed with a label_replace bridging two spellings
// of one id.
func TestRouterSeriesCarryInstanceIDNotReplicaID(t *testing.T) {
	rm := collectRouter(t, func(r *Recorder) {
		r.RecordRouterRequest(context.Background(), "dep-1", "qwen", "t-1", "interactive", "i-1", "success", 0.4)
		r.RecordRouterTokens(context.Background(), "dep-1", "qwen", "t-1", "interactive", "i-1", "8k", 128)
		r.RecordRouterDecision(context.Background(), "dep-1", "i-1", "picked")
		r.RecordReplicaInFlight(context.Background(), "dep-1", "i-1", 3)
	})

	for _, name := range []string{
		telemetry.MetricRequestsTotal,
		telemetry.MetricRequestDuration,
		telemetry.MetricTokensGenerated,
		telemetry.MetricRouterDecisionsTotal,
		telemetry.MetricReplicaInFlight,
	} {
		sets := attrSetsFor(rm, name)
		if len(sets) == 0 {
			t.Fatalf("%s: no series emitted", name)
		}
		for _, s := range sets {
			if _, ok := s.Value(attribute.Key("replica_id")); ok {
				t.Errorf("%s: still carries replica_id", name)
			}
			id, ok := s.Value(attribute.Key(telemetry.LabelInstanceID))
			if !ok {
				t.Errorf("%s: missing %s", name, telemetry.LabelInstanceID)
				continue
			}
			if id.AsString() != "i-1" {
				t.Errorf("%s: %s = %q, want %q", name, telemetry.LabelInstanceID, id.AsString(), "i-1")
			}
		}
	}
}
