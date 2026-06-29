package provisioners

import (
	"fmt"
	"os"
)

// ProviderAPIKeyEnv returns the env var name that holds the named
// provider's API key. The mapping is provider-domain knowledge --
// it lives here alongside the provider constants so cmd/, examples/,
// and any future caller see one source of truth.
//
// Today's mapping:
//
//	runpod      -> RUNPOD_API_KEY
//	vast        -> VAST_API_KEY        (registered when #150 lands)
//	lambdalabs  -> LAMBDA_API_KEY      (registered when #151 lands)
//	local       -> "" (no key required; local provider runs in-process)
//
// Unknown providers return "" -- the caller treats "" as "we don't
// know what to check; let the downstream provider call surface the
// error from the SDK itself."
//
// String literals are used for the vast / lambdalabs cases because
// the provider constants land with each adapter's PR; this file
// can ship ahead of them.
func ProviderAPIKeyEnv(provider string) string {
	switch provider {
	case ProviderRunPod:
		return "RUNPOD_API_KEY"
	case "vast":
		return "VAST_API_KEY"
	case "lambdalabs":
		return "LAMBDA_API_KEY"
	case ProviderLocal:
		return ""
	default:
		return ""
	}
}

// EnsureProviderAPIKey returns an error explaining which env var the
// operator needs to set before driving a request through the named
// provider. nil when the provider doesn't need a key (local) or
// when the key is present.
func EnsureProviderAPIKey(provider string) error {
	keyEnv := ProviderAPIKeyEnv(provider)
	if keyEnv == "" {
		return nil
	}
	if os.Getenv(keyEnv) != "" {
		return nil
	}
	return fmt.Errorf("%s is required (provider=%q provisions real instances; set it in env or pass --service-url to forward to a running iplane serve)", keyEnv, provider)
}

// IsDeployableProvider reports whether the named provider can host
// an engine container (image-native OR SSH-style via the sshdocker
// fallback executor). "local" is excluded -- it's a zero-cost test
// harness with no SSH endpoint.
//
// Used by demos and CLI commands that fail fast on operator
// confusion ("--provider local" while trying to run vLLM).
func IsDeployableProvider(provider string) bool {
	switch provider {
	case ProviderRunPod, "vast", "lambdalabs":
		return true
	default:
		return false
	}
}
