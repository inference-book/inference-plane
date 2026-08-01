# 0005 — Engine-image cold start ("pinning the engine")

**Status:** Findings + proposal (no code in this doc)
**Phase:** v0.2 Ch 9 (warm cache) → follow-up
**Depends on:** [docs/deployment-observability.md](../deployment-observability.md) (the phase ladder + `lastStartedAt` signal), [docs/modelstore.md](../modelstore.md) (model warm cache, the thing this generalizes from), [0004-cross-provider-warm-cache.md](0004-cross-provider-warm-cache.md)
**Blocks:** nothing yet. Feeds a cleaner Fig 9.7 phase breakdown and a future warm-pool / image-streaming story.

## Why this doc exists

Warm-cache pinning removes one big network transfer from a cold deploy:
the model weights. But a cold deploy has **two** big transfers, and we
only pinned one. The other is the engine container image (vLLM is
~20 GB), which is re-pulled on every fresh host.

The measured proof is in our own numbers. On the 1.5B validation run the
warm deploy's `engine:init` was **97.6 s**, not near-zero, even though the
weights were already on the mounted volume. The residual is the image
pull plus model load: **both** the cold and warm deploys pay the ~20 GB
image pull, because a mounted model volume does nothing for the container
image. On the 72B FP8 / B200 run the image-pull ("Extracting") phase
visibly dominated the first minutes of engine:init at $5.89/hr. So the
engine image is the *irreducible* part of today's warm deploy, and it is
the natural next slice to attack.

## The lever menu (container image cold start)

This is a known problem with a known set of solutions:

| lever | what it does | where it lives |
| --- | --- | --- |
| **Image streaming** | container starts before the full image is pulled; layers fetched lazily on first access | AWS SOCI (Seekable OCI), GKE image streaming, stargz/eStargz, nydus |
| **Regional registry / pull-through cache** | the image bytes come from a fast in-region mirror, not a far registry | ECR, Artifact Registry, any registry mirror |
| **Warm pools** | keep hosts with the image (and model) already resident; a deploy skips the pull entirely | scheduler / keep-alive policy |
| **Bake weights into the image** | one pull fetches image + weights together | custom per-model images; usually a loss (huge images, rebuild churn, defeats the shared model cache) |

## iplane's leverage, and its honest limit

Model-pin works because it is a **bind mount iplane controls** (RunPod
`networkVolumeId`, sshdocker `docker run -v`). Container image *loading*
is the provider's runtime, not a mount, so there is **no clean
provider-agnostic "engine pin" primitive** the way there is for models.
iplane's real levers are narrower:

1. **Warm pools** — keep a deployment resident instead of reaping it,
   which skips both the image pull and the weight download on the next
   request. iplane already owns this seam: the idle-TTL reaper plus
   `TouchDeployment` and the `--no-idle-destroy` pin. Turning that into a
   deliberate "keep N warm" capacity policy is the highest-leverage,
   most-portable move.
2. **Provider / region choice** — deploy where the registry is close and
   fast, or where image streaming exists.
3. **Image streaming where the provider exposes it** — SOCI (AWS), image
   streaming (GKE). These are big-cloud features, so this is another vote
   for the AWS/GCP adapter: volumes, RDMA pools (0004, 0003), and now
   image cold-start all converge on the same provider build.
4. **Slimmer engine images** — a marginal, always-available lever.

So "pinning the engine" is mostly *warm pools + streaming-capable
providers*, not a new mount primitive. Worth stating plainly so nobody
tries to force it into the `VolumeManager` shape.

## Concrete near-term enhancement: split the engine:init phase

iplane currently folds three very different costs into one `engine:init`
phase: **image pull + weight download + model load**. That is why the
warm-vs-cold figure can only show the *total* collapse, and why a warm
deploy looks confusingly slow (it still pays the image pull, invisibly).

The signal to split them already exists. `docs/deployment-observability.md`
notes RunPod's `lastStartedAt` flips from empty to populated when the image
pull finishes and the container starts. So the deployer can emit:

- `runpod:image-pull` — machine assigned → `lastStartedAt` populated.
- `engine:init` — `lastStartedAt` populated → `/health` 2xx (download + load).

With that split, the deployment dashboard attributes the ~20 GB image
slice separately from the weight slice, the engine-pin opportunity becomes
**measurable**, and Fig 9.7 / `diagram_coldstart_panel` can honestly show
that warm-cache erases the *weight* slice while the *image* slice is
unchanged. This is a small, self-contained deployer + telemetry change and
it directly improves the book figure's honesty.

## Proposal

1. **Split `engine:init` into `image-pull` + `engine:init`** on the RunPod
   deployer using the existing `lastStartedAt` signal. Small, measurable,
   improves the Ch 9 figure. Do this before any engine-caching work so the
   payoff is visible.
2. **Frame warm pools as the portable "engine pin"** — a keep-N-warm
   capacity policy layered on the reaper/`--no-idle-destroy` seam iplane
   already has. Design later; name it now so it is not conflated with
   model-pin.
3. **Fold image streaming into the AWS/GCP adapter scope** (0004) rather
   than chasing a provider-agnostic primitive that the container runtime
   will not give us.

## Book note (Ch 9)

Good teaching beat: decompose cold start into **image-pull +
weight-download + model-load**; warm-cache pinning kills the middle slice;
name image-pull's levers (streaming, registry proximity, warm pools) as
the related lever the chapter does *not* pin. This honestly frames what
warm-cache does and does not solve, and it explains why a warm deploy is
faster but not instant.
