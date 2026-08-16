package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
)

// autoProvider is the positional argument that asks iplane to choose the
// vendor instead of naming one.
//
// A word rather than a flag because it sits where a provider name goes, and
// the shape of the command should say that a choice is being delegated. It is
// also why the chosen provider is written back into the Spec: the state file
// records what was rented, never the instruction that led there.
const autoProvider = "auto"

// autoPlacementTimeout bounds the candidate sweep that precedes a create.
//
// Shorter than the create it precedes, because this is the free read-only part
// and an operator waiting to spend money should not be held up by one slow
// marketplace. A vendor that misses the window is reported as not having
// contributed rather than failing the placement.
const autoPlacementTimeout = 45 * time.Second

// resolveAutoPlacement picks the cheapest fitting candidate across every
// configured provider.
//
// Deliberately asks everyone rather than a named list. The operator who typed
// "auto" has said they do not want to choose, and quietly restricting the
// search to a default subset would answer a narrower question than the one
// they asked while looking like it answered theirs.
func resolveAutoPlacement(cmd *cobra.Command, id string) (*provisioners.Placement, error) {
	reqs := &provisionerv1.ResourceRequirements{
		MinVramGb: createMinVRAM,
		MinDiskGb: createMinDisk,
		MinRamGb:  createMinRAM,
		GpuCount:  createGPUCount,
		Class:     createClass,
		Sku:       createSKU,
	}
	fabricScope, err := parseFabricScope(createFabric)
	if err != nil {
		return nil, err
	}
	reqs.FabricScope = fabricScope
	reqs.MinFabricGbps = createFabricBW

	spec := &provisionerv1.Spec{Id: id, Requirements: reqs}
	if err := provisioners.ValidateAndExpandRequirements(spec); err != nil {
		return nil, err
	}

	svc, err := buildReadOnlyService()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), autoPlacementTimeout)
	defer cancel()

	placement, err := svc.SelectCheapest(ctx, nil, spec.GetRequirements())
	if err != nil {
		return nil, fmt.Errorf("--provider auto: %w", err)
	}
	return placement, nil
}
