# 08b — Quantization in production (STUB spec)

**Book section:** Ch 9 § Quantization in Production (`\S\ref{sec:quantization-production}`).

**What this example should show when built:** the same 30B-class model deployed at
two precisions, so the reader watches the weight footprint drop and the fit change.

## Intended behavior

1. Deploy the anchor at **FP16** and show it needs an 80 GB card:
   ```
   iplane deployment deploy qwen32-fp16 \
     --model Qwen/Qwen2.5-32B-Instruct \
     --min-vram-gb 80 \
     --engine-args --max-model-len,8192
   ```
   Capture `nvidia-smi` after load: ~65.5 GB weights resident, little headroom.

2. Deploy the same model at **4-bit AWQ** and show it fits a 24 GB card:
   ```
   iplane deployment deploy qwen32-awq \
     --model Qwen/Qwen2.5-32B-Instruct-AWQ \
     --min-vram-gb 24 \
     --engine-args --quantization,awq_marlin,--max-model-len,8192
   ```
   Capture `nvidia-smi`: ~19.3 GB weights, room left for the KV pool.

3. **Measure decode throughput both ways** (single stream + a small concurrent
   batch) and print tokens/s side by side. This is the real data behind
   `fig:quant-throughput` (currently synthetic). Confirm the Marlin kernel is
   engaged for the AWQ run (the `awq_marlin` quant path), since the book's point
   is that the kernel dominates the format.

## Notes / honesty

- **Needs real GPUs** — the whole point is the on-card footprint. GPU-free is not
  meaningful here.
- Today this rides the shipped `--engine-args` pass-through (`--quantization
  awq_marlin` goes straight to vLLM). The roadmap `--quantization awq` convenience
  flag + AWQ image-catalog variant would let step 2 drop the raw `--engine-args`;
  fold that in when it ships and update the book's Capability Snapshot.
- Anchor throughput/quality numbers to cite live in the book's reference list
  (JarvisLabs vLLM quant benchmarks; AWQ/GPTQ papers).
