# 08e — The single-GPU ceiling (STUB spec)

**Book section:** Ch 9 § The Single-GPU Ceiling (`\S\ref{sec:single-gpu-ceiling}`).
This act is the **handoff to Ch 10** (Multi-GPU / tensor parallelism,
`\ref{ch:multi-gpu}`). It shows the wall; it does **not** climb over it.

**What this example should show when built:** the two ways a single card runs out,
so the reader feels why Ch 10 has to exist.

## Intended behavior

1. **Weights alone don't fit.** Attempt the 70B anchor
   (`meta-llama/Llama-3.3-70B-Instruct`) on one card:
   - FP16 on an 80 GB card → OOM at load (~140 GB weights).
   - INT4/AWQ on a 48 GB card → loads but leaves no workable KV pool (or OOMs
     once `max-model-len` reserves a sequence).
   Capture both failures.

2. **Weights fit, KV drains.** Take the 32B AWQ deploy from 08d on a 24 GB card
   and push `--max-model-len` + concurrent sessions until the KV pool is
   exhausted and the engine starts rejecting/queuing. This is the *same* ceiling
   Demo 07 (Ch 8) hit from the routing side (`subsec:cache-ceiling`), now shown
   as a hard VRAM wall rather than a cache eviction.

3. **Point at the exit, don't take it.** End by noting the fix is
   `--tensor-parallel-size N` (spread one engine across GPUs) — and STOP. That is
   Ch 10 / Demo 09's opening. Do not implement or demo multi-GPU here; Ch 9 keeps
   every engine one opaque single-card KV domain.

## Notes / honesty

- **Needs real GPUs**, including at least one multi-card box only to *show the
  fix exists* (a one-liner, not a demo).
- No new iplane surface. `--tensor-parallel-size` rides the same `--engine-args`
  pass-through, but its teaching belongs to Ch 10, where iplane gains real
  multi-GPU placement (the ROADMAP v0.3 "70B / multi-GPU" work).
- Keep this act SHORT in the book — it is a chapter-closing handoff, not a
  full walkthrough.
