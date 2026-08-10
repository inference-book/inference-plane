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

## Concrete near-term enhancement: make the engine:init split actually measure

**Correction (2026-08-08).** An earlier draft of this doc proposed
building the `image-pull` / `engine:init` split as new work. That split
is already **written** and has been since the deployment-observability
feature (commit `8154481`): `deployer.go`'s `classifyEnginePhase` maps
`machine` → `runpod:image-pull` and `lastStartedAt` → `engine:init`, and
`docs/deployment-observability.md` documents the ladder. The code is not
the gap.

The gap is that the split **measures nothing**. Both validated runs put
the entire cold start in `engine:init` and ~0s in `image-pull`:

| run | total | `engine:init` | `image-pull` |
| --- | ---: | ---: | ---: |
| 1.5B (2026-07-28) | 233.8s | 230.6s | absent (no row) |
| 72B FP8 (2026-07-29) | 2692s | ~2689s | ~0s |

A ~20 GB image pull is minutes, so the boundary signal is wrong.
**Diagnosed live 2026-08-09** (1.5B / RTX 4090, traced with
`IPLANE_RUNPOD_PHASE_TRACE=1`): two independent bugs, either sufficient
on its own.

- `lastStartedAt` is stamped at pod-record **creation** (7 ms *before*
  `createdAt`) and never changes, so `classifyEnginePhase`'s
  `started`-first switch pins the phase to `engine:init` at t=0.
- `machine` stays `{}` for the whole deploy while the top-level
  `machineId` populates immediately, so `machinePresent()` is false
  throughout and `scheduling` → `image-pull` cannot fire either.

The working boundary is `runtime` (null until the container starts),
present in GraphQL v1 and REST v2 but absent from the REST v1 endpoint
the prober uses. Measured against it, the 1.5B cold start was ~99s image
pull (~70%) + ~41s engine init (~30%), all of it reported as
`engine:init`. Full trace and proposed fix in issue 208.

So iplane today still folds **image pull + weight download + model load**
into one `engine:init` phase, exactly as if the split had never been
written. That is why the warm-vs-cold figure can only show the *total*
collapse, and why a warm deploy looks confusingly slow (it still pays the
image pull, invisibly).

### Closed (2026-08-09)

Fixed and re-verified live the same day, on a fresh pod:

1. `machinePresent()` now also accepts the top-level `machineId`.
2. The container-start test is v2 `/v2/pods/{id}`'s `runtime` block
   instead of `lastStartedAt`. v2 was already plumbed (`stage.go` polls
   v2 logs), so no new client was needed. Presence is tested for real
   content, since RunPod returns present-but-empty objects.
3. `classifyEnginePhase` documents that both inputs must be state, never
   timestamps.
4. The deploy regression test's fake now serves a create-stamped
   `lastStartedAt` and an empty `machine` — the exact shape RunPod
   returns — so reintroducing either bug fails the ladder assertion.

| slice | before | after | ground truth |
| --- | ---: | ---: | ---: |
| `runpod:image-pull` | 0s (0%) | **109s (72%)** | ~99s (~70%) |
| `engine:init` | 134s (100%) | **43s (28%)** | ~41s (~30%) |

So the image slice is now measurable, which is the precondition this doc
needed. It also confirms the premise: on a 1.5B, image pull is ~70% of
the cold start, and a warm deploy pays all of it.

Only once the slice is real does the engine-pin opportunity become
**measurable** and can Fig 9.7 / `diagram_coldstart_panel` honestly show
that warm-cache erases the *weight* slice while the *image* slice is
unchanged.

## Proposal

1. **Make the existing `image-pull` / `engine:init` split measure
   something.** The split is already written; its boundary signal
   (`lastStartedAt`) reads ~0s for image-pull in every run so far.
   Diagnose the signal, replace it, or drop the phase (see "Work to close
   it" above). Do this before any engine-caching work, because until the
   image slice is attributable the payoff of caching it is unmeasurable.
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
