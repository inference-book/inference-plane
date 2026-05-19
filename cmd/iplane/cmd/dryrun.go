package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"connectrpc.com/connect"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/runpod"
)

// --dry-run lives in the CLI layer per design doc 0001-provisioner.md
// (§"CLI: dry-run", line 211): the Provisioner interface gains no
// dry-run method. The CLI:
//
//   1. Validates and expands the spec (catches user errors before
//      any work would happen on a real run).
//   2. Reads the state file (or the server's state via Describe in
//      remote mode) to detect the idempotent case.
//   3. For providers that publish a static catalog (runpod), resolves
//      the constraints to a cheapest SKU + estimated price.
//   4. Prints a `[dry-run] would ...` line and exits 0. Zero provider
//      calls, zero state-file writes.

// dryRunCreate is the create-verb dry-run path. Mirrors the logic the
// Service goes through (validate -> idempotency lookup -> spawn) but
// stops at "would Spawn" and prints what would happen.
func dryRunCreate(ctx context.Context, w io.Writer, client provisionerClient, spec *provisionerv1.Spec) error {
	if err := provisioners.ValidateID(spec.GetId()); err != nil {
		return err
	}
	if err := provisioners.ValidateAndExpandRequirements(spec); err != nil {
		return err
	}

	// Idempotency check: ask the server (via Describe, source=local
	// in both transports) whether this id already has a record. If
	// PENDING/ACTIVE -> no-op; if TERMINATED/FAILED or NotFound,
	// fall through to "would create."
	descCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, err := client.DescribeInstance(descCtx, connect.NewRequest(&provisionerv1.DescribeInstanceRequest{
		Id:     spec.GetId(),
		Source: provisionerv1.Source_SOURCE_LOCAL,
	}))
	switch {
	case err == nil:
		inst := resp.Msg.GetInstance()
		switch inst.GetState() {
		case provisionerv1.InstanceState_INSTANCE_STATE_PENDING,
			provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE:
			fmt.Fprintf(w, "[dry-run] would no-op: %q already exists on %s (state=%s, provider_id=%s). idempotent re-create returns the existing record without a provider call.\n",
				inst.GetId(), inst.GetProvider(),
				instanceStateLabel(inst.GetState()), inst.GetProviderId())
			return nil
		}
		// TERMINATED / FAILED -- fall through. The id slot is reusable.
	case isNotFound(err):
		// No prior record. Fall through to "would create."
	default:
		// Anything else (network failure, auth failure) is a real
		// error that the operator wants to see before they trust
		// the dry-run output.
		return fmt.Errorf("dry-run lookup of %q: %w", spec.GetId(), err)
	}

	cost, costNote := projectedCost(spec)
	fmt.Fprintf(w, "[dry-run] would create %q on %s\n", spec.GetId(), spec.GetProvider())
	if region := spec.GetRegion(); region != "" {
		fmt.Fprintf(w, "[dry-run]   region:     %s\n", region)
	} else if spec.GetProvider() == provisioners.ProviderRunPod {
		fmt.Fprintf(w, "[dry-run]   region:     (unpinned -- runpod schedules wherever capacity exists)\n")
	}
	reqs := spec.GetRequirements()
	fmt.Fprintf(w, "[dry-run]   constraints: vram>=%dGB, ram>=%dGB, disk>=%dGB, gpus=%d\n",
		reqs.GetMinVramGb(), reqs.GetMinRamGb(), reqs.GetMinDiskGb(), maxInt32(reqs.GetGpuCount(), 1))
	fmt.Fprintf(w, "[dry-run]   est cost:   %s%s\n", cost, costNote)
	fmt.Fprintln(w, "[dry-run] no provider calls made, no state file changes.")
	return nil
}

// dryRunDestroy is the destroy-verb dry-run path. Looks up the record
// (via Describe, server-side or state-file depending on transport),
// then prints what would happen.
func dryRunDestroy(ctx context.Context, w io.Writer, client provisionerClient, id string) error {
	if err := provisioners.ValidateID(id); err != nil {
		return err
	}
	descCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, err := client.DescribeInstance(descCtx, connect.NewRequest(&provisionerv1.DescribeInstanceRequest{
		Id:     id,
		Source: provisionerv1.Source_SOURCE_LOCAL,
	}))
	if err != nil {
		if isNotFound(err) {
			return fmt.Errorf("no instance with id %q (nothing to destroy)", id)
		}
		return fmt.Errorf("dry-run lookup of %q: %w", id, err)
	}
	inst := resp.Msg.GetInstance()
	switch inst.GetState() {
	case provisionerv1.InstanceState_INSTANCE_STATE_TERMINATED:
		fmt.Fprintf(w, "[dry-run] would no-op: %q is already TERMINATED.\n", id)
		return nil
	}
	fmt.Fprintf(w, "[dry-run] would destroy %q on %s\n", id, inst.GetProvider())
	fmt.Fprintf(w, "[dry-run]   provider id: %s\n", inst.GetProviderId())
	fmt.Fprintf(w, "[dry-run]   from state:  %s\n", instanceStateLabel(inst.GetState()))
	fmt.Fprintln(w, "[dry-run] no provider calls made, no state file changes.")
	return nil
}

// projectedCost returns the estimated hourly rate string + an
// adjective explaining where it came from. For local, always $0
// (the laptop). For runpod, ask MatchSKUs and look up the cheapest
// hit's catalog price. The actual rate at Spawn time may differ --
// RunPod's live pricing can drift from our static catalog. That's
// fine for an estimate.
func projectedCost(spec *provisionerv1.Spec) (string, string) {
	switch spec.GetProvider() {
	case provisioners.ProviderLocal:
		return "$0.0000/hr", " (local; the laptop provisions itself)"
	case provisioners.ProviderRunPod:
		skus := runpod.MatchSKUs(spec.GetRequirements())
		if len(skus) == 0 {
			return "(no matching SKU)", " -- the catalog has nothing satisfying these constraints; tighten or try a higher class"
		}
		if entry := runpod.LookupSKU(skus[0]); entry != nil {
			return fmt.Sprintf("$%.4f/hr", entry.PriceUSDPerHour),
				fmt.Sprintf(" (cheapest match: %s; runpod's live price at spawn may differ)", entry.GpuTypeID)
		}
		return "(unknown)", ""
	default:
		return "(unknown)", " -- provider has no catalog for cost estimation"
	}
}

func maxInt32(a, b int32) int32 {
	if a > b {
		return a
	}
	return b
}

// isNotFound matches the wrapped ErrNotFound that the Service returns
// when a record is missing (both transports). Pulled out so create
// and destroy dry-run share the same matcher.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, provisioners.ErrNotFound) {
		return true
	}
	// Connect transport wraps the cause inside a connect.Error; check
	// the code as well.
	var ce *connect.Error
	if errors.As(err, &ce) && ce.Code() == connect.CodeNotFound {
		return true
	}
	return false
}
