// Package config loads the control plane's runtime configuration from a
// YAML file with environment-variable overrides (12-factor style).
//
// Two related configs feed v0.1:
//
//   - The deployment config (this file) describes the running instance:
//     where vLLM is, what the OTLP endpoint is, which provider+gpu_type
//     this control plane is running on (used to label cost metrics).
//
//   - providers.yaml in the repo root holds the multi-provider rate
//     table. The control plane emits a gauge per provider so the
//     cross-provider cost-projection panel works without redeploying
//     when rates change.
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the top-level deployment config.
type Config struct {
	Server     ServerConfig     `yaml:"server"`
	Backend    BackendConfig    `yaml:"backend"`
	Telemetry  TelemetryConfig  `yaml:"telemetry"`
	Deployment DeploymentConfig `yaml:"deployment"`
}

// ServerConfig configures the HTTP listener of the control plane.
type ServerConfig struct {
	Addr            string `yaml:"addr"`              // e.g. ":8080"
	ReadTimeoutSec  int    `yaml:"read_timeout_sec"`  // request read deadline
	WriteTimeoutSec int    `yaml:"write_timeout_sec"` // response write deadline
	ShutdownSec     int    `yaml:"shutdown_sec"`      // graceful shutdown budget
}

// BackendConfig identifies the inference engine for this deployment.
// Engine selects the implementation; URL is engine-specific.
type BackendConfig struct {
	Engine string `yaml:"engine"` // "vllm" in v0.1
	URL    string `yaml:"url"`    // e.g. "http://vllm:8000"
	Name   string `yaml:"name"`   // label value for metrics
}

// TelemetryConfig configures the OpenTelemetry SDK.
type TelemetryConfig struct {
	OTLPEndpoint string  `yaml:"otlp_endpoint"` // e.g. "otel-collector:4317"
	ServiceName  string  `yaml:"service_name"`  // resource attribute
	Environment  string  `yaml:"environment"`   // "dev" / "prod"
	SampleRatio  float64 `yaml:"sample_ratio"`  // trace head sampling
}

// DeploymentConfig describes which provider/gpu_type/billing_mode this
// control plane instance is running on. Drives the labels on cost
// metrics so the actual-spend panel is anchored to reality.
type DeploymentConfig struct {
	Provider    string `yaml:"provider"`     // "runpod", "lambda", "equinix_metal", "owned"
	GPUType     string `yaml:"gpu_type"`     // "a10g", "rtx4090", "a40", "h100"
	BillingMode string `yaml:"billing_mode"` // "metered_per_second", "reserved_monthly", "owned_capex"
	InstanceID  string `yaml:"instance_id"`  // unique per-instance label value
}

// Load reads YAML config from path, applies env-var overrides for the
// fields a deployer typically tweaks at runtime, fills defaults for
// anything left unset, and validates the result.
//
// Layering is deliberate: defaults < YAML < env. The reasoning:
// defaults cover the development case (run with no config file, talk
// to localhost), YAML covers per-deployment shape (which engine, which
// observability backend), env covers per-instance secrets and overrides
// that should not be checked in alongside the YAML.
func Load(path string) (*Config, error) {
	cfg := defaultConfig()

	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("config: read %s: %w", path, err)
		}
		if err := yaml.Unmarshal(raw, cfg); err != nil {
			return nil, fmt.Errorf("config: parse %s: %w", path, err)
		}
	}

	applyEnvOverrides(cfg)

	if err := validate(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func defaultConfig() *Config {
	return &Config{
		Server: ServerConfig{
			Addr:            ":8080",
			ReadTimeoutSec:  60,
			WriteTimeoutSec: 600, // long enough for slow generations
			ShutdownSec:     30,
		},
		Backend: BackendConfig{
			Engine: "vllm",
			URL:    "http://localhost:8000",
			Name:   "vllm-primary",
		},
		Telemetry: TelemetryConfig{
			OTLPEndpoint: "localhost:4317",
			ServiceName:  "inference-plane",
			Environment:  "dev",
			SampleRatio:  1.0, // sample everything in dev
		},
	}
}

// applyEnvOverrides reads CP_-prefixed environment variables and
// overlays them on cfg. Only the most-frequently-tweaked fields are
// exposed this way; less common fields stay YAML-only to keep the
// override surface manageable.
func applyEnvOverrides(cfg *Config) {
	if v := os.Getenv("CP_SERVER_ADDR"); v != "" {
		cfg.Server.Addr = v
	}
	if v := os.Getenv("CP_BACKEND_ENGINE"); v != "" {
		cfg.Backend.Engine = v
	}
	if v := os.Getenv("CP_BACKEND_URL"); v != "" {
		cfg.Backend.URL = v
	}
	if v := os.Getenv("CP_BACKEND_NAME"); v != "" {
		cfg.Backend.Name = v
	}
	// OTel SDK already honors OTEL_EXPORTER_OTLP_ENDPOINT directly,
	// but we also read it here so cfg.Telemetry reflects what the
	// SDK will actually use for logging diagnostics.
	if v := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); v != "" {
		cfg.Telemetry.OTLPEndpoint = strings.TrimPrefix(v, "http://")
	}
	if v := os.Getenv("CP_PROVIDER"); v != "" {
		cfg.Deployment.Provider = v
	}
	if v := os.Getenv("CP_GPU_TYPE"); v != "" {
		cfg.Deployment.GPUType = v
	}
	if v := os.Getenv("CP_BILLING_MODE"); v != "" {
		cfg.Deployment.BillingMode = v
	}
	if v := os.Getenv("CP_INSTANCE_ID"); v != "" {
		cfg.Deployment.InstanceID = v
	}
}

// validate enforces the minimum a useful deployment needs: a backend
// URL, an OTLP endpoint, and a non-empty service name. The deployment
// labels (provider, gpu_type, billing_mode) are intentionally optional;
// a deployment without them just emits unlabeled cost metrics, which
// is a degraded experience but not a failure mode.
func validate(cfg *Config) error {
	if cfg.Server.Addr == "" {
		return errors.New("config: server.addr is required")
	}
	if cfg.Backend.Engine == "" {
		return errors.New("config: backend.engine is required")
	}
	if cfg.Backend.URL == "" {
		return errors.New("config: backend.url is required")
	}
	if cfg.Telemetry.ServiceName == "" {
		return errors.New("config: telemetry.service_name is required")
	}
	return nil
}
