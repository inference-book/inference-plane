package cmd

import (
	"os"

	"github.com/inference-book/inference-plane/internal/provisioners"
)

// EnvProvider is the env var the CLI consults as a fallback default
// for `--provider` flags. Operators set IPLANE_PROVIDER once in their
// shell (or .envrc) and every provider-defaulting iplane command
// picks it up without having to repeat `--provider vast` each time.
//
// Chapter narrative: Ch 6's demos default to runpod (the only
// provider shipped in v0.1). Ch 7's heterogeneous fleets let
// operators run the same demo or CLI verb against any provider
// iplane knows about:
//
//	IPLANE_PROVIDER=vast iplane deployment deploy llama --model ...
//
// Explicit `--provider <name>` on the command line still wins -- the
// env var only fills the default when the flag is omitted.
const EnvProvider = "IPLANE_PROVIDER"

// defaultProvider returns the provider name the caller should use as
// a flag default: the IPLANE_PROVIDER env var when set, otherwise the
// fallback supplied by the caller (typically the legacy default like
// `provisioners.ProviderRunPod` or `""` when the flag is required).
//
// Used at flag-definition time so the resolved default appears in
// `--help` output and survives a `pflag.Reset`. Resolving once at
// startup matches how viper bindings work elsewhere in the CLI.
func defaultProvider(fallback string) string {
	if v := os.Getenv(EnvProvider); v != "" {
		return v
	}
	return fallback
}

// providerAPIKeyEnv is a wrapper around provisioners.ProviderAPIKeyEnv
// kept here so cmd/ callsites import one symbol from one neighbor
// (provider_env.go) for both the IPLANE_PROVIDER default and the
// per-provider API-key env-var name. The mapping itself is provider-
// domain knowledge that lives in internal/provisioners/apikey.go.
func providerAPIKeyEnv(provider string) string {
	return provisioners.ProviderAPIKeyEnv(provider)
}

// ensureProviderAPIKey returns a clear "set $X" error when a provider
// that needs an API key has it missing from env. Used by the CLI
// subcommands that auto-provision (iplane up, iplane instance
// create <provider>) so the failure surfaces before any provider
// SDK call.
func ensureProviderAPIKey(provider string) error {
	return provisioners.EnsureProviderAPIKey(provider)
}
