// Package telemetry initializes the OpenTelemetry SDK: tracer provider,
// meter provider, and resource attributes describing this deployment.
//
// v0.1 exports OTLP/gRPC to a local OpenTelemetry Collector, which fans
// out to Tempo (traces), Mimir (metrics), and Loki (logs). Production
// swap is one env var change (OTEL_EXPORTER_OTLP_ENDPOINT) -- the SDK
// code stays the same.
//
// Logs are not emitted via the OTel SDK in v0.1; slog writes structured
// JSON to stdout and the docker logging driver forwards to the
// collector. The OTel logs SDK is still in v0.x at time of writing;
// adding it is straightforward but waits until a stable release.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"

	"github.com/inference-book/inference-plane/internal/config"
)

// Shutdown flushes pending telemetry and closes exporters. Returned by
// Init so the entrypoint can defer it without knowing the SDK internals.
type Shutdown func(ctx context.Context) error

// Init wires up the SDK and returns a Shutdown for graceful teardown.
//
// Two providers go global:
//   - TracerProvider with OTLP/gRPC exporter, configured sampler.
//   - MeterProvider with OTLP/gRPC exporter, periodic export every 15s.
//
// Resource attributes carry the deployment's identity: service.name,
// environment, plus the cost-relevant labels (provider, gpu_type,
// billing_mode, instance_id). These are attached to every span and
// every metric, so the cross-provider cost panels and per-instance
// drill-downs work without per-call labeling.
//
// W3C Trace Context + Baggage propagators are registered globally so
// outbound HTTP calls (vLLM, future backends) carry trace IDs through
// headers, enabling end-to-end traces across services.
func Init(ctx context.Context, cfg config.TelemetryConfig, dep config.DeploymentConfig) (Shutdown, error) {
	if cfg.ServiceName == "" {
		return nil, errors.New("telemetry: service_name is required")
	}

	res, err := buildResource(ctx, cfg, dep)
	if err != nil {
		return nil, fmt.Errorf("telemetry: resource: %w", err)
	}

	traceProvider, traceShutdown, err := buildTracerProvider(ctx, cfg, res)
	if err != nil {
		return nil, fmt.Errorf("telemetry: tracer provider: %w", err)
	}
	otel.SetTracerProvider(traceProvider)

	meterProvider, meterShutdown, err := buildMeterProvider(ctx, cfg, res)
	if err != nil {
		// Roll back the tracer provider so we don't leak the exporter.
		_ = traceShutdown(ctx)
		return nil, fmt.Errorf("telemetry: meter provider: %w", err)
	}
	otel.SetMeterProvider(meterProvider)

	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	shutdown := func(ctx context.Context) error {
		// Run both shutdowns even if the first errors so we don't
		// leak resources from the second.
		errT := traceShutdown(ctx)
		errM := meterShutdown(ctx)
		return errors.Join(errT, errM)
	}
	return shutdown, nil
}

// buildResource constructs the OTel resource. The deployment-identity
// fields (provider, gpu_type, billing_mode, instance_id) are
// intentionally optional; an unlabeled deployment still emits useful
// telemetry, just without the per-provider cost breakdown.
func buildResource(ctx context.Context, cfg config.TelemetryConfig, dep config.DeploymentConfig) (*resource.Resource, error) {
	attrs := []attribute.KeyValue{
		semconv.ServiceName(cfg.ServiceName),
		semconv.DeploymentEnvironmentName(cfg.Environment),
	}
	if dep.Provider != "" {
		attrs = append(attrs, attribute.String(AttrDeploymentProvider, dep.Provider))
	}
	if dep.GPUType != "" {
		attrs = append(attrs, attribute.String(AttrDeploymentGPUType, dep.GPUType))
	}
	if dep.BillingMode != "" {
		attrs = append(attrs, attribute.String(AttrDeploymentBillingMode, dep.BillingMode))
	}
	if dep.InstanceID != "" {
		attrs = append(attrs, attribute.String(AttrDeploymentInstanceID, dep.InstanceID))
	}

	return resource.New(ctx,
		resource.WithAttributes(attrs...),
		resource.WithFromEnv(),     // honor OTEL_RESOURCE_ATTRIBUTES
		resource.WithProcess(),     // process.pid, process.executable.name
		resource.WithHost(),        // host.name, host.id
	)
}

func buildTracerProvider(ctx context.Context, cfg config.TelemetryConfig, res *resource.Resource) (*sdktrace.TracerProvider, Shutdown, error) {
	exporter, err := otlptrace.New(ctx, otlptracegrpc.NewClient(
		otlptracegrpc.WithEndpoint(cfg.OTLPEndpoint),
		otlptracegrpc.WithInsecure(), // in-cluster traffic; TLS is the collector's job to terminate at the edge
	))
	if err != nil {
		return nil, nil, err
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter,
			sdktrace.WithMaxQueueSize(2048),
			sdktrace.WithBatchTimeout(5*time.Second),
		),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(
			sdktrace.TraceIDRatioBased(samplingRatio(cfg.SampleRatio)),
		)),
	)
	shutdown := func(ctx context.Context) error { return provider.Shutdown(ctx) }
	return provider, shutdown, nil
}

func buildMeterProvider(ctx context.Context, cfg config.TelemetryConfig, res *resource.Resource) (*sdkmetric.MeterProvider, Shutdown, error) {
	exporter, err := otlpmetricgrpc.New(ctx,
		otlpmetricgrpc.WithEndpoint(cfg.OTLPEndpoint),
		otlpmetricgrpc.WithInsecure(),
	)
	if err != nil {
		return nil, nil, err
	}

	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter,
			sdkmetric.WithInterval(15*time.Second),
		)),
		sdkmetric.WithResource(res),
	)
	shutdown := func(ctx context.Context) error { return provider.Shutdown(ctx) }
	return provider, shutdown, nil
}

// samplingRatio clamps the configured sample ratio into [0, 1]. A ratio
// outside that range is almost certainly a config bug; we'd rather be
// over-permissive at SDK init than fail to start. Production deployments
// should set this explicitly (typically 0.01-0.1 for high traffic).
func samplingRatio(r float64) float64 {
	if r <= 0 {
		return 0
	}
	if r >= 1 {
		return 1
	}
	return r
}
