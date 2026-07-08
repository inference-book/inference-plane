# Demo 08 — Scaling to 30B+ Models (Ch 9) — STUB

> **Status: STUB / forward-scaffold.** This directory is authored *ahead* of the
> iplane Ch 9 surface, from the book side. Chapter 9 is engine/model-centric (the
> VRAM budget, quantization, engine tuning), so most of the teaching is
> first-principles and needs no new iplane code. The demo bits that *do* touch
> iplane ride a thin, already-shipped vehicle: the `--engine-args` pass-through
> and the Ch 6 `--class` / `--min-vram-gb` placement surface. Each act below is a
> `PROMPT.md`-style spec of what the example should *do* when it is built, so the
> book prose can point at `examples/08-scaling-30b/<act>` while the runnable code
> lands later. No `run.sh` yet — these are specs, not scripts.

The chapter's spine is the **VRAM budget**: weights + KV cache + activations +
overhead against one card's capacity. The demos make that budget concrete on a
real (rented) GPU, and show the two levers that move it — quantization (shrink
the weights) and the engine's memory knobs (decide how the leftover splits).

Anchor model: **Qwen2.5-32B-Instruct** (65.5 GB FP16 weights; 19.3 GB at 4-bit
AWQ). 70B tier: **Llama-3.3-70B-Instruct**.

## Acts

| Act | Illustrates (book section)                        | One-line intent |
| --- | ------------------------------------------------- | --------------- |
| 08a | The VRAM budget / "does it fit" (§ VRAM Budget)   | Deploy the 32B anchor at FP16 and watch it eat an 80 GB card; `nvidia-smi` + vLLM's own KV-block report make the four-term budget visible. |
| 08b | Quantization in production (§ Quantization)       | Same model at AWQ vs FP16: weight footprint drops ~65→~19 GB, the model now fits a 24 GB card, throughput measured both ways. |
| 08c | Engine partitioning / OOM-at-startup (§ Partition)| Drive `--gpu-memory-utilization` / `--max-model-len` past the KV pool and reproduce vLLM's "not enough KV cache blocks" startup failure, then the fix. |
| 08d | Placement driven by the budget (§ Placement)      | `iplane deploy --class medium --min-vram-gb 24 --engine-args --quantization awq` finds a card that fits the computed budget and deploys — the thin control-plane vehicle. |
| 08e | The single-GPU ceiling (§ Single-GPU Ceiling)     | Attempt the 70B anchor on one card, hit the wall, motivate Ch 10 tensor parallelism. Closes at the ceiling; does NOT open the multi-GPU box. |

## What is GPU-free vs paid

Unlike Demo 07, most of Ch 9's numbers **require a real GPU** — the VRAM budget is
only honest against real hardware. The gate here is renting GPUs for the figures,
not building an iplane feature. Where a point can be made without a GPU (the budget
arithmetic itself, the placement resolver picking a SKU), the act says so.

## iplane surface these acts assume

- **Shipped today:** `iplane deployment deploy --engine-args <flags>` passes
  arbitrary vLLM flags through (`--quantization awq`, `--gpu-memory-utilization`,
  `--max-model-len`, `--kv-cache-dtype fp8`). Placement via `--class` /
  `--min-vram-gb` / `--sku` (Ch 6).
- **Roadmap (not shipped):** dedicated `iplane deploy --quantization awq`
  convenience flag, AWQ/GPTQ image-catalog variants, `--engine-config <yaml>`
  profiles. See `ROADMAP.md` v0.2 "Quantization-aware deploy" / "Engine config
  per deployment". When these land, fold them into 08b/08d and cut the
  `ch09-final` `capabilities.yaml` entry the book's Capability Snapshot transcribes.
