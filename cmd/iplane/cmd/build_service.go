package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/inference-book/inference-plane/internal/deployments/sshdocker"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/local"
	"github.com/inference-book/inference-plane/internal/provisioners/runpod"
	"github.com/inference-book/inference-plane/internal/provisioners/stores/file"
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
func buildLocalService(store *file.Store, operatorID string) (*provisioners.Service, error) {
	keyStore, err := sshkeys.New(sshkeys.WithDir(filepath.Join(store.Dir(), "keys")))
	if err != nil {
		return nil, fmt.Errorf("open ssh key store: %w", err)
	}

	providers := []provisioners.Provider{local.New()}
	if key := os.Getenv("RUNPOD_API_KEY"); key != "" {
		providers = append(providers, runpod.New(runpod.NewClient(key)))
	}

	executor := sshdocker.NewExecutor()

	return provisioners.New(providers, store, operatorID,
		provisioners.WithKeyStore(keyStore),
		provisioners.WithDeploymentExecutor(executor),
		provisioners.WithModelStore(modelStoreForCLI()),
	), nil
}
