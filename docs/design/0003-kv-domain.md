# 0003 — KV Domain

**Status:** Proposed
**Phase:** v0.3, Ch 10 (Multi-instance fleet + multi-GPU KV domain)
**Depends on:** ROADMAP.md (v0.3 Ch 10), [0001-provisioner.md](0001-provisioner.md) (the "per-instance, not per-cluster" boundary this doc finally crosses), [0002-deploy.md](0002-deploy.md) (Deployment as placement; model-serving as a pluggable capability), CONSTRAINTS.md (CP/DP-1)
**Blocks:** Ch 10 implementation. Soft-depends on scale-down (#145) for the capacity contract's shrink path.

> **Partly superseded by [0006](0006-ch10-provider-reality-and-control-channel.md)** (2026-08-09).
> Two things in this doc did not survive contact with the providers.
> Its claim that RunPod Instant Clusters can back `GroupProvisioner` is wrong:
> the product exists, the API does not, so no adapter can rent a coherent
> multi-node pool from code today. And its `enum Interconnect { NONE, RDMA,
> NVLINK }` is replaced by `FabricScope { NONE, INTRA_NODE, INTER_NODE }` plus
> a bandwidth floor, because NVLink is one vendor's intra-node link while RDMA
> is a cross-node access semantic, and one field cannot answer both. The
> `FabricScope` form shipped in PRs 219 and 221. The rest of this doc, the
> KV-domain shape and the `DisaggregationRuntime` seam, stands as written.
> `Phase:` below should read v0.2, not v0.3; ROADMAP moved Ch 10 to v0.2.

## Why this doc exists

Prefill/decode disaggregation is the dominant production architecture for serving large models (DistServe, Splitwise, Mooncake, NVIDIA Dynamo, SGLang-PD). The prompt phase is compute-bound and the token phase is memory-bound; running them on one GPU makes them interfere, so production stacks split them onto separate GPU pools connected by an RDMA fabric that ships the KV cache across. This happens *inside* a disaggregation runtime, over a pool of GPUs that share one fabric. We call that pool a **KV domain**.

The question Ch 10 answers is: what does a control plane that manages N engines control about this, and what does it deliberately not touch? The answer this doc locks: **iplane is the finder and manager of the GPUs a runtime consumes, not the runtime.** It provisions a topology-bound RDMA pool, hands it to a runtime (Dynamo, SGLang-PD, llm-d), and manages the pool's lifecycle and size. The runtime owns everything inside the domain: prefill/decode roles, KV-aware routing, KV transfer, batching. This is a control-path role. iplane's output is a *shaped engine*; by the time a request arrives, the pool is already the right size, on the right fabric, with the right prefill:decode split.

This is the concretization of three things ARCHITECTURE.md and the roadmap already named but did not build: the router forwarding to engines "as opaque KV domains," the `ReplicaSpec` comment's future `GroupProvisioner` capability, and Ch 10's `--needs-rdma / --needs-nvlink` provisioning. No Go lands in this PR; Ch 10 implements against the shape here.

**Why not just build a disaggregation runtime.** Because Dynamo already is one, open source, and rebuilding it is a losing game. Verified as of mid-2026: Dynamo's control plane is officially NVIDIA-only (Ampere → Blackwell); AMD/ROCm support is experimental at the NIXL transfer layer (import-level since v1.2.1, June 2026) and lacks the disaggregation features; TPU/Trainium/Intel/Ascend have none. The runtimes are diverging by vendor: NVIDIA → Dynamo, AMD → llm-d (Red Hat aligned). Two single-vendor runtimes is exactly why a **vendor-neutral GPU-pool manager underneath both** is defensible, and it is a layer Dynamo structurally will not occupy. iplane adopts the runtime; it does not reimplement it (see ARCHITECTURE.md "own vs adopt vs pluggable").

## Decisions

### The Deployment stays the unit; a KV domain is a Deployment made opaque

We do not add a new top-level primitive. A KV domain is a Deployment whose replicas are no longer interchangeable. In a plain multi-replica Deployment (Ch 7), `instance_ids[i]` / `engine_endpoints[i]` are N equivalent replicas the router round-robins. In a KV-domain Deployment they are the pool's *internal* workers (prefill + decode), which are not interchangeable and which the router must never round-robin over. The runtime exposes one ingress; the router forwards there.

So the change is a marker on Deployment plus one router branch, not a parallel type hierarchy. The parallel-arrays shape (`instance_ids` / `engine_endpoints` / `replica_specs` / `unhealthy_instance_ids`) is reused verbatim for provisioning and health bookkeeping of the internal workers.

### `GroupProvisioner`: an optional Provider capability

Today every replica is an independent `Spawn`; there is no way to say "these N instances must share a fabric." A KV domain requires exactly that, because zero-copy KV transfer only works within one RDMA/NVLink domain, and N independent `Spawn` calls give no co-location guarantee. The `ReplicaSpec` comment already anticipated this seam ("one ReplicaSpec entry with replicas=N is literally a fleet description that can map to AWS ASGs / GCP MIGs / RunPod fleet APIs ... no single cloud's group primitive spans clouds").

```go
// GroupProvisioner is the optional Provider capability for renting a
// *coherent* group of instances on one fabric (an RDMA/NVLink domain),
// rather than N independent Spawns. A KV domain needs this: zero-copy KV
// transfer only works if the members share a fabric, which iplane cannot
// guarantee by issuing N separate Spawn calls. Providers that expose a
// cluster/fleet primitive (RunPod Instant Clusters, AWS EFA + placement
// groups, GCP MIG + compact placement) implement it; others do not, and a
// KV domain cannot be provisioned there. v0.3 ch10.
type GroupProvisioner interface {
    Provider
    // SpawnGroup rents count instances of spec, guaranteed co-located on one
    // Interconnect fabric, returned with a shared domain id. All-or-nothing:
    // a half-placed fabric is useless, so a partial rent rolls back before
    // returning.
    SpawnGroup(ctx context.Context, spec *provisionerv1.Spec, count int) (*provisionerv1.InstanceGroup, error)
}
```

Optional-capability idiom, matching `Deployer` in `internal/provisioners/deploy.go`: adapters opt in without changing the base `Provider` interface. A provider that cannot rent a coherent fabric simply does not implement it, and a KV-domain Deployment on that provider is rejected at create time with a clear error. This is a genuinely harder capability than renting N pods, and it is the real engineering cost of the direction.

The name is a cross-provider seam, so it stays generic (`GroupProvisioner`, not a provider-specific name); provider specifics (Instant Clusters, EFA, MIG) live only in the adapters.

### `DisaggregationRuntime`: pluggable, parallel to the engine abstraction

The engine abstraction (`internal/backends.Backend`: `Generate` / `Health` / `Name`) is the model for this. The runtime is the "model server" layer ARCHITECTURE.md keeps pluggable. iplane never implements PD; adapters launch or attach a runtime and report its ingress.

```go
// DisaggregationRuntime turns a provisioned KV-domain pool into a serving
// endpoint. The "model server" layer ARCHITECTURE.md keeps pluggable. iplane
// never reimplements one: adapters launch/attach a runtime (NVIDIA Dynamo,
// SGLang-PD, llm-d) and report the ingress. Dispatched by name through a
// registry, the same way `provider` dispatches through the provisioner
// registry (internal/runtimes). v0.3 ch10.
type DisaggregationRuntime interface {
    Name() string
    // Launch shapes the runtime over an already-provisioned pool and returns
    // its single ingress URL. iplane provisioned the fabric; the runtime
    // assigns prefill/decode roles and wires KV transfer.
    Launch(ctx context.Context, pool *provisionerv1.InstanceGroup, cfg RuntimeConfig) (ingress string, err error)
    Health(ctx context.Context) error
    Teardown(ctx context.Context, pool *provisionerv1.InstanceGroup) error
}
```

**The teachable MVP is an `external` runtime**, mirroring the external *provider*'s intentionally hollow lifecycle (see the external-provider package doc). The operator runs Dynamo or SGLang-PD themselves and gives iplane the ingress URL; iplane provisions the RDMA pool via `GroupProvisioner` and attaches. This exercises iplane's actual value (fabric-bound provisioning + cross-domain fleet management) without reimplementing a runtime. Adapters that truly launch a runtime (`dynamo`, `sglangpd`) come after.

Runtime names are dispatched through a registry; the generic seam is `DisaggregationRuntime`, and `dynamo` / `sglangpd` / `llmd` specificity lives only in each adapter.

### Data model additions

Per-instance fabric constraint, matching the roadmapped `--needs-rdma / --needs-nvlink`. This is a property of the instance (what NICs/fabric it needs), so it belongs on `ResourceRequirements`, which is already per-instance.

```proto
// Interconnect is the network fabric an instance's GPUs need. RDMA and NVLINK
// are what make a KV domain possible: zero-copy KV transfer between prefill and
// decode workers rides the fabric. UNSPECIFIED/NONE is the Ch 6-9 default
// (independent replicas over TCP, no shared KV). NVLINK/RDMA are fabric types,
// not provider names, so they belong in this generic enum. v0.3 ch10.
enum Interconnect {
  INTERCONNECT_UNSPECIFIED = 0;
  INTERCONNECT_NONE        = 1; // explicit "TCP is fine"
  INTERCONNECT_RDMA        = 2; // InfiniBand / RoCE, cross-node KV transfer
  INTERCONNECT_NVLINK      = 3; // intra-node NVLink domain
}

// added to ResourceRequirements (per-instance, like the rest of the message):
  // interconnect constrains SKU selection to instances wired for the named
  // fabric. A KV domain sets this on every member at provision time so the
  // pool lands in one RDMA/NVLink domain. v0.3 ch10.
  Interconnect interconnect = 7;
```

A role tag on `ReplicaSpec`. This is the subtle one, and it must be justified against the priority precedent (Deployment field 22 was a request-level priority that got removed because "priority is request-traffic-level, not artifact-level"). **Role is not traffic; it is capacity.** `PREFILL` vs `DECODE` describes what an instance group *is* (which hardware, which function in the pool), not how a request flowing through it should be treated. The per-request P/D decision stays inside the runtime. So role belongs on the runtime artifact; priority did not.

```proto
enum WorkerRole {
  WORKER_ROLE_UNSPECIFIED = 0; // plain replica, not part of a KV domain
  WORKER_ROLE_PREFILL     = 1; // compute-bound, newest GPUs
  WORKER_ROLE_DECODE      = 2; // memory-bound, cheaper GPUs
}

// added to ReplicaSpec:
  // role tags this instance group's function inside a KV domain. Capacity, not
  // traffic: it describes the group's hardware/function, not per-request
  // policy (contrast the removed default_priority at Deployment slot 22). The
  // runtime's planner drives the prefill:decode ratio by asking iplane to
  // scale groups by role; iplane fulfills, it does not choose the ratio. v0.3 ch10.
  WorkerRole role = 6;
```

The KV-domain marker on `Deployment`, reusing free slot 22 (the removed `default_priority`; pre-publication, no `reserved` marker is warranted, the slot is simply free).

```proto
// KVDomain marks a Deployment as one disaggregated runtime over a topology-bound
// pool, rather than N interchangeable replicas. This is the "opaque KV domain"
// ARCHITECTURE.md says the router forwards to (v0.3 ch10). When set:
//   - instance_ids / engine_endpoints describe the pool's *internal* workers
//     (prefill + decode). They are NOT round-robin routing targets; prefill and
//     decode workers are not interchangeable. The router forwards to
//     ingress_endpoint only.
//   - the runtime owns intra-domain routing, KV transfer, and the P:D split.
//     iplane owns provisioning the fabric-bound pool and resizing it on request.
// Absent (the Ch 6-9 default) means a plain multi-replica Deployment.
message KVDomain {
  // Which disaggregation runtime drives this pool. Dispatched through the
  // runtime registry (internal/runtimes) like `provider` dispatches through the
  // provisioner registry. "external" attaches to an operator-run frontend.
  string runtime = 1;
  // The runtime's single OpenAI-compatible frontend URL, the only endpoint the
  // router forwards to. Distinct from Deployment.engine_endpoints (the pool's
  // internal workers). Filled when the runtime reports ready.
  string ingress_endpoint = 2;
  // Fabric all members share. Mirrored onto each member's
  // ResourceRequirements.interconnect at provision time. A KV domain cannot
  // span providers (one fabric = one provider's cluster).
  Interconnect interconnect = 3;
}

// added to Deployment, slot 22 (was the removed default_priority; free pre-pub):
  // kv_domain, when set, marks this Deployment as a single disaggregated runtime
  // over a topology-bound pool. v0.3 ch10.
  KVDomain kv_domain = 22;
```

### The router branch (CP/DP-1 preserved)

`effectiveEndpoints` gains a KV-domain case: when `kv_domain` is set it returns the single `ingress_endpoint`, so the domain presents to the fleet router as one logical replica. The plural `engine_endpoints` remain for provisioning and health but are never routed over.

```go
func effectiveEndpoints(dep *provisionerv1.Deployment) []string {
    if kd := dep.GetKvDomain(); kd != nil { // opaque KV domain: one ingress
        if ing := kd.GetIngressEndpoint(); ing != "" {
            return []string{ing}
        }
        return nil
    }
    if eps := dep.GetEngineEndpoints(); len(eps) > 0 { // plain multi-replica
        return eps
    }
    if ep := dep.GetEngineEndpoint(); ep != "" {
        return []string{ep}
    }
    return nil
}
```

Everything downstream (`pickReplica`, `eligibleReplicas`, `hasStampedEndpoint`) consumes `effectiveEndpoints`, so it works unchanged. The router still reads deployment state only via the gRPC client (CP/DP-1 holds); nothing about a KV domain puts iplane on the token path. If anything, iplane is *more* off the hot path here: it forwards to one ingress and the runtime does all intra-domain routing.

### The capacity contract: where the runtime's planner boundary lands

The runtime's planner (Dynamo has one) decides the prefill:decode ratio. That is the runtime's job. When it wants to change the split, it calls iplane's existing `ScaleDeployment` with `add_replicas` carrying a role, and iplane's `GroupProvisioner` grows or shrinks the pool on the same fabric and re-hands it.

```
runtime planner:  "this domain needs +2 DECODE workers"
      |  ScaleDeployment{ add_replicas: [{ role: DECODE,
      |                                     requirements{ interconnect: RDMA },
      |                                     replicas: 2 }] }
      v
iplane:  SpawnGroup(+2 on the domain's fabric)  ->  runtime re-reads pool, rebalances roles
```

| iplane owns (control path)                          | runtime owns (inside the domain)          |
| --------------------------------------------------- | ----------------------------------------- |
| find + rent fabric-bound GPUs (`GroupProvisioner`)  | prefill:decode ratio decision (planner)   |
| place them in one RDMA/NVLink domain                | role assignment to workers                |
| maintain health, resize on request (`Scale`)        | KV-aware intra-domain routing             |
| one ingress into the fleet router                   | KV transfer (NIXL / Mooncake)             |
| cross-domain routing, admission, fairness           | batching, the token path                  |

iplane fulfills capacity requests; it never chooses the ratio. That single seam is what keeps the two planes separable. It soft-depends on role-scoped scale-down (#145) for the shrink direction.

## Consequences to decide before implementing

1. **A KV domain cannot span providers.** One fabric = one provider's cluster product. Multi-provider heterogeneity lives *across* domains, not within one; within a domain, "heterogeneous" means mixed SKUs on the same fabric (Splitwise's H100-prefill + A100-decode). This matches the `ReplicaSpec` comment ("no single cloud's group primitive spans clouds") and refines "heterogeneous fleets are the differentiator": the differentiator is heterogeneity *across* domains and mixed-SKU-within-fabric, not cross-provider inside a KV domain.

2. **KV-domain Deployments step out of the Ch 7-8 data-path story.** The router forwards to one ingress; prefix affinity and fair queueing apply to *plain* deployments, while the runtime owns intra-domain locality. Ch 7's title is "Routing, Queueing, and the Control Plane in the Data Path"; be deliberate that this class of deployment is the case where the control plane deliberately steps *back out* of the data path, and let that contrast teach.

3. **Not every provider can offer `GroupProvisioner`.** RunPod Instant Clusters, AWS EFA + placement groups, GCP compact placement can; scattered spot pods cannot. The create path must fail clearly when a KV-domain Deployment names a provider without the capability.

4. **`DisaggregationRuntime` adapters must stay thin.** Launch/attach + report ingress + teardown, like the external provider's hollow lifecycle. The moment an adapter starts reimplementing routing or KV transfer, the layering is lost and iplane is competing with Dynamo instead of managing GPUs for it.

## What this reuses vs. adds

Reuses: the parallel-arrays Deployment shape, the `ReplicaSpec` instance-group seam (where `GroupProvisioner` was already named), the roadmapped `--needs-rdma` flag, the external-provider attach idiom, `ScaleDeployment`, the optional-capability pattern (`Deployer`), and the `effectiveEndpoints` fallback.

Adds: the `Interconnect` enum + `ResourceRequirements.interconnect`, `WorkerRole` + `ReplicaSpec.role`, the `KVDomain` message + `Deployment.kv_domain` (slot 22), the `GroupProvisioner` capability, the `DisaggregationRuntime` registry (`internal/runtimes`), and one router branch.
