package metrics

import (
	"math"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/inference-book/inference-plane/internal/telemetry"
)

func closeTo(got, want, tol float64) bool { return math.Abs(got-want) <= tol }

// TestCostIsSecondsTimesRate checks the arithmetic the whole series
// exists for. An hour on a $1.69 instance is $1.69.
func TestCostIsSecondsTimesRate(t *testing.T) {
	now := time.Now()
	rm := collect(t, fleetStub{instances: []BillingInstance{
		{ID: "i-hour", Provider: "runpod", BillingMode: "metered_per_second", RateUSDPerHour: 1.69, Since: now.Add(-time.Hour)},
		{ID: "i-half", Provider: "vast", BillingMode: "metered_per_second", RateUSDPerHour: 1.30, Since: now.Add(-30 * time.Minute)},
	}})

	got := valuesFor(t, rm, telemetry.MetricInstanceCost)
	if !closeTo(got["i-hour"], 1.69, 0.01) {
		t.Errorf("i-hour cost = %v, want ~1.69", got["i-hour"])
	}
	if !closeTo(got["i-half"], 0.65, 0.01) {
		t.Errorf("i-half cost = %v, want ~0.65", got["i-half"])
	}
}

// TestCostFreezesWhenTheInstanceStops stops the meter at Until. A
// terminated instance holding a climbing figure would overstate every
// bill drawn after teardown.
func TestCostFreezesWhenTheInstanceStops(t *testing.T) {
	now := time.Now()
	rm := collect(t, fleetStub{instances: []BillingInstance{
		{ID: "i-done", RateUSDPerHour: 3.60, Since: now.Add(-2 * time.Hour), Until: now.Add(-time.Hour)},
	}})

	got := valuesFor(t, rm, telemetry.MetricInstanceCost)
	if !closeTo(got["i-done"], 3.60, 0.01) {
		t.Errorf("i-done cost = %v, want ~3.60 (one billed hour, not two)", got["i-done"])
	}
}

// TestUnpricedInstanceIsAbsentRatherThanFree matches the convention the
// rate gauge already holds. A zero would sum into a total that quietly
// understates the bill, and it reads as free rather than unknown.
func TestUnpricedInstanceIsAbsentRatherThanFree(t *testing.T) {
	now := time.Now()
	rm := collect(t, fleetStub{instances: []BillingInstance{
		{ID: "i-priced", RateUSDPerHour: 2.00, Since: now.Add(-time.Hour)},
		{ID: "i-unpriced", RateUSDPerHour: 0, Since: now.Add(-time.Hour)},
	}})

	got := valuesFor(t, rm, telemetry.MetricInstanceCost)
	if _, ok := got["i-unpriced"]; ok {
		t.Errorf("unpriced instance emitted a cost series (%v); want omitted", got["i-unpriced"])
	}
	if _, ok := got["i-priced"]; !ok {
		t.Error("priced instance is missing its cost series")
	}
}

// TestCostCarriesTheSameLabelsAsUptime is what makes the join work. A
// divergence here breaks cost per token silently, since PromQL drops
// non-matching series rather than erroring.
func TestCostCarriesTheSameLabelsAsUptime(t *testing.T) {
	now := time.Now()
	rm := collect(t, fleetStub{instances: []BillingInstance{
		{ID: "i-1", Provider: "runpod", GPUSKU: "NVIDIA A100 80GB PCIe", BillingMode: "metered_per_second", RateUSDPerHour: 1.69, Since: now.Add(-time.Hour)},
	}})

	keys := func(name string) map[attribute.Key]string {
		sets := seriesFor(t, rm, name)
		if len(sets) != 1 {
			t.Fatalf("%s: got %d series, want 1", name, len(sets))
		}
		out := map[attribute.Key]string{}
		for _, kv := range sets[0].ToSlice() {
			out[kv.Key] = kv.Value.AsString()
		}
		return out
	}

	cost := keys(telemetry.MetricInstanceCost)
	uptime := keys(telemetry.MetricInstanceUptimeSeconds)
	if len(cost) != len(uptime) {
		t.Fatalf("cost has %d labels, uptime has %d", len(cost), len(uptime))
	}
	for k, want := range uptime {
		if got, ok := cost[k]; !ok || got != want {
			t.Errorf("cost label %s = %q (present=%v), uptime has %q", k, got, ok, want)
		}
	}
}

// TestCostReportsNothingWithoutAFleet mirrors the uptime case: a
// recorder built without a source reports no instances rather than
// failing the scrape.
func TestCostReportsNothingWithoutAFleet(t *testing.T) {
	rm := collect(t, nil)
	if got := valuesFor(t, rm, telemetry.MetricInstanceCost); len(got) != 0 {
		t.Errorf("got %d cost series with no fleet source, want 0", len(got))
	}
}
