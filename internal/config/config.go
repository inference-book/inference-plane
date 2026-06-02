// Package config holds the typed Config struct that the iplane serve
// subcommand assembles from viper (file + env + flags + defaults).
//
// Loading is done in cmd/iplane/cmd/serve.go via viper.Unmarshal with
// the `yaml` struct tag; this package owns the types and the
// validation rules but not the precedence logic.
package config

import "errors"

// Config is the top-level deployment config.
type Config struct {
	Server     ServerConfig     `yaml:"server"`
	Backend    BackendConfig    `yaml:"backend"`
	Telemetry  TelemetryConfig  `yaml:"telemetry"`
	Deployment DeploymentConfig `yaml:"deployment"`
	Router     RouterConfig     `yaml:"router"`
}

// RouterConfig configures the v0.2 data-plane router. Beat 2 adds the
// queue; future beats will add per-replica selection knobs here.
type RouterConfig struct {
	Queue QueueConfig `yaml:"queue"`
}

// QueueConfig parameterizes the M/M/k waiting room in front of the
// engine. Servicers is k (number of parallel dispatcher goroutines);
// Capacity is the bounded waiting-room size N.
//
// Servicers == 0 is the Beat 1 path: no queue, no concurrency cap;
// requests forward inline. Servicers > 0 activates the queue and
// caps engine concurrency.
type QueueConfig struct {
	Servicers int `yaml:"servicers"`
	Capacity  int `yaml:"capacity"`
}

// ServerConfig configures the HTTP listener of the control plane.
type ServerConfig struct {
	Addr            string `yaml:"addr"`              // e.g. ":8080"
	ReadTimeoutSec  int    `yaml:"read_timeout_sec"`  // request read deadline
	WriteTimeoutSec int    `yaml:"write_timeout_sec"` // response write deadline
	ShutdownSec     int    `yaml:"shutdown_sec"`      // graceful shutdown budget
}

// BackendConfig identifies the inference engine for this deployment.
type BackendConfig struct {
	Engine string `yaml:"engine"` // "vllm" | "mock"
	URL    string `yaml:"url"`    // e.g. "http://vllm:8000" (ignored by mock)
	Name   string `yaml:"name"`   // label value for metrics
}

// TelemetryConfig configures the OpenTelemetry SDK.
type TelemetryConfig struct {
	OTLPEndpoint string  `yaml:"otlp_endpoint"` // e.g. "otel-collector:4317"
	ServiceName  string  `yaml:"service_name"`  // resource attribute
	Environment  string  `yaml:"environment"`   // "dev" / "prod"
	SampleRatio  float64 `yaml:"sample_ratio"`  // trace head sampling
}

// DeploymentConfig describes which provider/gpu_type/billing_mode
// this control plane instance is running on. Drives the labels on
// cost metrics so the actual-spend panel is anchored to reality.
type DeploymentConfig struct {
	Provider    string `yaml:"provider"`
	GPUType     string `yaml:"gpu_type"`
	BillingMode string `yaml:"billing_mode"`
	InstanceID  string `yaml:"instance_id"`
}

// Validate enforces the minimum a useful deployment needs. The
// deployment-identity labels are intentionally optional -- a deployment
// without them emits unlabeled cost metrics, which is a degraded
// experience but not a failure mode.
func Validate(cfg *Config) error {
	if cfg.Server.Addr == "" {
		return errors.New("config: server.addr is required")
	}
	if cfg.Backend.Engine == "" {
		return errors.New("config: backend.engine is required")
	}
	if cfg.Telemetry.ServiceName == "" {
		return errors.New("config: telemetry.service_name is required")
	}
	// router.queue: servicers and capacity are independent knobs;
	// validate the meaningful invariants. Servicers == 0 is the Beat 1
	// path (queue disabled); negative is a typo.
	if cfg.Router.Queue.Servicers < 0 {
		return errors.New("config: router.queue.servicers must be >= 0 (0 disables the queue)")
	}
	if cfg.Router.Queue.Servicers > 0 && cfg.Router.Queue.Capacity <= 0 {
		return errors.New("config: router.queue.capacity must be > 0 when servicers > 0")
	}
	return nil
}
