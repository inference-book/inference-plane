package cmd

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// deploymentDescriber and instanceDescriber are the two reads the sweep
// needs to describe the hardware it measured.
//
// Narrower than the deploymentClient / provisionerClient interfaces the
// lifecycle verbs use, because provenance is a read and a fake for two
// methods beats a fake for twelve.
type deploymentDescriber interface {
	DescribeDeployment(context.Context, *provisionerv1.DescribeDeploymentRequest) (*provisionerv1.DescribeDeploymentResponse, error)
}

type instanceDescriber interface {
	DescribeInstance(context.Context, *provisionerv1.DescribeInstanceRequest) (*provisionerv1.DescribeInstanceResponse, error)
}

// fleetProvenance is what the control plane can say about the hardware a
// sweep ran against. Every field is empty or zero when nobody asked, or
// when the answer could not be had.
type fleetProvenance struct {
	Provider string `json:"provider,omitempty"`
	GPUSKU   string `json:"gpu_sku,omitempty"`
	GPUCount int32  `json:"gpu_count,omitempty"`
	Replicas int    `json:"replicas,omitempty"`
	Plan     string `json:"plan,omitempty"`
}

// describeFleet reads a deployment and the instances behind it so a figure
// can name the hardware without an operator retyping it.
//
// One DescribeDeployment plus one DescribeInstance per replica, run once
// before the ladder starts rather than per level. The cost is bounded by
// the replica count and paid while nothing is being measured.
//
// Fields the fleet disagrees on are joined rather than collapsed to the
// first answer. A deployment spanning two providers is the case iplane
// exists to handle, and reporting one of them would describe a run that
// did not happen.
func describeFleet(ctx context.Context, dc deploymentDescriber, ic instanceDescriber, deployID string) (fleetProvenance, error) {
	var out fleetProvenance

	dep, err := dc.DescribeDeployment(ctx, &provisionerv1.DescribeDeploymentRequest{Id: deployID})
	if err != nil {
		return out, fmt.Errorf("describe deployment %q: %w", deployID, err)
	}
	d := dep.GetDeployment()
	out.Plan = planFromEngineArgs(d.GetEngineArgs())

	var ids []string
	for _, r := range d.GetReplicas() {
		ids = append(ids, r.GetInstanceIds()...)
	}
	if len(ids) == 0 {
		// v0.1-shaped records carry the singular field only. Same
		// fallback the router's effective helpers make.
		if id := d.GetInstanceId(); id != "" {
			ids = []string{id}
		}
	}
	out.Replicas = len(ids)

	var providers, skus []string
	for _, id := range ids {
		resp, err := ic.DescribeInstance(ctx, &provisionerv1.DescribeInstanceRequest{Id: id})
		if err != nil {
			return out, fmt.Errorf("describe instance %q: %w", id, err)
		}
		inst := resp.GetInstance()
		if p := inst.GetProvider(); p != "" {
			providers = append(providers, p)
		}
		if hw := inst.GetHardware(); hw != nil {
			if s := hw.GetGpuSku(); s != "" {
				skus = append(skus, s)
			}
			out.GPUCount += hw.GetGpuCount()
		}
	}
	out.Provider = joinDistinct(providers)
	out.GPUSKU = joinDistinct(skus)
	return out, nil
}

// joinDistinct renders a fleet-wide value: the single answer when every
// replica agrees, and every distinct answer joined by "+" when they do
// not. Sorted so two runs on the same fleet produce the same string.
func joinDistinct(vals []string) string {
	seen := map[string]bool{}
	var out []string
	for _, v := range vals {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Strings(out)
	return strings.Join(out, "+")
}

// planFromEngineArgs reads the parallelism split back off the arguments
// the deploy path built, rendering it as the shorthand a figure caption
// uses ("tp4", "tp4pp2").
//
// Reads the arguments rather than a stored plan because there is no
// stored plan. ValidateParallelism turns the requested split into engine
// flags and the flags are what the record keeps, so this is where the
// answer lives. Returns empty for a deployment that asked for no split,
// which is one way and not zero ways.
func planFromEngineArgs(args []string) string {
	tp := parallelArgValue(args, "--tensor-parallel-size")
	pp := parallelArgValue(args, "--pipeline-parallel-size")
	var b strings.Builder
	if tp > 1 {
		fmt.Fprintf(&b, "tp%d", tp)
	}
	if pp > 1 {
		fmt.Fprintf(&b, "pp%d", pp)
	}
	return b.String()
}

// parallelArgValue finds one flag's integer value, accepting both the
// "--flag N" and "--flag=N" spellings since nothing guarantees which one
// a deployment's arguments were built with.
func parallelArgValue(args []string, flag string) int {
	for i, a := range args {
		switch {
		case a == flag && i+1 < len(args):
			if n, err := strconv.Atoi(args[i+1]); err == nil {
				return n
			}
		case strings.HasPrefix(a, flag+"="):
			if n, err := strconv.Atoi(strings.TrimPrefix(a, flag+"=")); err == nil {
				return n
			}
		}
	}
	return 0
}
