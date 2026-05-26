package cmd

import (
	"os"

	"github.com/inference-book/inference-plane/internal/modelstores"
	"github.com/inference-book/inference-plane/internal/modelstores/huggingface"
)

// skipModelValidation is the persistent root flag controlling whether
// in-process Service construction wires the HF-backed model store
// (validates + propagates HF_TOKEN) or a no-op Passthrough. The flag
// lives on rootCmd so every verb that constructs a Service inherits
// it uniformly without each verb re-declaring it.
//
// Set via `iplane --skip-model-validation <verb>` or per-shell via
// IPLANE_SKIP_MODEL_VALIDATION=1.
var skipModelValidation bool

// modelStoreForCLI returns the ModelStore CLI commands should wire
// into provisioners.New. Honors --skip-model-validation (and the
// IPLANE_SKIP_MODEL_VALIDATION env fallback for unattended runs).
// Picks up $HF_TOKEN from the operator's shell for gated-model auth.
//
// Centralized here so a future "use a different validator" change
// (e.g. a vLLM-config sanity checker) only touches one site.
func modelStoreForCLI() modelstores.ModelStore {
	if skipModelValidation || os.Getenv("IPLANE_SKIP_MODEL_VALIDATION") == "1" {
		return modelstores.Passthrough{}
	}
	return huggingface.New(os.Getenv("HF_TOKEN"))
}
