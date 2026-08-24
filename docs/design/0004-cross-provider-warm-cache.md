# 0004 — Cross-provider warm cache and volumes

**Status:** Findings + proposal (no code in this doc)
**Phase:** v0.2 Ch 9 (warm cache, shipped for RunPod) → v0.3 (multi-provider)
**Depends on:** [0001-provisioner.md](0001-provisioner.md) (optional-capability model), [docs/modelstore.md](../modelstore.md) (warm cache), ROADMAP.md (v0.3 `S3Store`/`GCSStore`, AWS adapter)
**Blocks:** a cross-provider warm-cache demo; the Ch 10 RDMA-pool provisioning shares the same "big-cloud is the robust home" conclusion.

## Why this doc exists

Warm-cache model pinning (Ch 9) shipped as an **optional** `VolumeManager`
provider capability, and exactly one provider implements it: RunPod
(`internal/provisioners/runpod/volume.go`, `stage.go`). This doc records
what we learned probing whether a second provider can carry it, so the
multi-provider build is scoped from facts rather than assumptions. The
short version: it is an iplane implementation gap, not a universal
provider limitation, but the providers differ enough that the clean
answer is a big-cloud adapter, not the marketplace providers.

The capability seam (validated live 2026-07-28, RunPod):

- `EnsureVolume` — find-or-create a persistent, region-locked volume.
- `StageModel` — pre-populate the volume with the weights (RunPod: a
  throwaway CPU pod mounts the volume, runs `hf download`, exits; iplane
  reads the completion marker back out of the pod's v2 log stream because
  no pod-status field signals a one-shot command finished).
- mount at deploy — RunPod maps the volume onto `networkVolumeId`; VM-style
  providers map it onto a `docker run -v` host bind via the sshdocker
  executor (`internal/deployments/sshdocker/docker.go`, already wired).

The **mount** half is provider-agnostic already. The gap is `EnsureVolume`
+ `StageModel` on a second provider, and whether that provider even has a
persistent-volume primitive to hang them on.

## Per-provider findings (probed live 2026-07-28/29)

| provider | persistent volume | create via API | stage | mount | verdict |
| --- | --- | --- | --- | --- | --- |
| **RunPod** | network volumes | yes (`POST /networkvolumes`) | CPU pod + v2 logs | `networkVolumeId` | **works today** |
| **Lambda** | persistent filesystems (region-locked) | **UNVERIFIED — the 405 was on the wrong path, see #432** | GPU instance (no CPU tier) + SSH | sshdocker `-v` | find-only, manual pre-create |
| **AWS / GCP** | EBS / persistent disks + EFS/Filestore + **S3 / GCS** | yes, fully programmatic | cheap instance or bucket-native | block or object | **ideal, no adapter yet** (v0.3) |
| **Vast** | host-local disk, mostly ephemeral | n/a | n/a | n/a | genuinely weak (marketplace) |

Key specifics:

- **Lambda filesystem create is not in the API. Superseded 2026-08-24; see
  #432 before relying on this.** The finding was that `GET /file-systems`
  works (200) and `POST /file-systems` returns 405, so a Lambda
  `VolumeManager` could only implement `EnsureVolume` as **find-by-name**
  and would have to error with "create the filesystem in the Lambda console
  first" when absent.

  Lambda's published OpenAPI document (v1.10.0, read 2026-08-24) lists both
  spellings and only one of them is hyphenated: `GET /api/v1/file-systems`,
  `POST /api/v1/filesystems`, `DELETE /api/v1/filesystems/{id}`. The 405 was
  therefore fired at a path that has never had a POST, and the create verb
  lives at the unhyphenated sibling. Nothing has been created through either,
  so the conclusion may still hold for a reason nobody has looked for. What is
  certain is that the evidence recorded for it was the wrong request. #432
  probes it.

  Every other seam (stage, mount) is buildable either way: Lambda has a
  fitting GPU (`gpu_1x_gh200`, 96 GB, FP8-native, `us-east-3` had capacity)
  and the sshdocker `-v` mount already exists.
- **Lambda has no CPU-only instance tier**, so staging rents a GPU box
  just to download. RunPod stages on a ~$0.06/hr CPU pod; the Lambda
  equivalent is a GPU instance. Staging cost is one-time (amortized over
  warm deploys) but higher.
- **Lambda's base deploy path is unvalidated live, and the prediction paid
  out.** The adapter (`internal/provisioners/lambdalabs/`) implements the
  compute Provisioner (Spawn/Describe/Terminate/List, HTTP-Basic) and deploys
  via the shared sshdocker executor, and has still never run against a live
  Lambda VM. The RunPod warm-cache path shipped "done" and had seven live
  defects, and this doc guessed Lambda's would too.

  Three were found by reading rather than by renting (#427, PR 429), and the
  first one alone means the path could not have worked: the adapter had no
  `KeyRegistrar` despite the package doc claiming one, so the VM booted
  holding whichever key the account listed first while the deploy path
  presented iplane's own. `WaitForSSHReady` was configured and never
  implemented. `preempted` mapped to PENDING, so a reclaimed box would have
  been waited out for the full engine-ready deadline. The rental itself is
  still owed, and #427 tracks what only hardware can settle.

## Why big clouds are the real answer

Two independent needs point at the same place.

1. **Warm cache.** AWS/GCP have programmatic block volumes (EBS,
   persistent disks) and, more importantly, object storage (S3, GCS). The
   roadmap already parks `S3Store` / `GCSStore` at v0.3 precisely for
   "object-storage backends for fleet provisioning at scale." Object-store
   staging sidesteps the region-locked-volume dance entirely: stage once to
   a bucket, pull to any instance in any region.
2. **Multi-GPU / RDMA (Ch 10).** Tensor and pipeline parallelism need
   NVLink within a node and RDMA (InfiniBand / EFA) across nodes, plus
   guaranteed co-location of N big GPUs. Marketplace capacity is spot-like;
   AWS `p5.48xlarge` (8×H100 + NVSwitch + EFA), GCP `a3-mega` (8×H100 +
   NVLink + RDMA), and Azure ND-H100-v5 give the topology and the capacity
   guarantee. This is exactly what [0003-kv-domain.md](0003-kv-domain.md)'s
   `GroupProvisioner` and Ch 10's `--needs-nvlink` / `--needs-rdma` lean on.

So a big-cloud adapter (starting with AWS or GCP) is the highest-leverage
provider build: it unlocks the *clean* cross-provider warm cache (object
store, no dashboard step) and the Ch 10 RDMA-pool provisioning, on a
provider whose capacity you can actually count on.

## Proposal

1. **Do not** build Vast warm cache (no persistent cross-instance volume).
2. **Lambda find-only `VolumeManager`** is a viable *interim* proof of
   cross-provider warm cache, but it lands with a manual-dashboard-create
   caveat and requires validating the base Lambda deploy first. Build it
   only if a near-term flagship needs a second provider before the AWS/GCP
   adapter exists.
3. **Prioritize an AWS (or GCP) adapter for v0.3** with object-store
   staging (`S3Store` / `GCSStore`), which is both the robust warm-cache
   home and the foundation Ch 10's RDMA-pool provisioning needs. Design it
   against the existing `VolumeManager` seam plus the `GroupProvisioner`
   seam from 0003.

## Status of the seam (what shipped, what is proven)

- `VolumeManager` interface + RunPod impl: shipped, **validated live**
  end-to-end (cold vs warm deploy, `storage_tier` split measured) on
  2026-07-28. See [docs/modelstore.md](../modelstore.md).
- Second-provider impl: none. This doc is the scoping for it.
