package metrics

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"gopkg.in/yaml.v3"

	"github.com/inference-book/inference-plane/internal/telemetry"
)

// BillingInstance is one rented thing, reduced to what a cost figure
// needs.
//
// Since is when the provider said it was active and the meter started,
// which is not when the record was created: a record exists while the
// provider is still deciding, and that time is not billed. Until is set
// only once it stops, so a zero Until means still running rather than
// ran for no time.
//
// A zero RateUSDPerHour means the provider reported no price, which is
// unknown and not free. The rate gauge omits that instance rather than
// publishing a zero somebody would sum into a total.
type BillingInstance struct {
	ID             string
	Provider       string
	GPUSKU         string
	BillingMode    string
	RateUSDPerHour float64
	Since          time.Time
	Until          time.Time
}

// FleetSource reports the instances currently billing.
//
// Narrow on purpose. The cost recorder needs identity and price, and
// giving a metrics package the state store would invert the dependency
// the rest of the daemon is built on. The control plane implements this
// over its own records; nothing else here knows what a provider is.
type FleetSource interface {
	BillingInstances() []BillingInstance
}

// Provider is one row from providers.yaml after rate normalization.
// EffectiveRateUSDPerSec is the per-second cost computed from the
// billing-mode-specific fields (hourly / monthly / capex+power).
type Provider struct {
	Name                   string
	BillingMode            string
	GPUType                string
	EffectiveRateUSDPerSec float64
}

// CostRecorder holds the cost-related instruments described in the
// chapter's cost-tracking subsection. Four signals:
//
//   - instance.uptime.seconds.total       (observable Int64Counter)
//     billed seconds per rented instance, measured from when the
//     provider started charging.
//
//   - instance.cost.usd.total             (observable Float64Counter)
//     what those seconds cost at the rate quoted at spawn. Divided by
//     the token counter on instance_id it gives cost per token, which
//     is the figure Part IV's economic argument rests on.
//
//   - inference.active.seconds.total      (sync Float64Counter)
//     time actually serving inference. Combined with uptime gives
//     utilization.
//
//   - gpu.effective_rate.usd_per_second   (observable Float64Gauge)
//     one observation per provider/gpu_type/billing_mode loaded from
//     providers.yaml. Powers the cross-provider snapshot panel:
//     the panel multiplies observed active-seconds against each
//     provider's rate to project monthly cost on each provider.
type CostRecorder struct {
	fleet         FleetSource
	providers     []Provider
	activeSeconds metric.Float64Counter
}

// NewCostRecorder constructs the cost instruments. The observable
// callbacks (uptime, rates) capture this CostRecorder by closure --
// each scrape interval the SDK invokes them and they emit their
// current observations.
func NewCostRecorder(fleet FleetSource, providers []Provider) (*CostRecorder, error) {
	meter := otel.Meter("inference-plane/cost")
	cr := &CostRecorder{
		fleet:     fleet,
		providers: providers,
	}

	if _, err := meter.Int64ObservableCounter(
		telemetry.MetricInstanceUptimeSeconds,
		metric.WithUnit("s"),
		metric.WithDescription("Billed seconds per rented instance, measured from when the provider said it was active."),
		metric.WithInt64Callback(cr.observeUptime),
	); err != nil {
		return nil, fmt.Errorf("cost: uptime counter: %w", err)
	}

	active, err := meter.Float64Counter(
		telemetry.MetricInferenceActiveSeconds,
		metric.WithUnit("s"),
		metric.WithDescription("Seconds spent actively serving inference. Combined with uptime gives utilization."),
	)
	if err != nil {
		return nil, fmt.Errorf("cost: active-seconds counter: %w", err)
	}
	cr.activeSeconds = active

	if _, err := meter.Float64ObservableGauge(
		telemetry.MetricInstanceRate,
		metric.WithUnit("USD/s"),
		metric.WithDescription("Per-second price of one rented instance, as the provider quoted it at spawn. Joins the uptime counter on instance_id to give spend."),
		metric.WithFloat64Callback(cr.observeInstanceRates),
	); err != nil {
		return nil, fmt.Errorf("cost: instance rate gauge: %w", err)
	}

	if _, err := meter.Float64ObservableCounter(
		telemetry.MetricInstanceCost,
		metric.WithUnit("USD"),
		metric.WithDescription("Dollars spent per rented instance, being the billed seconds times the rate quoted at spawn. Divided by the token counter on instance_id it gives cost per token."),
		metric.WithFloat64Callback(cr.observeInstanceCost),
	); err != nil {
		return nil, fmt.Errorf("cost: instance cost counter: %w", err)
	}

	if _, err := meter.Float64ObservableGauge(
		telemetry.MetricGPUEffectiveRate,
		metric.WithUnit("USD/s"),
		metric.WithDescription("Per-second cost rate per provider/gpu_type/billing_mode (loaded from providers.yaml). Powers the cross-provider snapshot panel."),
		metric.WithFloat64Callback(cr.observeRates),
	); err != nil {
		return nil, fmt.Errorf("cost: rate gauge: %w", err)
	}

	return cr, nil
}

// RecordActive adds elapsed inference time to the active-seconds
// counter, attributed to the instance that served it. Counted on
// failure too, because the card burned the time either way.
//
// Labelled with the instance id and nothing about the instance. The
// caller is the router, which holds the id for free and would have to
// fetch anything else through the control plane on a path where a
// synchronous hop per request has already cost a 25s p95 once. What
// the instance is gets attached by the uptime and rate series, which
// carry the same id and are emitted where that knowledge lives.
//
// An empty instance id is tolerated rather than dropped: legacy
// single-instance deployments carry none, and losing their inference
// time would understate utilization on exactly the oldest deployments.
func (cr *CostRecorder) RecordActive(ctx context.Context, model, deploymentID, instanceID string, durationSec float64) {
	if cr == nil || durationSec <= 0 {
		return
	}
	cr.activeSeconds.Add(ctx, durationSec, metric.WithAttributes(
		attribute.String(telemetry.LabelModel, model),
		attribute.String(telemetry.LabelDeployID, deploymentID),
		attribute.String(telemetry.LabelInstanceID, instanceID),
	))
}

// observeUptime emits one series per instance that is or was billing,
// measured from when the provider said it was active.
//
// This used to report seconds since the recorder was constructed, under
// one label set asserted by the operator's shell at daemon startup. For
// a v0.1 control plane managing a single deployment that described
// something real. It has described nothing since: one daemon runs many
// deployments, a heterogeneous fleet spans providers within a single
// deployment, and the control plane is not itself a workload (#163).
func (cr *CostRecorder) observeUptime(_ context.Context, observer metric.Int64Observer) error {
	for _, inst := range cr.billing() {
		observer.Observe(int64(inst.billedSeconds(time.Now())), metric.WithAttributes(instanceAttrs(inst)...))
	}
	return nil
}

// observeInstanceRates emits what each live instance costs per second.
//
// Separate from observeRates, which prices the catalog. This one prices
// the fleet, and the two answer different questions: what could we rent,
// versus what are we renting. Multiplying this by the uptime series on
// instance_id is how a deployment's spend is derived, which is the
// figure a cost argument is made from.
//
// An instance whose provider reported no rate is omitted. Emitting zero
// would be indistinguishable from free, and it would sum into a total
// that quietly understates the bill.
func (cr *CostRecorder) observeInstanceRates(_ context.Context, observer metric.Float64Observer) error {
	for _, inst := range cr.billing() {
		if inst.RateUSDPerHour <= 0 {
			continue
		}
		observer.Observe(inst.RateUSDPerHour/secondsPerHour, metric.WithAttributes(instanceAttrs(inst)...))
	}
	return nil
}

// observeInstanceCost emits what each instance has cost so far.
//
// The two series either side of this one carry the factors and left the
// multiplication to whoever was reading. That worked while a cost figure
// meant one number at the end of a run. Part IV needs cost per token as
// concurrency rises, which is this divided by the token counter over a
// window, and a join of two series through a third is not something a
// panel query should have to express (#346).
//
// Computed at scrape time from the current quoted rate, which is exact
// only because the rate is fixed when the instance is rented. A provider
// whose price moved mid-rental would have every earlier second repriced
// retroactively here. No rental is reclaimable or spot-priced today
// (#333), so revisit this the day one is, and not before.
//
// Unpriced instances are omitted for the same reason observeInstanceRates
// omits them. Zero is a bill, and unknown is not.
func (cr *CostRecorder) observeInstanceCost(_ context.Context, observer metric.Float64Observer) error {
	now := time.Now()
	for _, inst := range cr.billing() {
		if inst.RateUSDPerHour <= 0 {
			continue
		}
		spend := inst.billedSeconds(now) * inst.RateUSDPerHour / secondsPerHour
		observer.Observe(spend, metric.WithAttributes(instanceAttrs(inst)...))
	}
	return nil
}

// billing asks the fleet source what is running, tolerating its absence
// so a recorder built without one (tests, a daemon with no state) simply
// reports no instances.
func (cr *CostRecorder) billing() []BillingInstance {
	if cr.fleet == nil {
		return nil
	}
	return cr.fleet.BillingInstances()
}

// instanceAttrs is the label set shared by the uptime counter and the
// rate gauge. Shared deliberately: the two are meant to be joined, and a
// join only works when both sides carry the same keys.
func instanceAttrs(inst BillingInstance) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String(telemetry.LabelInstanceID, inst.ID),
		attribute.String(telemetry.LabelProvider, inst.Provider),
		attribute.String(telemetry.LabelGPUType, inst.GPUSKU),
		attribute.String(telemetry.LabelBillingMode, inst.BillingMode),
	}
}

// billedSeconds is how long this instance has been on the meter.
//
// Ends at Until when it has stopped, so a terminated instance holds its
// final figure instead of climbing forever. A record with no Since has
// not started billing and reports zero.
func (b BillingInstance) billedSeconds(now time.Time) float64 {
	if b.Since.IsZero() {
		return 0
	}
	end := now
	if !b.Until.IsZero() {
		end = b.Until
	}
	if end.Before(b.Since) {
		return 0
	}
	return end.Sub(b.Since).Seconds()
}

// observeRates is the callback for the per-provider rate observable
// gauge. One observation per row in providers.yaml, regardless of
// which provider this control plane is actually deployed on. The
// cross-provider snapshot panel uses every observation to project
// what cost would look like on each alternative.
func (cr *CostRecorder) observeRates(_ context.Context, observer metric.Float64Observer) error {
	for _, p := range cr.providers {
		observer.Observe(p.EffectiveRateUSDPerSec, metric.WithAttributes(
			attribute.String(telemetry.LabelProvider, p.Name),
			attribute.String(telemetry.LabelGPUType, p.GPUType),
			attribute.String(telemetry.LabelBillingMode, p.BillingMode),
		))
	}
	return nil
}

// ── providers.yaml loader ──────────────────────────────────────────

// LoadProviders reads providers.yaml and normalizes each row into a
// per-second effective rate. Each billing mode has its own normalization:
//
//	metered_per_second   : rate_usd_per_hour / 3600
//	reserved_monthly     : fixed_usd_per_month / 730 / 3600
//	bare_metal_monthly   : fixed_usd_per_month / 730 / 3600 (same shape)
//	owned_capex          : (capex / amort_months / 730 / 3600)
//	                       + (avg_power_watts * power_usd_per_kwh / 1000 / 3600)
//
// The 730 hours/month constant matches the normalization used in the
// chapter and the calculator pages -- 365 * 24 / 12.
func LoadProviders(path string) ([]Provider, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("providers.yaml: %w", err)
	}
	var f providersFile
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("providers.yaml parse: %w", err)
	}
	if len(f.Providers) == 0 {
		return nil, errors.New("providers.yaml: no providers defined")
	}

	out := make([]Provider, 0, len(f.Providers))
	for _, p := range f.Providers {
		rate, err := p.normalize()
		if err != nil {
			return nil, fmt.Errorf("providers.yaml: %s: %w", p.Name, err)
		}
		out = append(out, Provider{
			Name:                   p.Name,
			BillingMode:            p.BillingMode,
			GPUType:                p.GPUType,
			EffectiveRateUSDPerSec: rate,
		})
	}
	return out, nil
}

// providersFile mirrors the on-disk YAML shape.
type providersFile struct {
	LastUpdated string        `yaml:"last_updated"`
	Sources     []string      `yaml:"sources"`
	Providers   []providerDef `yaml:"providers"`
}

// providerDef is the union of fields any single provider entry might
// carry. Only the fields relevant to its billing mode are populated;
// validation in normalize() catches missing required fields per mode.
type providerDef struct {
	Name        string `yaml:"name"`
	BillingMode string `yaml:"billing_mode"`
	GPUType     string `yaml:"gpu_type"`

	// metered_per_second
	RateUSDPerHour float64 `yaml:"rate_usd_per_hour,omitempty"`

	// reserved_monthly / bare_metal_monthly
	FixedUSDPerMonth float64 `yaml:"fixed_usd_per_month,omitempty"`

	// owned_capex
	CapexUSD       float64 `yaml:"capex_usd,omitempty"`
	AmortMonths    int     `yaml:"amort_months,omitempty"`
	AvgPowerWatts  float64 `yaml:"avg_power_watts,omitempty"`
	PowerUSDPerKWh float64 `yaml:"power_usd_per_kwh,omitempty"`

	Notes string `yaml:"notes,omitempty"`
}

// secondsPerHour converts a provider's quoted hourly rate into the
// per-second unit both cost gauges publish.
const secondsPerHour = 3600.0

// hoursPerMonth is the conventional approximation: 365 * 24 / 12.
// Matches the chapter's break-even calculations and the web calculators.
const hoursPerMonth = 730.0

// normalize collapses any billing-mode-specific fields into a single
// effective USD-per-second rate. Validation surfaces bad entries
// (missing required fields, unknown billing mode) at startup, not
// at scrape time.
func (p providerDef) normalize() (float64, error) {
	if p.GPUType == "" {
		return 0, errors.New("gpu_type: required")
	}
	switch p.BillingMode {
	case "metered_per_second":
		if p.RateUSDPerHour <= 0 {
			return 0, errors.New("metered_per_second: rate_usd_per_hour must be > 0")
		}
		return p.RateUSDPerHour / 3600, nil

	case "reserved_monthly", "bare_metal_monthly":
		if p.FixedUSDPerMonth <= 0 {
			return 0, fmt.Errorf("%s: fixed_usd_per_month must be > 0", p.BillingMode)
		}
		return p.FixedUSDPerMonth / hoursPerMonth / 3600, nil

	case "owned_capex":
		if p.CapexUSD <= 0 || p.AmortMonths <= 0 {
			return 0, errors.New("owned_capex: capex_usd and amort_months must be > 0")
		}
		amortPerSec := p.CapexUSD / float64(p.AmortMonths) / hoursPerMonth / 3600
		powerPerSec := p.AvgPowerWatts * p.PowerUSDPerKWh / 1000 / 3600
		return amortPerSec + powerPerSec, nil

	case "spot_variable":
		// Treat as metered for now -- spot rates fluctuate, the gauge
		// just reports the current quoted rate. Revisit when we wire
		// a live rate source.
		if p.RateUSDPerHour <= 0 {
			return 0, errors.New("spot_variable: rate_usd_per_hour must be > 0")
		}
		return p.RateUSDPerHour / 3600, nil

	case "":
		return 0, errors.New("billing_mode: required")
	default:
		return 0, fmt.Errorf("unknown billing_mode: %q", p.BillingMode)
	}
}
