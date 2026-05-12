package runpod

import (
	"fmt"
	"strings"

	"github.com/inference-book/inference-plane/internal/provisioners"
)

// skusByClass maps iplane's GPU class taxonomy to RunPod's canonical
// gpuTypeId strings (the literal values accepted by the
// podFindAndDeployOnDemand mutation). The first entry per class is the
// "preferred" SKU resolveSKU returns when the operator supplies
// --gpu-class but not --gpu-sku.
//
// The list is intentionally short. Operators wanting a specific SKU
// not in the table pass --gpu-sku <gpuTypeId> on the CLI, which
// bypasses this lookup entirely (the design doc's escape hatch).
//
// Verify the strings against the live API with:
//
//	curl -X POST https://api.runpod.io/graphql \
//	  -H "Authorization: Bearer $RUNPOD_API_KEY" \
//	  -H "Content-Type: application/json" \
//	  -d '{"query": "query { gpuTypes { id displayName memoryInGb } }"}'
//
// RunPod occasionally adds new SKUs or renames old ones; the
// per-chapter tag (`ch06-final`) freezes the version of this table the
// chapter narrative depends on.
var skusByClass = map[string][]string{
	provisioners.GPUClassSmall: {
		"NVIDIA GeForce RTX 4090",
		"NVIDIA GeForce RTX 5090",
	},
	provisioners.GPUClassMedium: {
		"NVIDIA RTX A6000",
		"NVIDIA A100 40GB PCIe",
	},
	provisioners.GPUClassLarge: {
		"NVIDIA A100 80GB PCIe",
		"NVIDIA A100-SXM4-80GB",
		"NVIDIA H100 80GB HBM3",
	},
	provisioners.GPUClassXLarge: {
		"NVIDIA H100 80GB HBM3",
		"NVIDIA H100 NVL",
	},
}

// classBySKU is the reverse index, built once at init. Used by
// classifySKU to label an existing pod's GPU when we adopt it via
// Describe / List (the pod was created from a sku string and we want
// to surface the matching class to the operator).
var classBySKU = func() map[string]string {
	m := make(map[string]string)
	// Iterate classes in a stable order so a SKU listed under multiple
	// classes (H100 80GB HBM3 appears under both large and xlarge)
	// resolves to the same class deterministically.
	for _, class := range []string{
		provisioners.GPUClassSmall,
		provisioners.GPUClassMedium,
		provisioners.GPUClassLarge,
		provisioners.GPUClassXLarge,
	} {
		for _, sku := range skusByClass[class] {
			if _, exists := m[sku]; !exists {
				m[sku] = class
			}
		}
	}
	return m
}()

// resolveSKU picks the preferred SKU for a class. Returns a clear
// error listing the known classes if the input does not match -- this
// is the message the operator sees when they type a typo'd class.
func resolveSKU(class string) (string, error) {
	skus, ok := skusByClass[class]
	if !ok || len(skus) == 0 {
		return "", fmt.Errorf("no SKU mapping for class %q (known classes: %s)", class, strings.Join(knownClasses(), ", "))
	}
	return skus[0], nil
}

// classifySKU returns the class that owns a given SKU, or "" if the
// SKU is not in our table. Empty result means "operator-supplied sku
// we have no opinion about" -- we still pass it through.
func classifySKU(sku string) string {
	return classBySKU[sku]
}

func knownClasses() []string {
	return []string{
		provisioners.GPUClassSmall,
		provisioners.GPUClassMedium,
		provisioners.GPUClassLarge,
		provisioners.GPUClassXLarge,
	}
}

// isActiveProviderState reports whether a RunPod desiredStatus counts
// as "the instance is running" for idempotency-adoption purposes. The
// Service struct calls into this via isActiveLikeProviderState; we
// keep the vocabulary local to the adapter so the central registry
// does not need to know RunPod's state names.
//
// CREATED and RUNNING both count: a CREATED pod is being initialized
// but billing has started and the operator already owns it.
// RESTARTING also counts -- a transient state, not a teardown.
//
// EXITED, TERMINATED, and any error states do NOT count -- adopt them
// would re-Spawn against a gone instance.
func isActiveProviderState(state string) bool {
	switch strings.ToUpper(state) {
	case "CREATED", "RUNNING", "RESTARTING":
		return true
	}
	return false
}
