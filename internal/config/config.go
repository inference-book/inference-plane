// Package config holds the typed Config struct that the iplane serve
// subcommand assembles from viper (file + env + flags + defaults).
//
// Loading is done in cmd/iplane/cmd/serve.go via viper.Unmarshal with
// the `yaml` struct tag; this package owns the types and the
// validation rules but not the precedence logic.
package config

import (
	"errors"
	"time"
)

// Config is the top-level deployment config.
type Config struct {
	Server     ServerConfig     `yaml:"server"`
	Backend    BackendConfig    `yaml:"backend"`
	Telemetry  TelemetryConfig  `yaml:"telemetry"`
	Deployment DeploymentConfig `yaml:"deployment"`
	Router     RouterConfig     `yaml:"router"`
	ModelCache ModelCacheConfig `yaml:"model_cache"`
}

// ModelCacheConfig activates a warm model-cache store so deployments
// mount pre-staged weights off a persistent volume instead of
// re-downloading from HuggingFace every deploy. Absent (the default,
// and every pre-Ch-9 example's config) means the plain HF-passthrough
// store: cold download inside the pod. A forward example opts in by
// adding this block, so earlier demos keep the cold path unchanged.
//
// The block is provider-agnostic. VolumeID names a provider-managed
// volume handle for the image-native deploy path (a RunPod network
// volume id today); HostPath names a host directory for the sshdocker
// bind path (a VM provider's mounted filesystem). Set whichever your
// deploy path uses; either non-empty enables the cache. MountPath is
// the in-container path the engine reads from (defaults to /models).
//
// Populating the volume is out of scope here -- this block mounts a
// volume assumed to already hold the weights. On a cache miss the
// engine still reaches HF, so a misconfigured volume degrades to a cold
// deploy rather than failing.
type ModelCacheConfig struct {
	VolumeID  string `yaml:"volume_id"`  // provider-managed volume handle (image-native path)
	HostPath  string `yaml:"host_path"`  // host directory to bind (sshdocker path)
	MountPath string `yaml:"mount_path"` // in-container attach path; defaults to /models
}

// Enabled reports whether the warm model-cache is configured. Either a
// volume handle or a host path turns it on; both empty is the default
// cold-download path.
func (m ModelCacheConfig) Enabled() bool {
	return m.VolumeID != "" || m.HostPath != ""
}

// RouterConfig configures the v0.2 data-plane router. Beat 2 adds the
// queue; ch8 adds per-request routing-policy selection.
type RouterConfig struct {
	Queue QueueConfig `yaml:"queue"`

	// TouchDebounceInterval is the minimum wall-clock gap between two
	// persisted TouchDeployment writes for the same deploy_id. The
	// router fires touch on every inference request; without
	// debouncing, each request takes a full state.json read+write
	// tax. Default is 5s. Lower = fresher last_activity_at on disk
	// (the reaper sees recent touches sooner). Higher = less disk IO
	// on the hot path. Zero disables debouncing (every touch hits
	// disk -- v0.1 behavior).
	TouchDebounceInterval time.Duration `yaml:"touch_debounce_interval"`

	// RoutingPolicy selects per-request replica selection (v0.2 ch8):
	// "round_robin" (default, the Ch 7 load balancer) or
	// "prefix_affinity" (Ch 8 sticky routing -- pin a session's turns
	// to the replica that holds its prefix). Unknown values are
	// rejected at startup.
	RoutingPolicy string `yaml:"routing_policy"`

	// AffinityOverloadThreshold enables the prefix_affinity load-aware
	// override (v0.2 ch8): when a session's pinned replica has this many
	// or more in-flight requests, the turn spills to the coolest eligible
	// replica (the pin is kept, so the session snaps back once its
	// replica cools). 0 (default) disables the override -- pure
	// stickiness. Only consulted when routing_policy is prefix_affinity.
	AffinityOverloadThreshold int `yaml:"affinity_overload_threshold"`
}

// QueueConfig parameterizes the M/M/k waiting room in front of the
// engine. Beat 2.4 consolidated the two-pool model into a single
// scheduler that holds both lanes. Servicers is the global worker
// count (shared across lanes); Interactive.Capacity and
// Batch.Capacity are per-lane bounded waiting-room sizes.
//
// Backward-compat: when only the top-level Capacity is set (Beat 2.1
// shape), both lanes inherit that capacity.
//
// InFlightCap (Beat 2.4) is the per-deployment in-flight bound
// the scheduler enforces -- mirrors the engine's max-num-seqs so
// iplane doesn't outpace the engine's batcher.
//
// All zero values is the Beat 1 path: direct forward, no scheduler.
type QueueConfig struct {
	Servicers   int       `yaml:"servicers"`
	Capacity    int       `yaml:"capacity"`
	InFlightCap int       `yaml:"in_flight_cap"`
	Interactive LaneQueue `yaml:"interactive"`
	Batch       LaneQueue `yaml:"batch"`

	// TenantWeights configures per-tenant fair-share weights for
	// the scheduler's intra-lane dispatch (v0.2 ch7-beat2.5).
	// Tenants not listed get scheduler.DefaultTenantWeight on
	// first Submit. Nil / empty map means "all tenants equal."
	// Restart-only in v0.2; hot-reload is filed as a follow-up.
	TenantWeights map[string]int `yaml:"tenant_weights"`
}

// LaneQueue is the per-priority-lane configuration. Beat 2.4
// honors only Capacity (the per-lane bounded waiting room);
// Servicers is shared across lanes via QueueConfig.Servicers.
type LaneQueue struct {
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
	// Per-lane sub-blocks: same invariants as the top-level fields.
	if cfg.Router.Queue.Interactive.Servicers < 0 {
		return errors.New("config: router.queue.interactive.servicers must be >= 0")
	}
	if cfg.Router.Queue.Interactive.Servicers > 0 && cfg.Router.Queue.Interactive.Capacity <= 0 {
		return errors.New("config: router.queue.interactive.capacity must be > 0 when servicers > 0")
	}
	if cfg.Router.Queue.Batch.Servicers < 0 {
		return errors.New("config: router.queue.batch.servicers must be >= 0")
	}
	if cfg.Router.Queue.Batch.Servicers > 0 && cfg.Router.Queue.Batch.Capacity <= 0 {
		return errors.New("config: router.queue.batch.capacity must be > 0 when servicers > 0")
	}
	if cfg.Router.Queue.InFlightCap < 0 {
		return errors.New("config: router.queue.in_flight_cap must be >= 0 (0 = unlimited)")
	}
	return nil
}
