# 0006 — Ch 10 provider reality and the engine control channel

**Status:** Findings + proposal (no code in this doc)
**Phase:** v0.2 Ch 10 (multi-GPU + distributed fleet), epic #211
**Depends on:** [0003-kv-domain.md](0003-kv-domain.md) (the `GroupProvisioner` / `Interconnect` shape this doc fact-checks), [0004-cross-provider-warm-cache.md](0004-cross-provider-warm-cache.md) (the same "probe the providers before scoping" method, and its big-cloud conclusion), CONSTRAINTS.md (CP/DP-1)
**Blocks:** #203 (interconnect metadata + filter), #204 (control channel), #212 (multi-node pool). Rescopes #212.

## Why this doc exists

Epic #211 lists two open questions that gate the whole chapter, and both were
being answered from assumption rather than measurement:

1. **Can our providers actually do this?** [0003](0003-kv-domain.md) asserts
   that "RunPod Instant Clusters, AWS EFA + placement groups, GCP compact
   placement" can back a `GroupProvisioner`. One of those three is wrong.
2. **What shape is the control channel?** #204 lists "long-lived stream vs
   periodic registration" as undecided, and #205, #213, #214 are all payload or
   presentation over whatever gets chosen.

This doc settles both from live probes taken 2026-08-09, the same way
[0004](0004-cross-provider-warm-cache.md) scoped the warm cache from probes
rather than docs. Where a provider claim here contradicts a vendor page, the
probe wins and the probe is quoted.

# Part 1. Provider findings (probed live 2026-08-09)

| provider | per-SKU interconnect metadata | multi-node group primitive | verdict for Ch 10 |
| --- | --- | --- | --- |
| **RunPod** | none in the API; form factor readable from the `gpuTypeId` string | Instant Clusters exist as a **product**, **no API** | fine for #203, **cannot back #212** |
| **Vast** | **`bw_nvlink`, measured GB/s, server-side filterable** | `cluster_id` present but sparse; renter-side path undocumented | **best available source** for #203, unproven for #212 |
| **Lambda** | none | 1-Click Clusters are sales-gated | out; **API key is dead** (see below) |
| **AWS / GCP** | EFA yes, NVLink **no** | real: placement groups, capacity blocks, MIGs | the only solid #212 home, no adapter |

## RunPod: the product exists, the API does not

`gpuTypes` (GraphQL v1) returns exactly `id`, `displayName`, `memoryInGb`,
`maxGpuCount`, `secureCloud`, `communityCloud`, `manufacturer`. There is no
fabric field of any kind, and no per-machine topology anywhere in the pod API.

What RunPod does give, for free, is **form factor encoded in the SKU id**:

```
NVIDIA A100 80GB PCIe      NVIDIA A100-SXM4-80GB      NVIDIA A100-SXM4-40GB
NVIDIA H100 PCIe           NVIDIA H100 80GB HBM3      NVIDIA H100 NVL
NVIDIA H200 NVL            NVIDIA H200                NVIDIA B200
NVIDIA B300 SXM6 AC        NVIDIA L40S                NVIDIA RTX 6000 Ada Generation
```

Every datacenter part names its form factor, and form factor determines the
intra-node fabric class: SXM sits on an NVSwitch mesh, NVL is a bridged pair,
PCIe is a PCIe root with at most an optional bridge. That is enough to build
#203's curated catalog for RunPod without inventing facts.

**Instant Clusters cannot be driven programmatically.** This is the finding that
changes the plan, and it is checked three ways:

- `rest.runpod.io/v1/openapi.json` lists every path the REST API serves:
  `pods`, `endpoints`, `templates`, `networkvolumes`, `containerregistryauth`,
  `billing`. Nothing cluster-shaped.
- `GET api.runpod.io/v2/clusters` returns a hard `404 {"detail":"The requested
  path was not found."}`. A widely-syndicated third-party article claims this
  endpoint accepts a cluster config POST. It does not exist.
- RunPod's own Instant Clusters guides describe console deployment and say
  nothing about an API or CLI path.

The product is real (2-8 nodes, 16-64 GPUs, 1600-3200 Gbps on B200/H200/H100/A100,
`ens1`-`ens8` inter-node interfaces, `NCCL_SOCKET_IFNAME=ens1`). It is simply
not reachable from code, which is the only thing a provider adapter can use.

**So [0003](0003-kv-domain.md) line 208 is wrong as written** and should be
corrected when that doc is next touched: RunPod cannot implement
`GroupProvisioner` today, not because it lacks the fabric but because it does
not expose the rental of one.

## Vast: the only provider that reports the fabric, and it reports it honestly

Vast's offer search carries `bw_nvlink` (measured GB/s), `pci_gen`, `gpu_lanes`,
`pcie_bw`, `machine_id`, `cluster_id`, `geolocation`. `bw_nvlink` is filterable
server-side, confirmed by running `bw_nvlink >= 100` and getting back only
NVLinked hosts.

Distribution over a 1000-offer sweep of rentable on-demand multi-GPU offers:

| gpu_name | zero | nonzero | reported GB/s |
| --- | ---: | ---: | --- |
| A100 SXM4 | 9 | 29 | 300 |
| A100 PCIE | 21 | 3 | 300, 275 |
| H100 SXM | 3 | 21 | 900, 478 |
| H100 NVL | 0 | 3 | 319 |
| H100 PCIE | 4 | 5 | 319 |
| H200 | 2 | 19 | 478 |
| B200 | 5 | 23 | 956, 900 |
| RTX A6000 | 5 | 2 | 56 |
| L40S | 18 | 0 | — |
| RTX 6000Ada | 13 | 0 | — |

Read that table carefully, because it is the whole argument for #203 and it is
better evidence than the chapter currently has.

- **It is right where it can be checked.** L40S and RTX 6000 Ada report zero
  across the board, correctly, since neither has NVLink. RTX A6000 reports 56,
  which is the real per-direction bridge bandwidth.
- **The SKU name lies in both directions.** 21 of 24 `A100 PCIE` offers report
  zero, but 3 report 275-300 GB/s, meaning an NVLink bridge is installed on a
  card whose name says PCIe. Meanwhile 9 of 38 `A100 SXM4` offers report zero on
  a board that is physically always NVLinked.
- **Therefore zero does not mean "no NVLink". It means "not measured".** About a
  quarter of SXM machines under-report. This is exactly #203's stated hazard,
  observed rather than predicted, and it is why the ticket's "unknown must not
  silently pass a `--needs-nvlink` filter" requirement is load-bearing.

`cluster_id` is populated on only 7 of 400 multi-GPU on-demand offers, and the
renter-side path for launching co-located instances is not in the reachable
docs (the cluster documentation is written for hosts registering machines they
own). Not disproven, but not something to plan #212 around without more work.

## Lambda: out, and the key is dead

`GET cloud.lambdalabs.com/api/v1/instance-types` now returns
`{"error":{"code":"global/invalid-api-key"}}`. `LAMBDA_API_KEY` worked during
the Ch 9 session on 2026-07-31 and has since expired or been revoked. Separately,
1-Click Clusters are sales-gated, so Lambda was never a Ch 10 vehicle. Worth
rotating the key regardless, since the Ch 9 heterogeneous-fleet story uses it.

## AWS/GCP: solid for the group, still silent on NVLink

`DescribeInstanceTypes` returns `gpuInfo`, `networkInfo` (carrying EFA support),
`placementGroupInfo`, and `SupportedUsageClasses` including `capacity-block`.
That is a genuine programmatic group primitive and a genuine first-class
cross-node fabric flag, which is what #212 needs and no marketplace offers.

But note what is **not** there: no NVLink field, no GPU peer-to-peer topology.
AWS tells you the instance type is `p5.48xlarge` and leaves you to know that
means 8x H100 SXM on NVSwitch. So the conclusion generalizes past our three
providers:

> No provider reports intra-node NVLink topology as a queryable spec field.
> Vast reports a *measurement*, which is a different thing and comes with a
> quarter of its readings missing. Every other provider requires a curated
> catalog keyed on SKU name.

That is a fact worth putting in the chapter, because it is the reason the
operator pain in #203 exists at all.

# Part 2. Where interconnect metadata comes from (#203)

#203 names three sourcing options and asks which we are doing. The answer is
that they are not alternatives, they are **tiers of confidence**, and the field
carries which tier it came from.

| tier | source | example | trust |
| --- | --- | --- | --- |
| `DECLARED` | curated catalog keyed on SKU name + form factor | RunPod `NVIDIA A100-SXM4-80GB` → NVLink | high for the card, silent on the host |
| `MEASURED` | provider-reported reading | Vast `bw_nvlink: 300.0` | highest when present |
| `UNKNOWN` | no catalog entry and no reading | Vast `bw_nvlink: 0.0` on an SXM part | **never satisfies a filter** |

Three rules fall out, and they are the reviewable content of the ticket:

1. **`UNKNOWN` fails closed.** `--needs-nvlink` rejects an offer whose fabric is
   unknown. The operator can opt back in explicitly, but the default cannot be
   "probably fine", because a silent pass is precisely the bill-before-answer
   failure the ticket exists to remove.
2. **A zero reading is `UNKNOWN`, not `NONE`.** The A100 SXM4 rows above are the
   proof. Distinguishing "measured zero" from "not measured" is not possible in
   Vast's payload, so both collapse to `UNKNOWN` when the catalog says the card
   should have had a link. When the catalog agrees the card has no NVLink
   (L40S, RTX 6000 Ada, consumer parts), zero is promoted to `NONE`.
3. **`MEASURED` outranks `DECLARED` when they disagree.** The bridged `A100
   PCIE` at 300 GB/s is a real machine that a name-only catalog would have
   wrongly excluded.

Also note the RunPod adapter already has the right home for the catalog:
`internal/provisioners/runpod/skus.go` maps `gpuTypeId` to a `SKUSpec` today.
The Vast adapter's offer struct currently reads only `gpu_name`, `num_gpus`,
`gpu_ram`, so picking up `bw_nvlink` is a small, contained change.

## The core proto describes fabrics, not vendors

[0003](0003-kv-domain.md) proposed `enum Interconnect { NONE, RDMA, NVLINK }`.
That enum is superseded here, because its two interesting values are not
siblings. **NVLink is one vendor's intra-node link. RDMA is a cross-node access
semantic.** One field answering two questions, with a vendor name baked into a
seam meant to outlive it.

What the operator is actually asking is never "NVLink". It is whether N cards
can move tensors between them fast enough, which decomposes into two
vendor-neutral properties: the **scope** of the fast path, and its
**bandwidth**. The technology name is something we discover, not something we
require.

```proto
// FabricScope is where an instance's fast interconnect reaches. Vendor-neutral
// on purpose: NVLink, AMD xGMI and Intel Xe Link are all INTRA_NODE, while
// InfiniBand, RoCE and AWS EFA are all INTER_NODE. The control plane requires a
// scope and a bandwidth; which technology delivers it is the adapter's business
// and never a branch in core logic.
enum FabricScope {
  FABRIC_SCOPE_UNSPECIFIED = 0;
  FABRIC_SCOPE_NONE        = 1; // PCIe / TCP is fine
  FABRIC_SCOPE_INTRA_NODE  = 2; // cards within one node share a fast link
  FABRIC_SCOPE_INTER_NODE  = 3; // nodes share a fast link
}
```

Requirement side, on `ResourceRequirements`: `fabric_scope` + `min_fabric_gbps`.
Observation side, on `Hardware`: `fabric_scope`, `fabric_gbps`, `fabric_source`
(the tier from the table above), and `string fabric_technology` carrying
`nvlink`, `nvswitch`, `xgmi`, `infiniband`, `roce`, `efa`. That last field is
descriptive only. Nothing in the control plane branches on it, and that rule is
what keeps it from quietly becoming a vendor enum again.

**This is not tidiness.** RunPod's live catalog includes `AMD Instinct MI300X
OAM`, 192 GB, 8 cards, today. An `INTERCONNECT_NVLINK` enum can never match it
even though xGMI is exactly the fabric a tensor-parallel group wants, so the
generalization buys a SKU we already have API access to rather than a
hypothetical one. It also dissolves an argument we would otherwise have about
AWS EFA, which uses SRD rather than standard RDMA verbs: `INTER_NODE` is true of
EFA without anyone having to litigate whether it counts as RDMA.

**The two scopes are not ordered, and it is tempting to think they are.**
Inter-node does not imply intra-node: a PCIe-only box behind InfiniBand has a
fast cross-node fabric and no fast link between its own cards. So a requirement
needing both at once needs two fields, not one enum value. We ship the single
`fabric_scope` now and add the second axis when it has a consumer, since the
only thing wanting both is a cross-node pool, which is #212, which Part 3
descopes. The field comment should record why it is one axis and what forces the
second.

At the CLI the canonical vocabulary is `--fabric intra-node|inter-node|none`
plus `--min-fabric-gbps`, with `--interconnect nvlink` and `--needs-rdma` kept
as documented aliases. Operators and the chapter both think in vendor terms, so
the sugar belongs at the human edge and the properties belong in the contract.

# Part 3. #212 is rescoped, not scheduled

A multi-node pool as one deployment member has no programmatic backing on
RunPod, an undocumented one on Vast, and no adapter on AWS/GCP. Building the
control-plane half against zero working providers would produce a seam nothing
can exercise, which is the opposite of how this repo has scoped every other
capability (see [0004](0004-cross-provider-warm-cache.md), which shipped
`VolumeManager` against the one provider that could carry it).

So:

- **#212 stays open and unscheduled**, blocked on a provider that can rent a
  coherent multi-node pool from code. That is the AWS/GCP adapter, already named
  as the top-priority next item in the Ch 9 handoff, where it converges with
  cross-provider warm cache and large-GPU capacity.
- **Everything else in the epic proceeds intra-node**, where a span of 2-8 cards
  on one node is real, rentable today, and enough to make the span column
  non-decorative. Ch 10's own worked example is deliberately intra-node (TP-4,
  four cards, one node), so the chapter does not lose its spine.
- **#203's `--needs-rdma` ships as a filter with no consumer yet.** That is
  acceptable and honest, and it should be said out loud in the ticket rather
  than discovered later.

The book consequence is worth naming: Ch 10 can teach cross-node pipeline
parallelism as a concept, and demonstrate intra-node tensor parallelism. It
cannot demonstrate a cross-node span until the big-cloud adapter lands. That is
a narrower claim than the epic implies, and better to fix in the prose now than
in errata.

# Part 4. The control channel is a lease, not a stream (#204)

**Proposal: periodic re-registration under a lease, unary, agent-initiated,
over the existing public Connect/HTTP surface.** Not a long-lived stream.

Four reasons, in descending order of how repo-specific they are:

1. **A long-lived stream walks straight into a bug we already shipped.** Ch 9
   established that `server.write_timeout_sec` severs long-running requests
   mid-flight; a slow `CreateDeployment` died with `unexpected EOF` for exactly
   this reason, and the fix was to align three separate timeouts. A control
   channel held open for the life of an engine is that same trap with no
   natural bound. A 15-second unary call never meets the timeout.
2. **A lease survives a control-plane restart without extra machinery.** Streams
   die with the process and the fleet view comes back empty until every agent
   notices and redials. With a lease, the CP restarts, agents re-register on
   their normal cadence, and the view refills within one lease period. That is a
   real operational property, not a stylistic preference.
3. **The detection window becomes an explicit number.** #204 wants detection
   time to be a property of the channel rather than of fleet size. A lease TTL
   is that number, written down and tunable. A stream's detection time is
   whatever TCP and the intervening NATs decide, and since a dropped stream must
   be given a grace period before declaring death anyway, the stream design
   converges on a lease with extra steps.
4. **It enforces #204's own guardrail structurally.** The ticket insists that
   push carries membership while pull and router-local state keep carrying
   anything the data path's correctness rides on, and that #189 must be able to
   layer on this transport without dragging routing state onto it. If membership
   rides a coarse lease and #189 later rides a separate tight-freshness
   mechanism, the guardrail is enforced by the two things being physically
   different channels with different budgets. Put both on one stream and the
   only thing stopping a well-meaning change from moving in-flight counts onto
   it is a comment.

**Transport.** The agent runs on a rented pod on the public internet, so it
cannot reach the gRPC server, which binds `127.0.0.1:9090` and is an in-process
implementation detail. It dials the public HTTP surface on `:8080`, which the
Connect adapter already serves. Agent-initiated, so pods behind NAT work with no
inbound path. This does not change CP/DP-1: the channel terminates in the
control plane, and the router keeps reading state through the generated gRPC
client.

**Payload**, per #204, and deliberately not more:

- group composition and per-node identity (the input #214 needs for attribution;
  RunPod's top-level `machineId` is the node identity, already understood from
  the Ch 9 phase-ladder work)
- lease renewal
- operational readings the engine computes anyway: in-flight, cache utilisation
- link health from #213

**States.** `assembling` (some cards joined, group not formed), `serving`,
`serving_degraded` (assembled, correct, slow), and `lost` (lease expired).
`assembling` and `serving_degraded` are the two with no single-card equivalent
and are the reason the probe cannot be stretched to cover this.

**Write path caution.** Registrations from N agents land concurrently and each
one is a read-modify-write against deployment state. `stores/file` needs its
in-process mutex for exactly this, and the lost-update bug that bit multi-replica
from-scratch deploys is the same shape. Do not call `store.Update`/`Read` from
inside an `Update` closure.

# Part 5. What this means for the build order

The epic's suggested order survives with one correction and one addition.

1. **#204 first**, unchanged. Provider-agnostic, and everything in job 2 hangs
   off it.
2. **#203 in parallel**, unchanged, and now better specified: two providers
   exercise two different sourcing tiers, which makes the ticket teach something
   a single-provider implementation would not.
3. **#205 and #213** once the channel carries state. #205 must resolve the #145
   drain-machinery overlap deliberately rather than implementing a second drain.
4. **#214 and #215** last.
5. **#212 leaves the sequence**, blocked on the AWS/GCP adapter (Part 3).

## Experiments: settled 2026-08-10

Two of the three ran live on one rented box (2x RTX A6000 on Vast, $0.639/hr,
about $0.10 total; nothing left running, verified against both provider APIs).
Full detail in [0007-gpu-validation-findings.md](0007-gpu-validation-findings.md).

- **Can NVML read link state inside a provider container? YES.** `nvidia-smi
  nvlink -s` returns per-link state and `-e` returns the replay / recovery / CRC
  counters, from inside the container with no special privileges. #213's
  load-bearing assumption holds and the sensor is buildable.
- **Is Vast's `bw_nvlink` per-machine or per-offer? Per-machine.** Across 800
  rentable multi-GPU offers spanning 624 machines, exactly one machine reported
  differing values across its own offers. Safe to reason about per-machine.
- **Vast renter-side co-located rental.** Still open. Whether `cluster_id` can
  rent two instances on one cluster from the API; if yes, #212 has a marketplace
  path and Part 3 gets revisited.

The same run gave the first real-hardware check of anything shipped for #203.
iplane stamped `INTRA_NODE / MEASURED / 449 Gbps / nvlink` on the A6000;
`nvidia-smi topo -m` reported `NV4` (four links) at 14.062 GB/s each, which is
56.25 GB/s, matching Vast's reading of 56.248 and our gigabit conversion to
within rounding. A third finding matters for later tickets: a box **cannot see
its own provider machine id from inside**, so #214's failure attribution needs
that identity injected at deploy time rather than discovered by the agent.

## Corrections this doc lands

- [0003](0003-kv-domain.md) line 208 lists RunPod Instant Clusters as able to
  back `GroupProvisioner`. It cannot, for lack of an API. 0003 also carries
  `Phase: v0.3, Ch 10`, while ROADMAP moved Ch 10 to v0.2. Both need a touch-up.
  0003 is currently untracked in git; it should be committed before anything
  links to it.
- [0003](0003-kv-domain.md)'s `enum Interconnect { NONE, RDMA, NVLINK }` is
  superseded by `FabricScope` (Part 2). Its two interesting values answer
  different questions and one of them is a vendor's product name. The
  `ResourceRequirements.interconnect` field 0003 proposed becomes
  `fabric_scope` + `min_fabric_gbps`, and `KVDomain.interconnect` follows the
  same rename when that message is built.
- `ROADMAP.md` line 66 says the engine's `gpu_prefix_cache_hit_rate` is "(now
  scraped)". It is not, and the same file contradicts it at line 106, which
  records the Ch 8 metric as "measured router-side as a proxy ... the engine
  metric itself is deferred to issue 51, not faked on mock". The `ch08-final`
  capability snapshot stands; line 66 is a stale parenthetical. This is the
  reconciliation epic #211 asks someone with the history to settle.
