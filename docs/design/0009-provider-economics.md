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

## Recommendation for #358

Vast, filtered to a high-bandwidth host, at `--image
vllm/vllm-openai:v0.27.1`. Roughly a fifth of the AWS price, measured
fabric rather than declared, and a four-minute weight download that makes
the absent warm cache irrelevant for a single run. Take capacity when the
sampler shows it.

RunPod becomes the better answer for repeated runs, since a network volume
turns the download into a one-off. Volumes are datacenter-locked and sized
0-4000 GB, so 450 GB is comfortable.
