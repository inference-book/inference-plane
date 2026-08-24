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
| **Lambda** | persistent filesystems (region-locked) | **YES — `POST /api/v1/filesystems`**, unhyphenated (#432) | GPU instance (no CPU tier) + SSH, **not built yet** | `file_system_names` at launch, then a host-path bind | create + find, **shipped** |
| **AWS / GCP** | EBS / persistent disks + EFS/Filestore + **S3 / GCS** | yes, fully programmatic | cheap instance or bucket-native | block or object | **ideal, no adapter yet** (v0.3) |
| **Vast** | host-local disk, mostly ephemeral | n/a | n/a | n/a | genuinely weak (marketplace) |

Key specifics:

- **Lambda filesystem create IS in the API. Corrected 2026-08-24 (#432).** The
  original finding was that `GET /file-systems` works (200) and `POST
  /file-systems` returns 405, so a Lambda `VolumeManager` could only implement
  `EnsureVolume` as find-by-name and would have to error with "create the
  filesystem in the Lambda console first" when absent. That conclusion stood
  for four weeks and it is wrong.

  Lambda spells the collection two ways and only one of them takes a POST:

  ```
  GET    /api/v1/file-systems      list      (hyphenated)
  POST   /api/v1/filesystems       create    (not hyphenated)
  DELETE /api/v1/filesystems/{id}  delete    (not hyphenated)
  ```

  The 405 was fired at the read path. Probed live 2026-08-24, creating and
  deleting a filesystem in `us-east-1`:

  ```
  POST /api/v1/filesystems {"name":"iplane-probe-fs","region":"us-east-1"}  -> 200
  {"id":"641142fb...","name":"iplane-probe-fs",
   "mount_point":"/lambda/nfs/iplane-probe-fs",
   "is_in_use":false,"region":{"name":"us-east-1"}}

  DELETE /api/v1/filesystems/641142fb...  -> 200  {"deleted_ids":["641142fb..."]}
  ```

  Four things follow, and they are what a `VolumeManager` needs.

  **The mount path is derived, not chosen.** `/lambda/nfs/<name>`, returned on
  the record. `VolumeRef` can carry it rather than the adapter guessing.

  **A filesystem outlives every instance.** The one above existed with zero
  instances on the account, which is the property a warm cache is entirely
  about. `is_in_use` reports whether anything currently has it mounted.

  **`file_system_names` is honoured at launch, and validated before capacity
  is checked.** Launching into the wrong region with a real filesystem name
  returns `global/object-does-not-exist` naming both the filesystem and the
  region, and rents nothing:

  ```
  "Filesystem with name 'iplane-probe-fs' does not exist in region 'us-west-1',
   or you do not have permission to access it."
  ```

  Failing before the rent rather than after is the useful half. A wrong
  volume name costs an error rather than a billed box that mounts nothing.

  **Region-locking is enforced by the vendor**, so the datacenter-locked rule
  this doc already carries for RunPod holds on Lambda too and does not need
  to be re-imposed in iplane.

  Still unverified, and it needs hardware: whether the mount actually appears
  inside the engine container at that path. Folded into #427's rental rather
  than costing a second one.

- **Lambda has no CPU-only instance tier**, so staging rents a GPU box
  just to download. RunPod stages on a ~$0.06/hr CPU pod; the Lambda
  equivalent is a GPU instance. Staging cost is one-time (amortized over
  warm deploys) but higher.
- **Lambda's base deploy path is validated live now, and the prediction paid
  out in full.** This doc guessed that the adapter, which had never been
  pointed at a Lambda VM, would carry defects the way the RunPod warm-cache
  path did when it shipped "done". It carried seven.

  Three were found by reading rather than by renting (#427, PR 429), and the
  first alone means the path could not have worked: the adapter had no
  `KeyRegistrar` despite the package doc claiming one, so the VM booted
  holding whichever key the account listed first while the deploy presented
  iplane's own. `WaitForSSHReady` was configured and never implemented.
  `preempted` mapped to PENDING.

  Four more came out of the rental itself on 2026-08-24, and three of those
  are on the shared VM-style path rather than in the adapter, so **AWS and GCP
  inherit them** (#428). `sshdocker` assumed a root SSH user; Lambda logs in
  as `ubuntu`, which is the stock cloud-image shape. `CreateInstance`'s
  idempotency lookup adopted a different deployment's machine. A retried
  auto-provisioned deploy rents a second machine (#439). And the one that
  matters most here: **a deployed VM-style deployment could not be destroyed
  at all**, because the endpoint a teardown needs was erased after the deploy
  used it.

  What works now: a Lambda box rents, deploys through `sshdocker`, serves
  tokens, and tears down. Cold start launch to serving is 4m30s. Full account
  in [0010](0010-lambda-validation-findings.md).

  For the warm cache specifically, the useful consequence is that the VM path
  this doc's Lambda column depends on is real rather than assumed. The mount
  seam is still unverified: nobody has looked at `/lambda/nfs/<name>` from
  inside a running container, which is what #436 needs.

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
