# 08d — Placement driven by the budget (STUB spec)

**Book section:** Ch 9 § Placement Driven by the Budget (`\S\ref{sec:placement}`).

**What this example should show when built:** the *only* genuine control-plane move
in Ch 9 — turning a computed VRAM budget into a rented card. Everything else in
the chapter is engine/model knowledge; this is the thin vehicle.

## Intended behavior

1. **The budget → a placement constraint.** The reader computed "32B AWQ needs
   ≥24 GB with cache headroom." Show that requirement resolving to a real SKU:
   ```
   iplane deployment deploy qwen32 \
     --model Qwen/Qwen2.5-32B-Instruct-AWQ \
     --min-vram-gb 24 \
     --engine-args --quantization,awq_marlin,--max-model-len,16384
   ```
   Print the resolver's chosen SKU (Ch 6 provider-catalog matching) and confirm
   the deploy comes up.

2. **Contrast `--min-vram-gb 24` with `--class medium`** — same budget, two
   vocabularies. Show both resolve to a fitting card.

3. **Make the pass-through visible.** Show that everything after `--engine-args`
   reaches vLLM verbatim (the control plane never parses `awq_marlin`). This is
   the `pattern:placement-vs-tuning` teaching point: control plane owns the card,
   engine owns the split.

## Notes / honesty

- The **resolver step is GPU-free** — picking a SKU from the catalog needs no
  rented card. The actual deploy needs a GPU.
- Shipped today: `--min-vram-gb` / `--class` / `--sku` (Ch 6) + `--engine-args`
  pass-through. **Roadmap:** a `--quantization awq` convenience flag + AWQ/GPTQ
  image-catalog variants would let step 1 drop the raw `--engine-args
  --quantization,awq_marlin`. When that ships, this act becomes the demo the
  `ch09-final` Capability Snapshot points at, and `--quantization` graduates from
  pass-through sugar to a first-class (but still thin) flag.
- Do **not** grow this into engine-config profiles here; `--engine-config <yaml>`
  is a separate roadmap item and a separate teaching beat.
