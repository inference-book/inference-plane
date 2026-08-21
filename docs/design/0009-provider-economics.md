# 0009 — What renting for Part IV actually costs

**Status:** Findings (no code in this doc)
**Phase:** v0.4 Part IV, epic #361
**Depends on:** [0008-whole-node-rental.md](0008-whole-node-rental.md) (which settled that the request shape works)
**Feeds:** #358 and #360 planning; the book's Ch 13 and Ch 14

## What this is

A free audit across the three configured providers plus published
hyperscaler pricing, run to plan the GLM and Kimi rentals. Everything here
came from `iplane capacity`, provider APIs and public price pages. Nothing
was rented.

Prices are a snapshot from 2026-08-18 and will date. The ratios and the
mechanisms are the durable part.

## Capability matrix

| Provider | 8x >=80 GB | Fabric evidence | Mounts / warm cache | Adapter |
| --- | --- | --- | --- | --- |
| Vast | yes | measured per offer | **no** (#254) | yes |
| RunPod | intermittent | declared from SKU | **yes**, the only one | yes |
| Lambda | vendor yes, catalog no (#380) | declared | no | yes |
| AWS / GCP | yes | yes | yes | **none** |

No provider offers both reliable eight-card capacity and a warm cache.
RunPod is the only one that could, since it is the only `MountAttacher`.

## Price for eight cards

> The A100 rows below cannot run GLM-5.2. See "The cards have to be Hopper
> or newer" further down; the table is kept because the price spread across
> providers is the point of it.

| | card | $/hr |
| --- | --- | --- |
| Vast | 8x A100 80G | **10.67** |
| RunPod | 8x H200 141G | 28.72 |
| Lambda | 8x A100 80G | 22.32 |
| Lambda | 8x H100 SXM5 | 31.92 |
| AWS `p5.48xlarge` | 8x H100 | **55.04** |
| GCP `a3-highgpu-8g` | 8x H100 | **~88** |

Five to eight times between the marketplaces and the hyperscalers for the
same silicon.

## Capacity is perishable, and this is the measured version

Eight-card, 80GB-plus, sampled every fifteen minutes by
[`hack/capacity-sample.sh`](../../hack/capacity-sample.sh):

```
20:33Z   2 offers   vast @ $12.80
20:49Z   3 offers   vast @ $12.80, runpod @ $21.52
21:35Z   3 offers   vast @ $12.80, runpod @ $21.52
22:05Z   3 offers   vast @ $12.80
22:20Z   1 offer    vast @ $28.23
22:48Z   0 offers   nothing, anywhere
23:58Z   1 offer    runpod @ $11.12
```

Three and a half hours, and the pool went from three offers to none and
back to one. There was a window in which the GLM run could not have been
started at any price.

The width matters more than the card. On RunPod at one point: 36 of 48
types obtainable as a single GPU, 19 of 48 as four, 7 of 48 as eight, and
none of those seven with 80 GB or more. Availability is a property of a
card **at a width**, not of a card.

**Planning consequence.** Eight-card capacity is taken when it appears
rather than scheduled. Anything that must be booked ahead wants a
hyperscaler and the five-to-eight-times premium that comes with it.

## Ingress bandwidth decides whether staging is worth building

Vast publishes an advertised download rate per offer. Two hosts live at
the same moment differed by seven times, and the weights download cost
differs by the same factor:

| host | link | GLM-5.2, ~450 GB | Kimi K3, ~1585 GB |
| --- | --- | --- | --- |
| Minnesota | 14.9 Gbps | 4.0 min | 14.2 min |
| Georgia | 2.1 Gbps | 28.5 min | 100.3 min |

At $10.67/hr that is $0.71 against $5.07 for GLM, and $2.52 against
$17.83 for K3.

So "stage the weights closer" is conditional advice, and the condition is
a number nobody checks when choosing a GPU host.
`IPLANE_VAST_MIN_INET_DOWN_MBPS` already exists and defaults to 1000;
raising it for a large-model run costs nothing and buys back most of that
time.

**Second half of the same question.** `iplane load --sweep` walks its
whole concurrency ladder against one running deployment, so the download
is paid once per run rather than once per measurement. A warm cache earns
its keep across sessions, not within one.

## The card count is set by the weights

```
Kimi K3, mxfp4, 8k x 8:   8 cards -> 229.0 GB/card   overcommitted
                         16 cards -> 115.0 GB/card   fits
```

That is on 180 GB Blackwells. Commodity nodes stop at eight cards, so
K3 cannot run on a single node at any price and its rental is a cross-node
problem by arithmetic rather than by preference. #212 and #352 are hard
blockers on #360, and GLM-5.2 at eight cards is the only single-node
option in Part IV.

## Engine requirement for GLM-5.2

GLM-5.2 declares `GlmMoeDsaForCausalLM` / `model_type: glm_moe_dsa`. vLLM
registers that architecture against its `deepseek_v2` implementation,
which is consistent with the model's compressed-latent cache and its
sparse-attention indexer fields.

**The floor is `v0.27.1`.** Checked tag by tag: `v0.26.0` and `v0.27.0` do
not carry the architecture and `v0.27.1` (2026-08-11) does. Pull
`vllm/vllm-openai:v0.27.1`, or `v0.27.1-cu129` on Blackwell, which is 12.24
GB against 10.53 and wants sizing into `--min-disk-gb` alongside the
weights.

The repo's own defaults are far behind this: `upDefaultImage` is `v0.7.0`
and `pinned-versions.env` carries `ENGINE_VERSION=0.7.3`. A Part IV deploy
has to pass `--image` explicitly and will inherit nothing usable.

## The cards have to be Hopper or newer, which retires the cheap option

The engine version is not the only gate. GLM-5.2's sparse attention needs
kernels that do not exist below Hopper: vLLM's `FLASHMLA_SPARSE` backend
and the lightning indexer's `fp8_mqa_logits` are Hopper and Blackwell only,
and upstream has declined SM80-class patches, so Ampere support lives in a
community Triton backend (PR 38476) rather than in a released image.

So the 8x A100 boxes this document priced at $10.40-10.67 cannot serve this
model at any download speed. That is worth stating plainly because the
first version of this document recommended them, and a pre-flight run on
2026-08-20 rented one, spent thirty-five minutes downloading weights and
would have failed at engine start regardless.

**Hopper narrows the field to roughly double the price.** RunPod's 8x H100
at $21.52/hr and Vast's 8x H100 PCIe at $18.14/hr are what remain. The Vast
box is PCIe-class rather than SXM, which for a tensor-parallel 753B model
risks confounding the throughput numbers a chapter is drawn from, so the
declared-fabric H100 SXM is the safer substrate despite costing more.

## The checkpoint is a separate decision from the model

`zai-org/GLM-5.2` is unquantized: 1506.7 GB at fp16, 753.3 GB at fp8. The
"452 GB at four bits" figure this document sizes against is an arithmetic
projection, not a repo you can deploy. A four-bit run needs a published
four-bit build, and which one decides the card:

| build | format | size | runs on |
| --- | --- | --- | --- |
| `cyankiwi/GLM-5.2-AWQ-INT4` | compressed-tensors W4A16 | 474 GB | Ampere and up (Marlin), so Hopper |
| `amd/GLM-5.2-MXFP4` | MXFP4 | ~450 GB | Hopper and up |
| `nvidia/GLM-5.2-NVFP4` | NVFP4 | ~450 GB | Blackwell only |
| `zai-org/GLM-5.2-FP8` | FP8, first-party | 753 GB | Hopper and up, needs 8x H200 for the room |

Eight 80 GB cards hold a four-bit build with headroom and cannot hold the
FP8 one. The first-party checkpoint therefore implies H200s at $28.72/hr,
which buys out the risk of trusting a third-party quantization of a
frontier model.

## A warm cache needs the cards and the volume in the same datacenter

RunPod's network volumes are locked to a datacenter, so a model staged in
the wrong one is a cache no deploy can mount. That makes "where is the
capacity" a scheduling input rather than a detail.

Measured 2026-08-21: RunPod had eight-card capacity in exactly one
datacenter on the whole platform, AP-IN-1, and AP-IN-1 supports no volumes.
At four cards the picture is fine, with H200s in two storage-capable
datacenters, but a four-bit GLM-5.2 is 118 GB per card on four and does not
fit. So the warm-cache route was unavailable that day, and the cold route
was the only one on offer.

Vast does not have this problem so much as it has a worse one: its volumes
are machine-scoped rather than datacenter-scoped, so `iplane model pin`
does not reach it at all (#254). Every Vast run pays the download.

`iplane capacity --provider runpod` now reports the datacenter and whether
it holds volumes (#399), so this is checkable rather than something to
rediscover in GraphQL.

## Recommendation for #358

Wait for eight-card Hopper capacity in a storage-capable datacenter, then
pin and run warm. `hack/capacity-sample.sh` is the instrument; the question
it answers is whether that combination appears often enough to schedule
around, and nothing else can answer it.

Failing that, cold on whichever Hopper box is available, at `--image
vllm/vllm-openai:v0.27.1`, accepting a 474 GB download per attempt. Budget
for more than one attempt: the pre-flight found three defects in the deploy
path before it got as far as loading a model (#392, #393, #397).

The engine-ready timeout, the client timeout and the server write timeout
all have to cover the download. Thirty-five minutes was not enough.
