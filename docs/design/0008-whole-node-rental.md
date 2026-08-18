# 0008 — Whole-node rental: 8 GPUs, one host, intra-node fabric

**Status:** Findings (no code in this doc)
**Phase:** v0.4 Part IV, epic #361, ticket #349
**Depends on:** [0006-ch10-provider-reality-and-control-channel.md](0006-ch10-provider-reality-and-control-channel.md) (where the fabric vocabulary came from)
**Blocks:** unblocks #358 (the GLM run needs this shape to be rentable)

## What this settles

#349 asked whether serving a large MoE across 8 cards on one host is
expressible today, and said to verify before building anything. It is,
and nothing had to be built. Verified live against all three providers on
2026-08-18 using `iplane capacity`, which is free and read-only by
contract. **Nothing was rented; total spend $0.**

The one gap found is a catalog omission on Lambda Labs, filed separately.

## The request shape

There is no whole-node concept and there does not need to be. `gpu_count`
already means "N GPUs co-located on one provider machine" (`types.proto`,
`ResourceRequirements`), and `FABRIC_SCOPE_INTRA_NODE` already means
"cards within one node share a fast link". Asking for both is asking for a
whole node.

```
iplane capacity --provider vast --gpu-count 8 --fabric intra-node --min-vram-gb 80
```

and the same constraints on a deploy:

```
iplane deployment deploy glm --provider vast \
  --gpu-count 8 --min-vram-gb 80 --fabric intra-node \
  --image vllm/vllm-openai:v0.26.0-cu129 --model zai-org/GLM-4.5 \
  --tp 8 --engine-args "--max-model-len=8192"
```

```
[dry-run]   engine args: [--max-model-len=8192]
[dry-run]   split adds:  [--tensor-parallel-size=8]
[dry-run]   plan read:   weights fp16, cache fp16, 8192 tokens x 1 sequences across 8 card(s)
```

The eight-way split is accepted because `ValidateParallelism` checks
`tp * pp * dp` against the cards on one replica, and a replica is one
machine. That is the same arithmetic that refuses a split spanning
instances, which is still not a thing (#212).

## What the providers answered

Captured 2026-08-18, `--gpu-count 8 --fabric intra-node --min-vram-gb 80`:

```
=== runpod
PROVIDER  SKU                    GPUS  VRAM  $/HR   TIER       FABRIC
runpod    NVIDIA A100-SXM4-80GB  8     80GB  11.12  on-demand  intra-node (declared, 4800 Gb)

=== vast
PROVIDER  HOST   SKU        REGION         GPUS  VRAM  $/HR   FABRIC
vast      28693  A100_SXM4  Minnesota, US  8     81GB  10.67  intra-node (measured, 2400 Gb)
vast      41635  A100_SXM4  Georgia, US    8     81GB  13.34  intra-node (measured, 2400 Gb)
vast      68467  H100_SXM   Japan, JP      8     81GB  35.20  intra-node (measured, 3824 Gb)

=== lambdalabs
no candidates from 1 provider(s) for these requirements. Nothing was rented.
```

Two things worth noticing in that output.

**The fabric verdicts come from different sources and both count.** RunPod
declares intra-node from the SKU name, Vast measures it per offer. That is
`FabricSource` doing its job, and it matters because the two disagree on
bandwidth for nominally similar hardware: RunPod's declared A100-SXM4 reads
4800 Gb where Vast's measured one reads 2400 Gb. Neither figure is wrong.
One is what the card family can do and the other is what a particular host
was seen doing, which is exactly the distinction `0006` introduced.

**Vast's cheapest 8-card A100 box undercuts RunPod's**, and its H100 box is
three times either. Prices are a snapshot and the point is only that the
comparison is available for free before renting anything.

## The gap: Lambda cannot serve this, and the reason is ours

Lambda returned nothing, and the cause is not the vendor. Lambda sells
whole-node shapes:

```
  4x  gpu_4x_a100                 $ 7.96/hr
  4x  gpu_4x_h100_sxm5            $16.36/hr
  8x  gpu_8x_a100_80gb_sxm4       $22.32/hr
  8x  gpu_8x_h100_sxm5            $31.92/hr
  8x  gpu_8x_b200_sxm6            $53.52/hr
```

Our curated catalog (`internal/provisioners/lambdalabs/skus.go`) has seven
entries and every one is `GPUCount: 1`. Lambda is the only provider whose
catalog rows describe a whole instance rather than a card, so
`skucatalog.Match` filters on `Entry.GPUCount`, and at `--gpu-count 8`
every row fails. The adapter never gets as far as asking.

Filed as #380. It is **not** the same thing as #352: that ticket is
Lambda's 1-Click Clusters, which are 16+ GPUs across nodes with InfiniBand
and inter-node fabric. This is a single box with NVLink inside it, which
Lambda already sells through the same `/instance-types` endpoint the
adapter already calls.

**A caveat on proving that end to end.** At the time of checking Lambda
reported zero regions with capacity across all 24 of its shapes, not just
the multi-GPU ones. So the catalog omission is provable by reading the
code and the API side by side, and the fixed path cannot be demonstrated
against Lambda until it has capacity again. Whoever takes the follow-up
should expect to land it on the catalog evidence rather than on a live
candidate list.

## What this does not settle

- **Nothing has been rented at this shape.** Two providers say they would
  rent one. Whether an 8-card box actually comes up, serves, and reports
  eight healthy NVLinks through `iplane fleet status` is #358's job, and
  the Ch 10 interconnect work (`internal/engineagent/nvlink.go`) is what
  will answer the last part.
- **A split still cannot span instances** (#212). Everything here is one
  machine. The cross-node case is Phase 3.
- **The budget arithmetic is not expert-parallel aware** (#376), so a
  per-card figure under `--ep` is optimistic. Irrelevant to a pure
  tensor-parallel plan like the `--tp 8` above, and relevant the moment
  Part IV uses EP.
