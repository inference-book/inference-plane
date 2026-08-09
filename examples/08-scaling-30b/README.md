# Demo 08 — Scaling to 30B+ Models (Ch 9)

> **Status: partly runnable.** Act **08a (cold-start distance)** is a full
> runnable demo — the warm-cache pinning surface shipped (PRs 197–201), so this
> act drives real `iplane model pin` + cold-vs-warm deploys. Acts **08b–08e**
> remain `PROMPT.md`-style specs, authored ahead of the iplane surface from the
> book side. Chapter 9 is otherwise engine/model-centric (the VRAM budget,
> quantization, engine tuning), so most of that teaching is first-principles and
> needs no new iplane code: those demo bits ride a thin, already-shipped vehicle
> (the `--engine-args` pass-through and the Ch 6 `--class` / `--min-vram-gb`
> placement surface). The specs let the book prose point at
> `examples/08-scaling-30b/<act>` while the runnable code lands later.

The chapter's spine is the **VRAM budget**: weights + KV cache + activations +
overhead against one card's capacity. The demos make that budget concrete on a
real (rented) GPU, and show the two levers that move it — quantization (shrink
the weights) and the engine's memory knobs (decide how the leftover splits).

Anchor model: **Qwen2.5-32B-Instruct** (65.5 GB FP16 weights; 19.3 GB at 4-bit
AWQ). 70B tier: **Llama-3.3-70B-Instruct**.

## Acts

| Act | Illustrates (book section)                        | One-line intent |
| --- | ------------------------------------------------- | --------------- |
| **08a** (runnable) | Cold-start distance / warm-cache pinning (§ Caching the Weights, Fig 9.7) | Same 32B model deployed cold (download from HF) then warm (mount a pinned volume); the `engine:init` phase collapses and the `storage_tier` split shows on the deployment dashboard. See [08a-cold-start-distance/](08a-cold-start-distance/). |
| 08b | Quantization in production (§ Quantization)       | Same model at AWQ vs FP16: weight footprint drops ~65→~19 GB, the model now fits a 24 GB card, throughput measured both ways. This is also where the VRAM budget's "does it fit" arithmetic (`nvidia-smi` + vLLM's KV-block report against the four-term budget) becomes concrete. |
| 08c | Engine partitioning / OOM-at-startup (§ Partition)| Drive `--gpu-memory-utilization` / `--max-model-len` past the KV pool and reproduce vLLM's "not enough KV cache blocks" startup failure, then the fix. |
| 08d | Placement driven by the budget (§ Placement)      | `iplane deploy --class medium --min-vram-gb 24 --engine-args --quantization awq` finds a card that fits the computed budget and deploys — the thin control-plane vehicle. |
| 08e | The single-GPU ceiling (§ Single-GPU Ceiling)     | Attempt the 70B anchor on one card, hit the wall, motivate Ch 10 tensor parallelism. Closes at the ceiling; does NOT open the multi-GPU box. |

## What is GPU-free vs paid

Unlike Demo 07, most of Ch 9's numbers **require a real GPU** — the VRAM budget is
only honest against real hardware, and 08a's cold-start distance is only honest
against a real download onto real storage (mock/`external` providers have no volume
mechanism, so they can't show a warm mount). The gate here is renting GPUs for the
figures, not building an iplane feature. Where a point can be made without a GPU
(the budget arithmetic itself, the placement resolver picking a SKU), the act says
so. **08a destroys every GPU pod it creates on exit** (see its README); the pinned
volume persists as the reusable cache.

## iplane surface these acts assume

- **Shipped today:** `iplane model pin` / `ls` / `unpin` (warm-cache pinning, the
  08a vehicle) plus the `storage_tier` deploy-phase label and the "Engine-init:
  warm vs cold" Grafana panel. `iplane deployment deploy --engine-args <flags>`
  passes arbitrary vLLM flags through (`--quantization awq`,
  `--gpu-memory-utilization`, `--max-model-len`, `--kv-cache-dtype fp8`). Placement
  via `--class` / `--min-vram-gb` / `--sku` (Ch 6).
- **Roadmap (not shipped):** dedicated `iplane deploy --quantization awq`
  convenience flag, AWQ/GPTQ image-catalog variants, `--engine-config <yaml>`
  profiles. See `ROADMAP.md` v0.2 "Quantization-aware deploy" / "Engine config
  per deployment". When these land, fold them into 08b/08d and cut the
  `ch09-final` `capabilities.yaml` entry the book's Capability Snapshot transcribes.
