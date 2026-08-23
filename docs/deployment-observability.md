# Deployment & lifecycle observability

The serving path has had metrics + traces since Chapter 6. The
*provision / teardown* path did not: "why is my RunPod deploy slow" was
unanswerable because the whole cold start was one opaque wait on the
engine's `/health` proxy (a 502→200 flip with no attribution). This adds
phase-attributed metrics, per-deploy traces, and a Grafana dashboard for
the spin-up / spin-down half of the control plane.

## The seam: the executor's phase stream

A `Deployer` (RunPod, sshdocker, ...) already emits `DeployStateUpdate`s
as it progresses — `{State, Phase, ProgressMessage, ...}`. That phase
stream is the instrumentation seam. The control-plane `Service` wraps the
`emit` closure it hands to `Deploy`/`Destroy` with a `deployObserver`
(`internal/provisioners/deploy_telemetry.go`) that turns phase
transitions into spans and duration metrics.

Deriving telemetry here rather than inside the adapters keeps the CP/DP-1
boundary intact: provider and data-plane code stay free of OTel imports,
and every provider gets the same lifecycle observability the moment it
emits phases. Adding a new phase is a one-line change to the string an
adapter emits — no telemetry wiring.

## Phases

The deployer reads two observable signals during the `/health` wait and
maps them to a monotonic phase ladder. They come from different API
versions because neither carries both: v1 has the host assignment but no
container-runtime state, v2 has the runtime block.

| Phase                | Signal                                              | What's happening                          |
| -------------------- | --------------------------------------------------- | ----------------------------------------- |
| `runpod:scheduling`  | v1 `machineId` empty and `machine` empty             | RunPod finding capacity for the SKU set   |
| `runpod:image-pull`  | v1 host assigned, v2 `runtime` null                  | host assigned, pulling the engine image   |
| `engine:init`        | v2 `runtime` populated                               | container up, model download + weight load |
| `engine:download`    | agent reports the model cache growing                | weights arriving; refines `engine:init`   |
| `engine:load`        | cache present and no longer growing                  | weights loading into VRAM + graph capture |
| `engine:serving`     | `/health` 2xx                                        | engine answering; deploy is RUNNING        |

`engine:init` splits into the two rungs above **only when something reports
the disk**. That refinement is the Service's, not a provider's: it happens in
the emit wrapper where the replica id is already in scope, so RunPod and Vast
get it without either adapter knowing. Absent a reading the phase stays
`engine:init`, exactly as before.

The split exists because one rung covered the download, the load and graph
capture, and the histogram could not say which was slow. Two GLM-5.2 deploys
spent an hour each in it and the record still cannot attribute either. See
`internal/provisioners/staging.go`.

The engine itself cannot help here: vLLM hands `snapshot_download` a disabled
tqdm, so a container pulling a 474 GB checkpoint prints nothing at all until
the fetch completes. Streaming its logs harder buys the load half and shows a
blank screen through the download.

Both signals are **state**, never timestamps. v1's `lastStartedAt` looks
like a container-start signal and is not one; see the history below.

The status read refines observability only; readiness is still `/health`
alone. A flaky status read keeps the last known phase (phases never
regress). Once the container has started, the deployer stops polling
status — `/health` is the only remaining signal.

### History: why the signals are what they are (issue 208)

Until 2026-08-09 the ladder above described only what the code
*intended*. Both live A/B runs measured an `image-pull` bucket at or near
zero, with `engine:init` absorbing the entire cold start:

| run | total | `engine:init` | everything else |
| --- | ---: | ---: | ---: |
| 1.5B (2026-07-28) | 233.8s | 230.6s | 3.2s (scheduling + create-pod; no image-pull row) |
| 72B FP8 (2026-07-29) | 2692s | ~2689s | ~3s |

**Confirmed live on 2026-08-09** (1.5B / RTX 4090, pod
`31f66dn93v32fa`, traced with `IPLANE_RUNPOD_PHASE_TRACE=1`). Across all
25 ticks of a 2m14s wait there was exactly one distinct signal
combination, and `scheduling` / `image-pull` were emitted zero times.
Two independent bugs, either of which alone breaks the ladder:

1. **`lastStartedAt` is a creation stamp.** It read `06:19:04.093`
   against a `createdAt` of `06:19:04.100`, so it is populated *7 ms
   before the pod record exists* and never changes. Since
   `classifyEnginePhase` tests `started` first, a non-empty value on the
   first probe pins the phase to `engine:init` at t=0 and makes
   `image-pull` structurally unreachable.
2. **`machine` stays `{}`.** `machinePresent()` only looks inside the
   `machine` object, which stayed empty for the entire deploy while
   RunPod populated the **top-level `machineId`** immediately. So
   `scheduling` → `image-pull` cannot fire either.

**Fixed** by keying `machinePresent()` on the top-level `machineId` too,
and by replacing the `lastStartedAt` test with v2's `runtime` block
(null until the container starts). Re-verified live the same day on a
fresh pod, against ground truth derived independently from
`runtime.uptimeInSeconds`:

| slice | before fix | after fix | ground truth |
| --- | ---: | ---: | ---: |
| `runpod:image-pull` | 0s (0%) | **109s (72%)** | ~99s (~70%) |
| `engine:init` | 134s (100%) | **43s (28%)** | ~41s (~30%) |

The lasting lesson: **phase boundaries must be state, not timestamps.**
A timestamp answers "did this happen at some point", which is a
different question from "is this true now" — and `lastStartedAt` was
stamped 5-7 ms *before* `createdAt` on every pod observed.

Two RunPod-specific traps worth remembering: it returns
present-but-empty objects (`"machine": {}`, and `runtime` can arrive the
same way), so presence checks must test for real content; and
`uptimeInSeconds` is coarse and cached (read 37 three times across 24s),
so it is fine as evidence but not as a clock.

Note the image pull dominates even a 1.5B cold start, and a *warm*
deploy pays it in full. Warm-cache pinning erases the weight download,
not the image. Levers for the image slice are in
[docs/design/0005-engine-image-coldstart.md](design/0005-engine-image-coldstart.md).

## Metrics

Declared in `metric-names.yaml` (regen with `make gen-names`):

| Instrument                            | Type      | Labels                     |
| ------------------------------------- | --------- | -------------------------- |
| `iplane.deployment.phase.duration`    | histogram | `phase`, `provider`, `result` |
| `iplane.deployment.provision.duration`| histogram | `provider`, `result`, `class` |
| `iplane.deployment.provisions.total`  | counter   | `provider`, `result`       |
| `iplane.deployment.teardown.duration` | histogram | `provider`, `result`       |

`result` is `running` (success), `timeout` (hit the engine-ready
deadline — the dominant cold-start failure), or `failed`. Teardown uses
`terminated` / `failed`.

## Traces

Each deploy/destroy is one trace: a root span `deployment.provision` (or
`deployment.teardown`) with a child span per phase, named by the phase
string. In Tempo this is the cold-start waterfall
(`scheduling → image-pull → engine-init → serving`) request-by-request.

## Dashboard

`deploy/grafana/provisioning/dashboards/inference-plane-deployment.json`
("Inference Plane Deployment & Lifecycle"). Read the *time by phase*
panel first — mean seconds per phase, stacked, so the bars attribute the
cold start to the stage actually costing you. Also: end-to-end
provision p50/p95, phase-p95 tails, provision outcomes, teardown
duration, and the idle-reaper spin-down series.

## Wiring

The daemon (`iplane serve`) and the one-shot `iplane up` both build a
`metrics.Recorder` and pass it via `provisioners.WithRecorder(...)` — the
same recorder the reaper already uses. Unset (tests, telemetry-free
CLIs) the observer records into a no-op and traces go to the no-op
tracer, so nothing needs telemetry to run.
