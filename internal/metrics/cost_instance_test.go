package metrics

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/inference-book/inference-plane/internal/telemetry"
)

// fleetStub is a fixed answer to "what is billing right now".
type fleetStub struct{ instances []BillingInstance }

func (f fleetStub) BillingInstances() []BillingInstance { return f.instances }

// collect installs a manual reader, builds a recorder over the given
// fleet, and returns the metrics one scrape produces. The SDK drives
// the observable callbacks, which is the only way to exercise them.
func collect(t *testing.T, fleet FleetSource) metricdata.ResourceMetrics {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(prev) })

	if _, err := NewCostRecorder(fleet, nil); err != nil {
		t.Fatalf("NewCostRecorder: %v", err)
	}
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	return rm
}

// seriesFor returns the data points of one metric by name.
func seriesFor(t *testing.T, rm metricdata.ResourceMetrics, name string) []attribute.Set {
	t.Helper()
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
			case metricdata.Gauge[float64]:
				for _, dp := range d.DataPoints {
					out = append(out, dp.Attributes)
				}
			}
		}
	}
	return out
}

func valuesFor(t *testing.T, rm metricdata.ResourceMetrics, name string) map[string]float64 {
	t.Helper()
	out := map[string]float64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, dp := range d.DataPoints {
					id, _ := dp.Attributes.Value(attribute.Key(telemetry.LabelInstanceID))
					out[id.AsString()] = float64(dp.Value)
				}
			case metricdata.Sum[float64]:
				for _, dp := range d.DataPoints {
					id, _ := dp.Attributes.Value(attribute.Key(telemetry.LabelInstanceID))
					out[id.AsString()] = dp.Value
				}
			case metricdata.Gauge[float64]:
				for _, dp := range d.DataPoints {
					id, _ := dp.Attributes.Value(attribute.Key(telemetry.LabelInstanceID))
					out[id.AsString()] = dp.Value
				}
			}
		}
	}
	return out
}

func TestUptimeIsOneSeriesPerInstanceWithItsOwnLabels(t *testing.T) {
	// The whole point. One global tuple made a heterogeneous fleet
	// unreportable: two replicas of one deployment on two providers
	// shared whatever the operator's shell had asserted, so the cost of
	// each was indistinguishable from the other (#163).
	now := time.Now()
	rm := collect(t, fleetStub{instances: []BillingInstance{
		{ID: "i-runpod", Provider: "runpod", GPUSKU: "NVIDIA A100 80GB PCIe", BillingMode: "metered_per_second", RateUSDPerHour: 1.69, Since: now.Add(-time.Hour)},
		{ID: "i-vast", Provider: "vast", GPUSKU: "A100_SXM4", BillingMode: "metered_per_second", RateUSDPerHour: 1.30, Since: now.Add(-30 * time.Minute)},
	}})

	sets := seriesFor(t, rm, telemetry.MetricInstanceUptimeSeconds)
	if len(sets) != 2 {
		t.Fatalf("got %d uptime series, want one per instance", len(sets))
	}
	seen := map[string]string{}
	for _, s := range sets {
		id, _ := s.Value(attribute.Key(telemetry.LabelInstanceID))
		prov, _ := s.Value(attribute.Key(telemetry.LabelProvider))
		seen[id.AsString()] = prov.AsString()
	}
	if seen["i-runpod"] != "runpod" || seen["i-vast"] != "vast" {
		t.Errorf("series carry the wrong providers: %+v", seen)
	}
}

func TestUptimeIsMeasuredFromTheInstanceNotTheRecorder(t *testing.T) {
	// The second defect the epic named and the issue body did not: the
	// counter reported seconds since NewCostRecorder, so for a
	// long-lived daemon and a pool rented an hour ago the billed-time
	// base was simply the wrong clock.
	rm := collect(t, fleetStub{instances: []BillingInstance{
		{ID: "i-1", Provider: "runpod", Since: time.Now().Add(-2 * time.Hour)},
	}})

	got := valuesFor(t, rm, telemetry.MetricInstanceUptimeSeconds)["i-1"]
	if got < 7100 || got > 7300 {
		t.Errorf("uptime = %.0fs, want about 7200 (two hours since the instance activated, not since this recorder was built)", got)
	}
}

func TestATerminatedInstanceStopsAccruing(t *testing.T) {
	// A record that stops billing has to hold its final figure. A
	// counter that kept climbing after teardown would make every
	// finished deployment look more expensive the longer ago it ran.
	now := time.Now()
	rm := collect(t, fleetStub{instances: []BillingInstance{
		{ID: "i-done", Provider: "vast", Since: now.Add(-3 * time.Hour), Until: now.Add(-2 * time.Hour)},
	}})

	got := valuesFor(t, rm, telemetry.MetricInstanceUptimeSeconds)["i-done"]
	if got < 3500 || got > 3700 {
		t.Errorf("uptime = %.0fs, want about 3600 (it ran for an hour and then stopped)", got)
	}
}

func TestAnInstanceThatNeverStartedBillingReportsNothing(t *testing.T) {
	// Provisioning latency is not billed, so a record with no start is
	// not a zero-second rental, it is a rental that has not begun.
	rm := collect(t, fleetStub{instances: []BillingInstance{
		{ID: "i-pending", Provider: "runpod", RateUSDPerHour: 1.69},
	}})

	if got := valuesFor(t, rm, telemetry.MetricInstanceUptimeSeconds)["i-pending"]; got != 0 {
		t.Errorf("a pending instance reported %v seconds of billing", got)
	}
}

func TestTheRateGaugePricesEachInstanceFromItsOwnQuote(t *testing.T) {
	// The rate the provider gave us at spawn, per instance, so spend is
	// a join rather than a guess against a catalog whose gpu_type
	// vocabulary matches no adapter's SKU string.
	rm := collect(t, fleetStub{instances: []BillingInstance{
		{ID: "i-1", Provider: "runpod", RateUSDPerHour: 3.60, Since: time.Now()},
	}})

	got := valuesFor(t, rm, telemetry.MetricInstanceRate)["i-1"]
	if got < 0.00099 || got > 0.00101 {
		t.Errorf("rate = %v USD/s, want 0.001 (3.60/hour)", got)
	}
}

func TestAnInstanceWithNoQuotedRateIsOmittedRatherThanPricedAtZero(t *testing.T) {
	// Zero and unknown are different claims, and a zero sums into a
	// total that understates the bill without saying so. The instance
	// still reports uptime, because how long it ran is known even when
	// its price is not.
	rm := collect(t, fleetStub{instances: []BillingInstance{
		{ID: "i-unpriced", Provider: "runpod", Since: time.Now().Add(-time.Hour)},
	}})

	if _, priced := valuesFor(t, rm, telemetry.MetricInstanceRate)["i-unpriced"]; priced {
		t.Error("an instance whose provider quoted no rate was published at zero")
	}
	if got := valuesFor(t, rm, telemetry.MetricInstanceUptimeSeconds)["i-unpriced"]; got <= 0 {
		t.Error("dropping the rate also dropped the uptime; how long it ran is still known")
	}
}

func TestUptimeAndRateShareLabelsSoTheyCanBeJoined(t *testing.T) {
	// Spend is uptime multiplied by rate on instance_id. That only
	// works while both sides carry the same keys, so this is the
	// property the whole design rests on.
	rm := collect(t, fleetStub{instances: []BillingInstance{
		{ID: "i-1", Provider: "runpod", GPUSKU: "NVIDIA L4", BillingMode: "metered_per_second", RateUSDPerHour: 0.44, Since: time.Now()},
	}})

	uptime := seriesFor(t, rm, telemetry.MetricInstanceUptimeSeconds)
	rate := seriesFor(t, rm, telemetry.MetricInstanceRate)
	if len(uptime) != 1 || len(rate) != 1 {
		t.Fatalf("want one series each, got %d uptime and %d rate", len(uptime), len(rate))
	}
	if !uptime[0].Equals(&rate[0]) {
		t.Errorf("label sets differ, so the join fails:\n uptime %v\n rate   %v", uptime[0].Encoded(attribute.DefaultEncoder()), rate[0].Encoded(attribute.DefaultEncoder()))
	}
}

func TestARecorderWithNoFleetSourceReportsNoInstances(t *testing.T) {
	// A daemon can be built without one, and a callback that panicked
	// on nil would take the whole scrape down with it.
	rm := collect(t, nil)
	if got := seriesFor(t, rm, telemetry.MetricInstanceUptimeSeconds); len(got) != 0 {
		t.Errorf("got %d series with no fleet source", len(got))
	}
}
