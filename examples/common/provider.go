// Package common holds tiny helpers shared by the examples/<demo>/
// walkthroughs. Each demo is its own `main` package; this package
// exists so the same three-line "read IPLANE_PROVIDER" lookup
// doesn't get copy-pasted across every new demo we add.
//
// Kept deliberately thin -- if a helper grows beyond a function or
// two of conditional fallback, that's a sign it belongs in the
// production code under cmd/ or internal/, not under examples/.
// The provider->API-key mapping (provider-domain knowledge that
// every CLI consumes) lives in internal/provisioners/apikey.go;
// re-exported here only as convenience aliases so demo binaries can
// import a single `examples/common` package for all their helper
// needs.
package common

import (
	"os"

	"github.com/inference-book/inference-plane/internal/provisioners"
)

// EnvProvider names the env var that overrides each demo's
// `--provider` flag default. Operators set IPLANE_PROVIDER once in
// their shell and every example/<demo> binary picks it up.
//
// Chapter narrative: Ch 6 demos default to runpod (the only
// deployable provider in v0.1). Ch 7 heterogeneous fleets let the
// same demo binary run against any provider iplane has registered:
//
//	IPLANE_PROVIDER=vast bash examples/04-router-in-path/run.sh
//
// Explicit `--provider <name>` on the command line still wins; the
// env var only fills the default when the flag is omitted.
const EnvProvider = "IPLANE_PROVIDER"

// DefaultProvider returns the provider name a demo should use as
// its `--provider` flag default: IPLANE_PROVIDER when set, otherwise
// runpod (preserving Ch 6's behavior for any demo that doesn't
// override).
func DefaultProvider() string {
	if v := os.Getenv(EnvProvider); v != "" {
		return v
	}
	return provisioners.ProviderRunPod
}

// ProviderAPIKeyEnv re-exports provisioners.ProviderAPIKeyEnv so demos
// can stay on the examples/common surface. See the upstream function
// for the mapping.
func ProviderAPIKeyEnv(provider string) string {
	return provisioners.ProviderAPIKeyEnv(provider)
}

// EnsureProviderAPIKey re-exports provisioners.EnsureProviderAPIKey.
func EnsureProviderAPIKey(provider string) error {
	return provisioners.EnsureProviderAPIKey(provider)
}

// IsDeployableProvider re-exports provisioners.IsDeployableProvider.
func IsDeployableProvider(provider string) bool {
	return provisioners.IsDeployableProvider(provider)
}
