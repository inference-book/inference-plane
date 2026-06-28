# 04-router-in-path

Beat 1 closer for v0.2 / Ch 7. Drives a chat completion **through the
router**, fires synthetic load to populate the v0.2 Grafana dashboard,
and points at the resulting Tempo trace.

```
client -> iplane router -> engine
```

(v0.1's path was `client -> engine`; the router is the v0.2 change the
chapter teaches.)

## What this exercises

- **Flat URL routing** (`/v1/chat/completions`) -- the router peeks the
  `model` field in the body and picks the newest RUNNING deployment
  serving it. The OpenAI-SDK-compatible shape.
- **Deploy-id URL routing** (`/v1/<deploy-id>/v1/models`) -- the
  unambiguous escape hatch for endpoints with no body to peek at. The
  router strips the prefix and forwards the rest.
- **Request metrics**: `iplane_router_requests_total`,
  `iplane_router_request_latency_seconds`,
  `iplane_router_completion_tokens_total` (all `deploy_id`-scoped).
- **W3C TraceContext + Baggage propagation**: router span + engine
  span share a trace id; baggage (e.g. `tenant_id`) survives the
  reverse-proxy hop.
- **`--no-idle-destroy` pin**: deployments live across demos 04/05/06
  without the reaper evicting them between runs.

## Cost

RunPod-only (local instances have no SSH endpoint, so v0.2 cannot
deploy to them). Cost depends on the model size you pick:

| Size  | Model                          | VRAM   | Cold start | Run cost |
|-------|--------------------------------|--------|-----------|----------|
| 1.5B  | `Qwen/Qwen2.5-1.5B-Instruct`   | ~3 GB  | 30-60s    | ~$0.05   |
| 3B    | `Qwen/Qwen2.5-3B-Instruct`     | ~6 GB  | 60-90s    | ~$0.10   |
| 7B    | `Qwen/Qwen2.5-7B-Instruct`     | ~14 GB | 90-180s   | ~$0.25   |

The pod is created with `--no-idle-destroy`, so the cost above is for
the demo run itself; if you leave it running for demos 05/06 you pay
the per-hour metered rate until you destroy it. The last step prompts
you to leave or destroy (default: leave).

A **zero-cost mock-engine variant** is filed as #126; this PR ships
the RunPod path only.

## Prerequisites

```bash
# Required: a scoped RunPod API key (full access, rpa_ prefix)
export RUNPOD_API_KEY=...

# Optional but recommended: tell the engine pod where to ship OTel
# data. The local observability stack runs on the operator's laptop,
# so a hosted OTLP URL (Grafana Cloud Free) or a `cloudflared` tunnel
# is what makes engine traces reachable.
export IPLANE_OTEL_ENDPOINT=https://otlp-gateway-prod-XXX.grafana.net/otlp
export IPLANE_OTEL_HEADERS='Authorization=Basic <token>'
```

The demo runs without `IPLANE_OTEL_ENDPOINT` set; the engine just
won't ship traces in that case. Router-side traces (the chapter's
primary artifact) still land in your local Tempo via the
`iplane serve` OTel pipeline.

## Run

```bash
# Terminal 1 -- observability stack (Tempo + Mimir + Grafana on :3000/:3200)
make up

# Terminal 2 -- iplane daemon on :8080
iplane serve

# Terminal 3 -- this walkthrough
cd examples/04-router-in-path
make demo
```

`make demo` drives a prebuilt `iplane` (the same binary terminal 2
runs); it does not compile the control plane. Resolution order is
`--bin`, then `$IPLANE_BIN`, then the repo's `bin/iplane`, then `iplane`
on `$PATH`. If none resolve it fails fast with a `make build` hint.

The walkthrough is interactive at two steps: "Choose a model size"
(default 1.5B) and "Leave the deployment running, or destroy it now"
(default: leave). Press Enter through both for the cheapest path.

## What the walkthrough does

1. **Ping `iplane serve`** -- ListDeployments as the cheapest
   reachability probe.
2. **Probe Grafana + Tempo** -- non-fatal pre-flight. If unreachable
   the dashboard tour at the end only prints pointers.
3. **Pick a model size** (1.5B / 3B / 7B).
4. **Find or create a RUNNING deployment** for the chosen model. If
   one already serves the chosen model in RUNNING state, the demo
   reuses it (zero cost, zero wait). Otherwise `CreateDeployment` with
   `--no-idle-destroy=true` provisions a fresh pod.
5. **GET `/v1/models` through the router** via the deploy-id URL --
   proves the router can reach the engine pod.
6. **POST `/v1/chat/completions` through the flat URL** -- one
   round-trip; prints the completion + token counts + latency.
7. **Fire `iplane load`** at the configured rps / duration --
   populates the v0.2 dashboard.
8. **Print Grafana panel pointers** for `inference-plane-v02`.
9. **Print Tempo trace pointer** with a Grafana Explore deep-link.
10. **Leave or destroy** (default: leave so demos 05/06 reuse it).
    Reused-existing deployments are kept alive even on `destroy` to
    avoid stomping on a longer-lived workflow.

## Re-runnable

Bring up `iplane serve` once and run this demo as many times as you
like. The detect-and-reuse step on `deploy` skips provisioning when a
matching RUNNING deployment already exists. Demos 05 (fair-queueing)
and 06 (multi-replica) attach to the same `iplane serve` and reuse
this deployment.

## Record a trace

```bash
make readme
```

Regenerates `RUN.md` from a real recorded run. Requires `iplane serve`
running and `RUNPOD_API_KEY` set; costs the same as a normal `make
demo` run.

The committed `RUN.md` here is the structural rendering (`make
readme-static`) -- it shows the mermaid diagram, actor list, and step
notes, but no captured engine output. Run `make readme` if you want a
live transcript.
