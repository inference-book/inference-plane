package provisioners_test

import (
	"context"
	"math"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/protobuf/types/known/timestamppb"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/metrics"
	"github.com/inference-book/inference-plane/internal/telemetry"
)

// sumFor totals one metric's float or int data points across every
// series, which is what a cost-per-token query does to a heterogeneous
// deployment before dividing.
func sumFor(rm metricdata.ResourceMetrics, name string) float64 {
	var total float64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			switch d := m.Data.(type) {
			case metricdata.Sum[float64]:
				for _, dp := range d.DataPoints {
					total += dp.Value
				}
			case metricdata.Sum[int64]:
				for _, dp := range d.DataPoints {
					total += float64(dp.Value)
				}
			}
		}
	}
	return total
}

// TestCostPerMillionTokensMatchesHandArithmetic is the acceptance for
// #346, driven through the real state store rather than a stub fleet.
//
// Two hours on a $1.69/hour instance is $3.38. Serving 250k tokens over
// that span is $13.52 per million. The daemon publishes the two series
// the division needs, both labeled instance_id, and this checks the
// quotient rather than trusting each factor separately.
func TestCostPerMillionTokensMatchesHandArithmetic(t *testing.T) {
	rented := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)
	svc := billingSvc(t, &provisionerv1.Instance{
		Id: "i-1", Provider: "runpod",
		Hardware:      &provisionerv1.Hardware{GpuSku: "NVIDIA A100 80GB PCIe"},
		HourlyRateUsd: 1.69,
		CreatedAt:     timestamppb.New(rented),
	})

	reader := sdkmetric.NewManualReader()
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(prev) })

	if _, err := metrics.NewCostRecorder(svc, nil); err != nil {
		t.Fatalf("NewCostRecorder: %v", err)
	}
	rec, err := metrics.NewRecorder()
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	rec.RecordRouterTokens(context.Background(), "dep-1", "qwen", "t-1", "interactive", "i-1", 250_000)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}

	spend := sumFor(rm, telemetry.MetricInstanceCost)
	tokens := sumFor(rm, telemetry.MetricTokensGenerated)
	if tokens == 0 {
		t.Fatal("no tokens series; the division has no denominator")
	}
	if math.Abs(spend-3.38) > 0.01 {
		t.Errorf("spend = %v, want ~3.38", spend)
	}

	perMillion := spend / tokens * 1e6
	if math.Abs(perMillion-13.52) > 0.05 {
		t.Errorf("cost per million tokens = %v, want ~13.52", perMillion)
	}
}

// TestTokensAndCostShareTheJoinKey is the property the division rests
// on. PromQL drops non-matching series rather than erroring, so a
// divergence here would show up as an empty panel and not as a failure.
func TestTokensAndCostShareTheJoinKey(t *testing.T) {
	svc := billingSvc(t, &provisionerv1.Instance{
		Id: "i-1", Provider: "runpod", HourlyRateUsd: 1.69,
		CreatedAt: timestamppb.New(time.Now().Add(-time.Hour)),
	})

	reader := sdkmetric.NewManualReader()
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(prev) })

	if _, err := metrics.NewCostRecorder(svc, nil); err != nil {
		t.Fatalf("NewCostRecorder: %v", err)
	}
	rec, err := metrics.NewRecorder()
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	rec.RecordRouterTokens(context.Background(), "dep-1", "qwen", "t-1", "interactive", "i-1", 10)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}

	idFor := func(name string) string {
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if m.Name != name {
					continue
				}
				switch d := m.Data.(type) {
				case metricdata.Sum[float64]:
					for _, dp := range d.DataPoints {
						v, _ := dp.Attributes.Value(attribute.Key(telemetry.LabelInstanceID))
						return v.AsString()
					}
				case metricdata.Sum[int64]:
					for _, dp := range d.DataPoints {
						v, _ := dp.Attributes.Value(attribute.Key(telemetry.LabelInstanceID))
						return v.AsString()
					}
				}
			}
		}
		return ""
	}

	cost := idFor(telemetry.MetricInstanceCost)
	tokens := idFor(telemetry.MetricTokensGenerated)
	if cost == "" || tokens == "" {
		t.Fatalf("missing instance_id: cost=%q tokens=%q", cost, tokens)
	}
	if cost != tokens {
		t.Errorf("join key differs: cost has %q, tokens has %q", cost, tokens)
	}
}
