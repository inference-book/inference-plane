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

## The catalog lives on GraphQL, not REST

`rest.runpod.io/v1` has no catalog endpoint. `GET /v1/gpus` returns 400 with
"that path does not exist in the specification". The GPU catalog is only on
`api.runpod.io/graphql`, which is why `Candidates` is the second GraphQL read
in this adapter after SSH keys, and why `gqlPost` was already there to reuse.

`gpuTypes.lowestPrice` takes a `gpuCount` and this is the reason the call is
worth making. Availability is a property of a card **at a width**, not of a
card: probing live on 2026-08-15, **35 of 48 types were obtainable as a single
GPU and 11 of 48 as eight**. A `stockStatus` of null means the type cannot be
had at that width at all, and a null price means the same, so both are dropped
rather than reported.

## RunPod does not expose a reclaimable tier through the catalog

`minimumBidPrice` sits next to `uninterruptablePrice` and looks exactly like a
spot rate. It is not, or at least the catalog gives no way to tell: the two
were **equal on all 38 shapes that had any availability** when measured on
2026-08-15.

A bid price that is not below the on-demand price is the same rental
relabelled, so `Candidates` drops those for a `RECLAIM_POLICY_PREFERRED`
request rather than claiming a discount that does not exist and an
interruptibility this endpoint gives no way to verify.

Worth re-measuring before building on it. The narrow claim is that the catalog
does not expose a distinguishable tier, so an operator cannot see what they
would be agreeing to before agreeing to it. Whether RunPod sells interruptible
capacity by some other route is a separate question; `bidPerGpu` exists on the
create call and the catalog cannot price it.

## System RAM comes with the pod shape

RunPod is the one provider that can neither let an operator select system RAM
nor report it per candidate. It arrives with the pod and scales with the card
count, so `DefaultSystemRAMGb` is a per-card estimate and the shared resolver
multiplies it by the requested width before comparing against `min_ram_gb`.

Getting that scaling wrong was #283: a four-card request asking for 200 GB was
judged against one card's 96, which rejected every A100 and H100 and landed on
a B200 whose single-card 256 was the only figure that cleared the bar. Asking
for the 384 that four cards actually carry matched nothing at all.

## The readiness loop lives elsewhere

`waitForEngineReady` is a wiring of `internal/provisioners/enginewait` (#268).
RunPod's half is the `Observe` callback plus `Config.Endpoint`, which is seeded
because the proxy URL is known the moment the pod id exists. That seeding is
load-bearing rather than cosmetic: it lets the loop probe `/health` before
asking the pods API anything, so a pod that is already serving costs zero status
reads. Three deployer tests assert exactly that by failing on an unexpected
`GET /pods/{id}`.

The phase ladder (`runpod:scheduling` -> `runpod:image-pull` -> `engine:init`)
and its derivation from pod status stay here, because they are claims about what
RunPod's API reports. Vast has its own, and the two deliberately share only the
`engine:init` rung so a dashboard can put them side by side.

## The API knows where the capacity is, and this adapter used to say it did not

`dataCenters { gpuAvailability(input:{gpuCount:N}) }` reports availability
per datacenter at the same width the price query asks about, and `Spawn`
already accepts a region as a best-effort `dataCenterIds` pin. The comment
that said RunPod "schedules wherever it has capacity" described the rent
path's default and quietly became a claim about what the API knows.

The datacenter is decision-relevant because a **network volume is locked to
one**. A model staged into the wrong datacenter is a warm cache no deploy
can mount. Measured 2026-08-21: RunPod's only eight-card capacity on the
whole platform was AP-IN-1, and AP-IN-1 supports no volumes, so the
stage-then-rent-warm plan was impossible that day and nothing in the CLI
could say so (#399).

Candidates are now one row per (type, datacenter), with the datacenter's own
stock reading where it has one, and `datacenter_storage` in the attrs. A
type no datacenter list mentions still gets a row with no region, because
the datacenter view is extra information and losing it must not lose the
candidate.

## costPerHr rides on the create response

The pod's price is bound off `createPodResponse` and reported through
`DeployStateUpdate.HourlyRateUSD`, rather than being left to the `Describe`
that runs after a successful deploy. A deploy that rents a pod and then
fails to serve still billed for the attempt, and the rate arriving only on
the happy path is what made those rentals vanish from spend (#397). An
absent field leaves zero, which is what unknown already meant.
