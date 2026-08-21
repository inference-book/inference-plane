package provisioners_test

import (
	"context"
	"strconv"
	"strings"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
)

// spanReq asks for one member assembled from `nodes` machines.
func spanReq(id string, nodes int32) *provisionerv1.CreateDeploymentRequest {
	return &provisionerv1.CreateDeploymentRequest{
		Deployment: &provisionerv1.Deployment{
			Id: id, Image: "vllm/vllm-openai:v0.7.0", Model: "zai-org/GLM-5.2", EnginePort: 8000,
		},
		ReplicasSpec: []*provisionerv1.ReplicaSpec{{
			Provider:     "mockfan",
			Requirements: &provisionerv1.ResourceRequirements{Class: provisioners.GPUClassSmall, GpuCount: 8},
			Replicas:     1,
			Nodes:        nodes,
		}},
		Wait: true,
	}
}

// The capability #212 exists for: rent K machines, hand them to one
// engine, track the result as one member. Eight cards is not enough for
// a 1.5 TB model and four boxes of eight is, so the deployment has to be
// able to say "four machines, one engine".
func TestAMemberCanSpanSeveralMachines(t *testing.T) {
	svc, store := fanOutMultiReplicaSvc(t, nil)

	resp, err := svc.CreateDeployment(context.Background(), spanReq("k3", 4))
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}

	dep := resp.GetDeployment()
	if dep.GetState() != provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING {
		t.Errorf("state = %s, want RUNNING", dep.GetState())
	}
	if got := len(dep.GetReplicas()); got != 1 {
		t.Fatalf("members = %d, want 1: four machines is one engine, not four", got)
	}
	if got := dep.GetReplicas()[0].GetInstanceIds(); len(got) != 4 {
		t.Errorf("member spans %v, want four machines", got)
	}
	// One endpoint over the set, which is the whole point.
	if ep := dep.GetReplicas()[0].GetEngineEndpoint(); ep == "" {
		t.Error("member has no endpoint")
	}
	if got := provisioners.EffectiveEndpoints(dep); len(got) != 1 {
		t.Errorf("endpoints = %v, want one", got)
	}
	// And every machine is recorded, because every one of them bills.
	f, _ := store.Read()
	for _, id := range provisioners.EffectiveInstanceIDs(dep) {
		if _, ok := f.Instances[id]; !ok {
			t.Errorf("machine %q is in the member and not in the state file, so nothing can destroy it", id)
		}
	}
}

// The ticket's actual difficulty, and the money half of it. Three of
// four nodes is not a degraded member, it is a failed member holding
// three rentals that will bill until somebody notices. The fan-out is
// deliberately partial-tolerant ACROSS members and must not be WITHIN
// one.
//
// The failing node creates its pod and then never serves, which is the
// realistic and expensive shape: the machine exists at the provider, so
// something has to hand it back.
func TestAMemberThatLosesANodeReturnsTheWholeSpan(t *testing.T) {
	prov := &fanOutMockProvider{name: "mockfan"}
	prov.deployFn = func(inst *provisionerv1.Instance, emit func(provisioners.DeployStateUpdate)) error {
		emit(provisioners.DeployStateUpdate{
			State:       provisionerv1.DeploymentState_DEPLOYMENT_STATE_CONFIGURING,
			ContainerID: "pod-" + inst.GetId(),
		})
		if strings.HasSuffix(inst.GetId(), "-n2") {
			return testErr("engine never answered /health")
		}
		emit(provisioners.DeployStateUpdate{
			State:          provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
			EngineEndpoint: "http://" + inst.GetId() + ":8000",
		})
		return nil
	}
	svc, store := fanOutSvcWith(t, prov)

	resp, err := svc.CreateDeployment(context.Background(), spanReq("k3", 4))
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}

	dep := resp.GetDeployment()
	if got := dep.GetState(); got != provisionerv1.DeploymentState_DEPLOYMENT_STATE_FAILED {
		t.Errorf("state = %s, want FAILED: a member missing a node is not a member", got)
	}
	if !strings.Contains(dep.GetFailureReason(), "whole span was returned") {
		t.Errorf("failure_reason = %q, want it to say the span went back", dep.GetFailureReason())
	}
	// Every machine of the span handed back at the provider, including
	// the three whose engines were fine. Leaving them is the leak.
	if len(prov.terminated) != 4 {
		t.Errorf("terminated %v, want all four pods of the span", prov.terminated)
	}
	f, _ := store.Read()
	for _, id := range []string{"k3-n0", "k3-n1", "k3-n2", "k3-n3"} {
		inst, ok := f.Instances[id]
		if !ok {
			t.Errorf("%s has no record, so nothing could have returned it", id)
			continue
		}
		if inst.GetState() != provisionerv1.InstanceState_INSTANCE_STATE_TERMINATED {
			t.Errorf("%s is %s, want TERMINATED", id, inst.GetState())
		}
	}
}

// A member serves on its rank-0 node's address. Every node reports an
// endpoint and nothing routes to the workers, so taking the last writer
// would hand traffic to one of them.
func TestAMemberServesOnItsPrimarysEndpoint(t *testing.T) {
	svc, _ := fanOutMultiReplicaSvc(t, nil)

	resp, err := svc.CreateDeployment(context.Background(), spanReq("k3", 4))
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}

	eps := provisioners.EffectiveEndpoints(resp.GetDeployment())
	if len(eps) != 1 {
		t.Fatalf("endpoints = %v, want one for the one member", eps)
	}
	if !strings.Contains(eps[0], "k3-n0") {
		t.Errorf("member serves on %q, want the rank-0 node's address", eps[0])
	}
}

// Across members the old tolerance stands: five independent replicas
// losing one is DEGRADED and still serving, which is the distinction
// this whole model exists to draw.
func TestLosingOneOfSeveralSingleNodeMembersIsStillDegraded(t *testing.T) {
	svc, _ := fanOutMultiReplicaSvc(t, func(inst *provisionerv1.Instance, emit func(provisioners.DeployStateUpdate)) error {
		if inst.GetId() == "part-r1" {
			return testErr("simulated failure")
		}
		emit(provisioners.DeployStateUpdate{
			State:          provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
			EngineEndpoint: "http://" + inst.GetId() + ":8000",
		})
		return nil
	})

	resp, err := svc.CreateDeployment(context.Background(), multiReplicaCreateReq("part", 3))
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	if got := resp.GetDeployment().GetState(); got != provisionerv1.DeploymentState_DEPLOYMENT_STATE_DEGRADED {
		t.Errorf("state = %s, want DEGRADED", got)
	}
}

// What a node is told about itself is the one thing that decides whether
// a span can become an engine. A multi-node engine runs the same image
// everywhere with a different job per node: rank 0 serves, the rest join
// it. The image can only tell which it is from IPLANE_NODE_INDEX.
//
// Shipped wrong in the first cut: every node of a member was handed the
// member's slot, so a four-node member told four machines they were node
// zero. An entrypoint branching on it would have started four heads, and
// the engine registry, which builds Engine.span by node index, would have
// deduped the span back down to one.
func TestEveryNodeOfASpanIsToldItsRank(t *testing.T) {
	prov := &fanOutMockProvider{name: "mockfan"}
	svc, _ := fanOutSvcWith(t, prov)

	if _, err := svc.CreateDeployment(context.Background(), spanReq("k3", 4)); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}

	for want, id := range []string{"k3-n0", "k3-n1", "k3-n2", "k3-n3"} {
		env, ok := prov.envSeen[id]
		if !ok {
			t.Errorf("%s was never deployed", id)
			continue
		}
		if got := env["IPLANE_NODE_INDEX"]; got != strconv.Itoa(want) {
			t.Errorf("%s was told IPLANE_NODE_INDEX=%q, want %q", id, got, strconv.Itoa(want))
		}
	}
}

// A fan-out of independent replicas is N engines of one node each, so
// every one of them is its own rank 0. The member's position in the
// deployment is not a rank within an engine, and conflating them is what
// the bug above was.
func TestEachSingleNodeReplicaIsItsOwnRankZero(t *testing.T) {
	prov := &fanOutMockProvider{name: "mockfan"}
	svc, _ := fanOutSvcWith(t, prov)

	if _, err := svc.CreateDeployment(context.Background(), multiReplicaCreateReq("fan", 3)); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}

	for _, id := range []string{"fan-r0", "fan-r1", "fan-r2"} {
		if got := prov.envSeen[id]["IPLANE_NODE_INDEX"]; got != "0" {
			t.Errorf("%s was told IPLANE_NODE_INDEX=%q, want 0: it is a whole engine, not a rank", id, got)
		}
	}
}
