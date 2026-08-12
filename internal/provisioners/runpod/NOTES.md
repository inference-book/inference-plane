# RunPod adapter notes

Implementation lore for `internal/provisioners/runpod`. Mostly one theme:
**RunPod's status fields describe the rental, not the container**, and every
entry below is a version of someone believing otherwise.

## Status fields that do not mean what they look like

- **RunPod machine field**: freshly-rented pods return `"machine": {}` empty from the follow-up GET; the populated record arrives a few seconds later. Adapter's `gpuSKU` / `gpuVRAMGB` helpers are nil-defensive. **"A few seconds" is optimistic** — traced live 2026-08-09, `machine` stayed `{}` for a whole 2m14s deploy while the **top-level `machineId`** populated immediately, so treat `machineId` as the host-assignment signal (`machinePresent()` keys on both).
- **Cold-start phase boundaries must be state, not timestamps** (issue 208, fixed 2026-08-09). RunPod's `lastStartedAt` reads like "container started" and is not: it is stamped at pod-record creation, 5-7 ms *before* `createdAt`, on every pod observed. Keying the ladder on it made `classifyEnginePhase` short-circuit to `engine:init` on the first probe, so `runpod:scheduling` and `runpod:image-pull` were structurally unreachable and image-pull measured exactly 0s on every run (including both book A/B runs). The real container-start signal is the **`runtime` block on v2 `GET /v2/pods/{id}`**, null until the container runs; REST v1 omits `runtime` entirely, GraphQL v1 and REST v2 both have it. Two sub-traps: RunPod returns present-but-empty objects (`"machine": {}`, and `runtime` can arrive the same way) so presence checks must test for real content; and `runtime.uptimeInSeconds` is coarse/cached (read 37 three times across 24s) so it is evidence, not a clock. After the fix, a 1.5B cold start splits 109s image-pull / 43s engine-init — **the image pull is ~70% of a small-model cold start, and a warm deploy pays all of it**. Debug with `IPLANE_RUNPOD_PHASE_TRACE=1`.
- **RunPod staging completion is detected via v2 pod logs, not pod status** (validated live 2026-07-28). RunPod gives NO signal that a one-shot pod's command finished: v1 `desiredStatus` stays `RUNNING`, and even v2 `status` stays `RUNNING` because the exited container is auto-restarted. So `internal/provisioners/runpod/stage.go`'s `StageModel` runs `hf download` (NOT the removed `huggingface-cli`), has the command print `__IPLANE_STAGE_DONE__` / `__IPLANE_STAGE_FAIL__` then `sleep infinity`, and `waitForStageComplete` polls `GET api.runpod.io/v2/pods/{id}/logs` (SSE, `WithLogsBaseURL`-overridable) for the marker via the pure `stageSignal` scanner; the `defer Terminate` still cleans the pod. Fake-harness tested against the logs endpoint. **Large models also need `hf download --max-workers 2`** (in the staging command): the CPU staging pod has only ~4 GB RAM, and the default 8 parallel workers streaming multi-GB shards through the FUSE volume mount OOM-kill the download (SIGKILL, rc 137) on a 70B+. A small model is unaffected; validated live on 72B FP8.

## What RunPod cannot tell you

**A container that exits is invisible.** Measured 2026-08-11 by renting a pod
whose entrypoint was `/bin/false`: `desiredStatus` stayed `RUNNING`,
`lastStatusChange` reported the rental rather than the container, and the v1
pod record carries no `status` or `runtime` field at all. RunPod restarts the
exited container, so from the API's view nothing is wrong.

This is why the adapter does **not** implement
`provisioners.FailureReporter`. It is a measured absence, not an oversight, and
the only surface carrying the failure is the v2 log stream. Read this before
"finishing" the implementation.

**A missing image, by contrast, is caught at pod-create** and rejected with a
clear message, so no pod is created and nothing bills:

```
create pod: Container image "..." was not found on the registry.
```

That asymmetry with Vast, which rents the box first and discovers the bad image
afterwards, is a real architectural difference between the two providers.
