// Package telemetry initializes the OpenTelemetry SDK: tracer provider,
// meter provider, and (via slog handlers) log correlation against trace
// context. v0.1 exports OTLP/gRPC to a local OpenTelemetry Collector,
// which fans out to Tempo / Loki / Mimir for the local Grafana stack.
//
// In production the same exporter points at a hosted observability
// backend (Grafana Cloud, Honeycomb, Datadog OTLP). The SDK code does
// not change; only the OTLP endpoint env var changes.
package telemetry

import (
	"context"

	"github.com/inference-book/inference-plane/internal/config"
)

// Shutdown flushes pending telemetry and closes exporters. Returned by
// Init so the entrypoint can defer it without knowing the SDK internals.
type Shutdown func(ctx context.Context) error

// Init wires up the SDK and returns a Shutdown for graceful teardown.
//
// TODO(v0.1): construct OTLP/gRPC trace and metric exporters, build
// resource (service.name, deployment.environment), register both
// providers globally so chi-otelhttp middleware and the metric
// instruments in internal/server/metrics pick them up.
func Init(ctx context.Context, cfg config.TelemetryConfig) (Shutdown, error) {
	noop := func(context.Context) error { return nil }
	return noop, nil
}
