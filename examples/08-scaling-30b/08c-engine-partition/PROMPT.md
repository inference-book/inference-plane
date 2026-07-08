# 08c — Engine partitioning & the startup that refuses (STUB spec)

**Book section:** Ch 9 § How the Engine Partitions VRAM
(`\S\ref{sec:engine-partition}`).

**What this example should show when built:** the vLLM memory knobs decide how the
budget splits, and getting them wrong is a *startup* failure, not a slow degrade.

## Intended behavior

1. **Reproduce the OOM-at-startup.** Deploy the anchor at FP16 (or on a
   deliberately small card) with `--max-model-len 32768` and capture vLLM's real
   refusal:
   ```
   ValueError: To serve at least one request with the model's max seq len
   (32768), (N GiB KV cache is needed, which is larger than the available
   KV cache memory. Based on the available memory, decrease max_model_len
   or increase gpu_memory_utilization.
   ```
   This is the transcript the book quotes — capture the exact current wording
   from the pinned vLLM version so the book stays honest.

2. **Fix it three ways**, each a term in the budget:
   - lower `--engine-args --max-model-len,8192` (less KV per reserved seq),
   - raise `--engine-args --gpu-memory-utilization,0.95` (claim more card),
   - `--engine-args --kv-cache-dtype,fp8` (halve KV bytes/token).
   Show each makes the same deployment start, and print vLLM's computed KV-block
   count so the reader sees the pool grow.

3. **Show the utilization split** via `nvidia-smi` + vLLM's startup log: weights
   resident vs KV pool size, at FP16 vs INT4 on the same card. This is the real
   data behind `fig:engine-partition`.

## Notes / honesty

- **Needs a real GPU** (and ideally two card sizes, e.g. a 24 GB and an 80 GB, to
  show the split move). The startup *error* can be reproduced cheaply on the
  smallest card that overcommits.
- All knobs ride the shipped `--engine-args` pass-through today. No new iplane
  surface required for this act — it teaches an *engine* concept; iplane is just
  the deploy vehicle.
- Capture the vLLM error text from the pinned `\enginedisplay` version; the
  message wording drifts between releases.
