package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/inference-book/inference-plane/internal/deployments/sshdocker"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/external"
	"github.com/inference-book/inference-plane/internal/provisioners/lambdalabs"
	"github.com/inference-book/inference-plane/internal/provisioners/local"
	"github.com/inference-book/inference-plane/internal/provisioners/runpod"
	"github.com/inference-book/inference-plane/internal/provisioners/stores/file"
	"github.com/inference-book/inference-plane/internal/provisioners/vast"
	"github.com/inference-book/inference-plane/internal/sshkeys"
)

// buildLocalService constructs a fully-wired in-process *provisioners.Service
// against the given state store. Single helper called by both the daemon
// path (cmd/iplane/cmd/serve.go) and the CLI in-process path
// (cmd/iplane/cmd/instance.go, deployment.go, up.go) so the wiring stays in
// one place when new providers, key-stores, or executors land.
//
// The store is expected to already be Open'd by the caller; this helper
// does not touch flock semantics. Daemon callers will typically hold a
// LockForLifetime over the store while this Service runs.
//
// Providers included:
//
//   - local: always present (zero-cost on-ramp).
//   - runpod: included only when RUNPOD_API_KEY is set in env. Same env
//     contract as the v0.1 CLI so the daemon and one-shot CLIs see the
//     same provider catalog without configuration drift.
//   - vast: included only when VAST_API_KEY is set in env. v0.2 ch7-
//     beat3.11 (#150). VM-style provider; the engine container runs via
//     the sshdocker executor.
//   - lambdalabs: included only when LAMBDA_API_KEY is set in env.
//     v0.2 ch7-beat3.12 (#151). VM-style provider; the engine
//     container runs via the sshdocker executor.
func buildLocalService(store *file.Store, operatorID string, extra ...provisioners.Option) (*provisioners.Service, error) {
	keyStore, err := sshkeys.New(sshkeys.WithDir(filepath.Join(store.Dir(), "keys")))
	if err != nil {
		return nil, fmt.Errorf("open ssh key store: %w", err)
	}

	providers := []provisioners.Provider{local.New(), external.New()}
	if key := os.Getenv("RUNPOD_API_KEY"); key != "" {
		var rpOpts []runpod.Option
		// The engine-ready wait defaults to 10m. A large engine image
		// cold-pulling on community capacity (plus the model load) can
		// exceed that, so let an operator extend it. Ch 9's big-model
		// deploys are the motivating case.
		if d := engineReadyTimeout("IPLANE_RUNPOD_ENGINE_READY_TIMEOUT"); d > 0 {
			rpOpts = append(rpOpts, runpod.WithEngineReadyTimeout(d))
		}
		providers = append(providers, runpod.New(runpod.NewClient(key), rpOpts...))
	}
	if key := os.Getenv("VAST_API_KEY"); key != "" {
		providers = append(providers, vast.New(vast.NewClient(key)))
	}
	if key := os.Getenv("LAMBDA_API_KEY"); key != "" {
		providers = append(providers, lambdalabs.New(lambdalabs.NewClient(key)))
	}

	// Engine-ready wait for the VM-style deploy path. Mirrors the
	// image-native path's IPLANE_RUNPOD_ENGINE_READY_TIMEOUT, because the
	// operator-facing question is identical: how long may a legitimate
	// deploy take to start serving. Left generic rather than named after
	// sshdocker, so a second VM-style path does not grow a third spelling.
	var execOpts []sshdocker.Option
	if d := engineReadyTimeout(""); d > 0 {
		execOpts = append(execOpts, sshdocker.WithHealthPoll(2*time.Second, d))
	}
	executor := sshdocker.NewExecutor(execOpts...)

	opts := []provisioners.Option{
		provisioners.WithKeyStore(keyStore),
		provisioners.WithDeploymentExecutor(executor),
		provisioners.WithModelStore(modelStoreForCLI()),
	}
	// Where an engine's agent should register. Not inferred from the listen
	// address: a daemon on :8080 behind NAT cannot see its own reachable
	// URL, and a plausible-looking guess produces agents that never arrive.
	// Same shape as IPLANE_OTEL_ENDPOINT, and `iplane telemetry url`
	// discovers a cloudflared tunnel for the local case.
	if url := os.Getenv("IPLANE_AGENT_SERVICE_URL"); url != "" {
		opts = append(opts, provisioners.WithAgentServiceURL(url))
	}
	opts = append(opts, extra...)
	return provisioners.New(providers, store, operatorID, opts...), nil
}

// engineReadyTimeout resolves how long a deploy may take to start serving.
//
// specific is a provider-scoped env var consulted first, so an operator who
// already set IPLANE_RUNPOD_ENGINE_READY_TIMEOUT keeps their behaviour;
// IPLANE_ENGINE_READY_TIMEOUT is the generic fallback that applies to every
// deploy path. Pass "" to consult only the generic one.
//
// Returns 0 when neither is set or the value does not parse, meaning the
// caller keeps its own default. A malformed duration is ignored rather than
// fatal: refusing to start the daemon over a typo'd timeout would be a
// worse outcome than using the default and letting the deploy report what
// happened.
func engineReadyTimeout(specific string) time.Duration {
	for _, key := range []string{specific, "IPLANE_ENGINE_READY_TIMEOUT"} {
		if key == "" {
			continue
		}
		if v := os.Getenv(key); v != "" {
			if d, err := time.ParseDuration(v); err == nil && d > 0 {
				return d
			}
		}
	}
	return 0
}
