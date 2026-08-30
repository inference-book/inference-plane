# Committed artifacts

Every figure chapters 14 and 15 print resolves to a file in this directory,
and `series.yaml` is the index that says which figure comes from which file
and what kind of number it is. `make check-series` re-derives every entry
marked `measured` from its artifact and fails when they disagree.

## Load sweeps

`glm52-8k-vast-h200-tp8.csv`, `glm52-32k-vast-h200-tp8.csv` and
`glm52-120k-vast-h200-tp8-ranging.csv` are `iplane load sweep` output with
the provenance header the sweep writes above the column row. The header
carries the model, the hardware, the workload and any open question the run
left behind, so a row can be read without the registry in front of you. The
120k file is a ranging shot rather than a publishable sweep and says so in
its own header.

## Capacity

`capacity-2026-08-29-two-floor.jsonl` is one run of `hack/capacity-sample.sh`,
eight rows: four GPU widths (1, 2, 4, 8) at each of two VRAM floors (80 and
140 GB), against runpod, vast and lambdalabs. Reproduce the shape of it with

```
hack/capacity-sample.sh /tmp/sample.jsonl
```

which needs `VAST_API_KEY`, `RUNPOD_API_KEY` and `LAMBDA_API_KEY` and rents
nothing. The offers themselves are perishable, so a fresh run will not match
this file; what reproduces is the method and the blind spot it exposes.

This is the first run taken after `383b066` gave the sampler a second floor,
and it is here because chapter 15 cites it. The rolling
`capacity-samples.jsonl` at the repo root is a working log and stays
gitignored; this is the observation the book leans on, pinned.

**What chapter 15 takes from it.** At eight cards the two floors return
disjoint offer sets. The 80 GB floor returns three candidates, all of them
80 GB parts, and vast answers `no_capacity`. The 140 GB floor, in the same
instant, returns seven, topping out at `8x B300 / 275 GB per card /
$100.01 per hour` on vast, which is 2200 GB to a chassis. Nothing at or
above 180 GB per card appears anywhere in the 688 samples the rolling log
holds at the 80 GB floor, at any width, across eleven days. That is the
whole point of the file: the series recorded a scarcity that was partly a
property of the question being asked.

Check the two numbers the chapter prints:

```
jq -r 'select(.gpu_count==8) | "\(.min_vram_gb)GB floor: \(.total_candidates) offers"' \
  docs/data/capacity-2026-08-29-two-floor.jsonl

jq -r 'select(.gpu_count==8 and .min_vram_gb==140) | .candidates[]
       | select(.vram_gb_per_gpu >= 200)
       | "\(.sku) \(.gpu_count)x\(.vram_gb_per_gpu)GB = \(.gpu_count * .vram_gb_per_gpu)GB at $\(.price_usd_per_hour)/hr"' \
  docs/data/capacity-2026-08-29-two-floor.jsonl
```

These are capacity observations rather than sweep-derived figures, so they
have no `derived` entry in `series.yaml` and no automated check. One market
observation at one instant is not a series and should not be dressed as one.
