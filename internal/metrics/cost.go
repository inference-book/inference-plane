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

// Deployment carries the labels that identify which provider /
// gpu_type / billing_mode the running control plane is on. Sourced
// from config.DeploymentConfig.
type Deployment struct {
	Provider    string
	GPUType     string
	BillingMode string
	InstanceID  string
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
// chapter's cost-tracking subsection. Three signals:
//
//   - instance.uptime.seconds.total       (observable Int64Counter)
//     wall-clock seconds since the control plane started. What you
//     are billed for in metered mode.
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
	deployment    Deployment
	providers     []Provider
	startTime     time.Time
	activeSeconds metric.Float64Counter
}

// NewCostRecorder constructs the cost instruments. The observable
// callbacks (uptime, rates) capture this CostRecorder by closure --
// each scrape interval the SDK invokes them and they emit their
// current observations.
func NewCostRecorder(dep Deployment, providers []Provider) (*CostRecorder, error) {
	meter := otel.Meter("inference-plane/cost")
	cr := &CostRecorder{
		deployment: dep,
		providers:  providers,
		startTime:  time.Now(),
	}

	if _, err := meter.Int64ObservableCounter(
		telemetry.MetricInstanceUptimeSeconds,
		metric.WithUnit("s"),
		metric.WithDescription("Wall-clock seconds since this control plane started. Base for billed-time cost in metered mode."),
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
// counter. Called from the service layer once the backend.Generate
// call returns (success or failure -- failure still counted because
// the GPU did burn time even if the request errored).
func (cr *CostRecorder) RecordActive(ctx context.Context, model string, durationSec float64) {
	if cr == nil || durationSec <= 0 {
		return
	}
	cr.activeSeconds.Add(ctx, durationSec, metric.WithAttributes(
		attribute.String(telemetry.LabelModel, model),
		attribute.String(telemetry.LabelProvider, cr.deployment.Provider),
		attribute.String(telemetry.LabelGPUType, cr.deployment.GPUType),
		attribute.String(telemetry.LabelBillingMode, cr.deployment.BillingMode),
	))
}

// observeUptime is the callback for the uptime observable counter.
// Returns wall-clock seconds since CostRecorder construction. Same
// deployment labels as RecordActive so the two metrics join on
// {provider, gpu_type, billing_mode} for utilization PromQL.
func (cr *CostRecorder) observeUptime(_ context.Context, observer metric.Int64Observer) error {
	observer.Observe(int64(time.Since(cr.startTime).Seconds()), metric.WithAttributes(
		attribute.String(telemetry.LabelProvider, cr.deployment.Provider),
		attribute.String(telemetry.LabelGPUType, cr.deployment.GPUType),
		attribute.String(telemetry.LabelBillingMode, cr.deployment.BillingMode),
	))
	return nil
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
