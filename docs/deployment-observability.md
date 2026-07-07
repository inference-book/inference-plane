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

RunPod's REST v1 exposes no explicit container-runtime state, so the
deployer reads two observable signals from `GET /pods/{id}` during the
`/health` wait and maps them to a monotonic phase ladder:

| Phase                | Signal                                  | What's happening                          |
| -------------------- | --------------------------------------- | ----------------------------------------- |
| `runpod:scheduling`  | `machine` empty                         | RunPod finding capacity for the SKU set   |
| `runpod:image-pull`  | `machine` populated, `lastStartedAt` "" | host assigned, pulling the engine image   |
| `engine:init`        | `lastStartedAt` populated               | container up, model download + weight load |
| `engine:serving`     | `/health` 2xx                           | engine answering; deploy is RUNNING        |

The status read refines observability only; readiness is still `/health`
alone. A flaky status read keeps the last known phase (phases never
regress). Once the container has started, the deployer stops polling
status — `/health` is the only remaining signal.

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
