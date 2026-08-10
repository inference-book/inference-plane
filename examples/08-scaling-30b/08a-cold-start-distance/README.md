# Demo 08a — Cold-start distance (Ch 9, § Caching the Weights)

The runnable backing for the book's **Figure 9.7**: a cold deploy and a
warm deploy of the *same* model on one time axis.

A 32B model is tens of gigabytes of weights. On a **cold** deploy the
engine downloads them from Hugging Face *inside the pod*, and at this
size the download dominates the cold start. **Warm-cache pinning**
pre-stages the weights onto a provider network volume with `iplane model
pin`, so the next deploy **mounts** them instead of downloading. The
`engine:init` phase collapses.

iplane tags every deploy phase with a `storage_tier` label (`cold` =
download in-pod, `warm` = mounted volume), so the collapse is a measured
gap on the deployment dashboard, not a stopwatch anecdote.

| Phase | What happens | `storage_tier` |
| ----- | ------------ | -------------- |
| COLD  | no pinned volume; engine downloads ~19 GB (AWQ) / 65 GB (FP16) from HF | `cold` |
| WARM  | `iplane model pin` staged the weights; the deploy mounts them | `warm` |

`run.sh` walks the arc an operator actually lives: deploy cold, feel the
download, pin the model, redeploy warm. Same model, same region, so the
only thing that changes between the two is where the weights came from.

## Run it (real GPUs — pennies to a dollar)

```bash
make build                                              # once, from the repo root
make infra-up                                           # obs stack (Grafana/Tempo/collector)
export RUNPOD_API_KEY=rpa_...                            # full-access key
export OTEL_EXPORTER_OTLP_ENDPOINT=localhost:4317        # so the panels populate
bash examples/08-scaling-30b/08a-cold-start-distance/run.sh
# or: cd examples/08-scaling-30b/08a-cold-start-distance && make demo
```

This spends real money: two GPU deploys (cold, then warm) plus one CPU
staging pod from the pin. Typically well under a dollar. The A/B numbers
print to stdout even without the obs stack; only the Grafana/Tempo views
need it.

### Teardown

`run.sh` destroys **every GPU pod it creates** on exit — both deploys,
and the CPU staging pod self-terminates. The cold deploy is torn down
*before* the warm one starts, so you never hold two GPUs at once. The
pinned network **volume persists on purpose**: it is the reusable cache,
and a re-run is warm and cheap. Destroy it too with `DEMO_UNPIN=1` (or,
after the fact, `iplane model unpin <volume-id>`).

If `run.sh` is killed hard (SIGKILL) the trap can't fire — check the
`runpod` console for orphaned pods and `iplane model ls` for the volume.

### What a run looks like

```
=== COLD deploy (coldstart-cold-10635) ... ===
=== COLD deploy reached RUNNING in 235s ===
==> destroying the cold deploy to free the GPU before pinning ...
=== pin Qwen/Qwen2.5-1.5B-Instruct onto a runpod/EU-RO-1 volume ===
staged Qwen/Qwen2.5-1.5B-Instruct onto volume 3e1bu1mrv9 (runpod / EU-RO-1)
=== WARM deploy (coldstart-warm-10635) ... ===
=== WARM deploy reached RUNNING in 102s ===
########################################################################
# cold-start distance
#   COLD  (download from HF) : 235s   storage_tier=cold
#   WARM  (mount pinned vol) : 102s   storage_tier=warm
########################################################################
```

Those are real numbers from a validated run (Qwen2.5-1.5B, `min-vram-gb 16`,
EU-RO-1, 2026-07-28). The per-phase split, read off
`iplane.deployment.phase.duration` by `storage_tier`:

| phase | cold | warm |
| --- | ---: | ---: |
| `runpod:scheduling` | 1.5s | 1.5s |
| `runpod:create-pod` | 1.7s | 2.1s |
| `engine:init` | **230.6s** | **97.6s** |
| total | 233.8s | 101.2s |

The whole `storage_tier` difference lives in `engine:init` (the download vs
the mount); scheduling and create-pod are identical.

This run predates the phase-ladder fix (issue 208), so its `engine:init`
still has the engine-image pull folded in — which is why the warm column
is 97.6s rather than near-zero. A rerun today reports the pull as its own
`runpod:image-pull` row on both columns, leaving `engine:init` as the
weight download + model load that `storage_tier` actually changes.

The exact seconds depend on the model, the region's capacity, and HF
bandwidth on the day. A 1.5B's ~3 GB download makes a modest gap; the book's
32B / 65 GB anchor makes it dramatic (that download dominates `engine:init`,
and the warm mount erases it).

### Where to look

- **Tempo** — search `deployment.provision`. Compare the two traces: the
  cold deploy has a fat `engine:init` child span (the download); the warm
  deploy's `engine:init` is a sliver (the mount). `scheduling` and
  `image-pull` look the same on both, which is the point: warm-cache
  erases the weight download, not the image pull.
- **Grafana** — "Inference Plane Deployment & Lifecycle" dashboard, the
  **"Engine-init: warm vs cold"** panel. That is the `storage_tier` split,
  read straight off `iplane.deployment.phase.duration`.

## Env knobs

| Var | Default | Purpose |
| --- | ------- | ------- |
| `DEMO_MODEL` | `Qwen/Qwen2.5-32B-Instruct-AWQ` | model to A/B. A smaller model runs faster/cheaper and still shows a download-dominated cold start. |
| `DEMO_IMAGE` | `vllm/vllm-openai:v0.7.0` | engine image. |
| `DEMO_MIN_VRAM_GB` | `24` | min VRAM per GPU (fits the AWQ anchor). |
| `DEMO_REGION` | `EU-RO-1` | region for *both* deploys and the pin. A volume is datacenter-locked, so this is the region that gets warmed. |
| `DEMO_PROVIDER` | `runpod` | provider. Must support volume pinning (`VolumeManager`); today that is RunPod. |
| `DEMO_DEPLOY_TIMEOUT` | `20m` | per-deploy `--wait` deadline. The CLI default (8m) is too short for a real cold deploy (image pull + download + engine start). |
| `DEMO_ENGINE_READY_TIMEOUT` | `20m` | daemon-side engine-`/health` wait (exported as `IPLANE_RUNPOD_ENGINE_READY_TIMEOUT`). The default 10m can be blown past by a cold ~10 GB image pull on community capacity. |
| `DEMO_MIN_DISK_GB` | `0` (deploy default) | container disk for image + downloaded weights. The default 20 GB is far too small for a large model; set it to weights + image + headroom (e.g. `150` for a ~73 GB model). |
| `DEMO_UNPIN` | `0` | `1` = destroy the pinned volume on exit. |
| `IPLANE_BIN` | `<repo>/bin/iplane` | path to the binary. |

Expect the whole run to take **~15–60 minutes**: a cold deploy (image pull +
download + engine start), the pin's staging pod, then a warm deploy. The cold
deploy is the long pole, and for a large model it dominates everything.

## Why this script owns `iplane serve`

`iplane model pin` runs **in-process** and needs the state-dir flock —
the same lock `iplane serve` holds for its whole lifetime. So the pin
can't run against a live daemon. `run.sh` sequences it: serve up for the
cold deploy, serve **down** for the pin, serve up again for the warm
deploy. That is also why this demo has no `make serve` target (it would
collide on `:8080` and the lock). Remote `--service-url` pinning is a
documented follow-up.

## Capturing the book figure (flagship: 72B FP8, measured)

Figure 9.7's cold-start distance, measured on a real 72B FP8 flagship
(`RedHatAI/Qwen2.5-72B-Instruct-FP8-dynamic`, ~73 GB) on a single B200
(EU-RO-1, 2026-07-29):

| | total | `engine:init` | `storage_tier` |
| --- | ---: | ---: | --- |
| **cold** (download) | **2692 s (~45 min)** | ~2689 s | cold |
| **warm** (mounted) | **244 s (~4 min)** | 239 s | warm |

**~11x faster**, ~$4.41 → ~$0.40 of B200 time per deploy. Almost the whole
cold start is `engine:init`, and ~41 min of that is the 73 GB HF download.

The totals above are what the A/B proves. Read the warm bar carefully
though: those 239s are *not* all mount-and-load. They still include the
~25 GB engine-image pull, which warm-cache does nothing for. These two
runs predate the phase-ladder fix (issue 208) and so report the pull
inside `engine:init`; a rerun today splits it out as `runpod:image-pull`.
On a 1.5B the pull alone was 109s of a 152s cold start, so on a rerun
expect the warm bar to be mostly image pull.

Reproduce it (needs an FP8-capable card, so a Blackwell/Hopper image, and
the disk + timeouts sized for the model):

```bash
DEMO_MODEL=RedHatAI/Qwen2.5-72B-Instruct-FP8-dynamic \
DEMO_MIN_VRAM_GB=150 \                 # isolates the 192 GB B200
DEMO_MIN_DISK_GB=150 \                 # 73 GB weights + ~25 GB image + headroom
DEMO_IMAGE=vllm/vllm-openai:v0.26.0-cu129-ubuntu2404 \  # Blackwell (B200) support
DEMO_REGION=EU-RO-1 \
DEMO_DEPLOY_TIMEOUT=45m DEMO_ENGINE_READY_TIMEOUT=45m \
  bash run.sh
```

H200 (141 GB, cheaper) is the nicer single-GPU fit if a volume region has
capacity; B200 is what was available. A 32B FP16 (`Qwen/Qwen2.5-32B-Instruct`,
`DEMO_MIN_VRAM_GB=80`) reproduces the book's original 65 GB caption on an
80 GB card if you want that exact number instead. Screenshot the Tempo
waterfall pair and the deployment dashboard's warm-vs-cold panel.

See [docs/modelstore.md](../../../docs/modelstore.md) (warm cache + pinning)
and [docs/deployment-observability.md](../../../docs/deployment-observability.md)
(the phase ladder and `storage_tier` label).
