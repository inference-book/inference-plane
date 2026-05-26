# Telemetry seeding

> **Status (2026-05-25):** v0.1 Phase 3 — the engine's OTel data
> reaching the operator's chosen sink. iplane plumbs the endpoint URL
> (and optional auth headers) onto the deployment's environment; the
> engine (vLLM, Triton, anything OTel-instrumented) does the rest.

## Why this exists

The chapter's flagship `iplane up --provider runpod --model qwen3.5-8b`
promise has three reads: **API key in, OpenAI endpoint out, AND
observable**. PR 45 covered the first two. This doc covers the third.

The engine runs on a remote pod (RunPod, later Lambda Labs / vast.ai
/ k8s). The operator's collector lives somewhere with a stable URL —
either a hosted provider or the operator's laptop reachable via tunnel.
iplane's job is **endpoint URL propagation**, not running an OTel
stack itself.

## Two paths to a sink

### Path A — Hosted OTLP (recommended for first-time readers)

Sign up for any provider with an OTLP HTTP endpoint:

| Provider           | Free tier OTLP endpoint                      | Auth header                        |
|--------------------|----------------------------------------------|------------------------------------|
| Grafana Cloud Free | `https://otlp-gateway-prod-XXX.grafana.net/otlp` | `Authorization=Basic <token>`      |
| Honeycomb          | `https://api.honeycomb.io`                   | `x-honeycomb-team=<api-key>`       |
| Uptrace            | `https://otlp.uptrace.dev`                   | `uptrace-dsn=<dsn>`                |

(Provider URLs and headers change occasionally — check your provider's
"OpenTelemetry" or "OTLP" docs page for the canonical values.)

```bash
export IPLANE_OTEL_ENDPOINT=https://otlp-gateway-prod-XXX.grafana.net/otlp
export IPLANE_OTEL_HEADERS='Authorization=Basic <base64 token>'
iplane deployment deploy my-llama --provider runpod --class small \
    --image vllm/vllm-openai:v0.7.0 --model Qwen/Qwen2.5-1.5B-Instruct
```

iplane sets `OTEL_EXPORTER_OTLP_ENDPOINT` and
`OTEL_EXPORTER_OTLP_HEADERS` on the pod. The engine ships
traces / metrics directly to the hosted backend. Zero local infra.

### Path B — Local docker-compose stack via cloudflared tunnel

The repo ships a complete obs stack at `deploy/docker-compose.yaml`
(otel-collector + tempo + loki + mimir + grafana). To make it reachable
from a remote pod, run cloudflared as a profiled service that creates
a public `trycloudflare.com` quick tunnel:

```bash
COMPOSE_PROFILES=tunnel make up
# Cloudflared starts and prints a URL like
#   https://random-words.trycloudflare.com
# to its stderr. iplane extracts it:
export IPLANE_OTEL_ENDPOINT=$(iplane telemetry url)
iplane deployment deploy my-llama --provider runpod --class small \
    --image vllm/vllm-openai:v0.7.0 --model Qwen/Qwen2.5-1.5B-Instruct
```

The pod ships OTLP to the trycloudflare URL → cloudflared forwards to
otel-collector inside docker-compose → fans out to tempo / loki / mimir.
Browse the data at `http://localhost:3000` (grafana, default
`admin/admin`).

**Caveat:** quick tunnels rotate every restart. Re-run
`iplane telemetry url` after `make up` to get the new URL. For a stable
URL, register a named cloudflared tunnel (out of scope for v0.1; a
chapter-Y exercise).

## How it works under the hood

`iplane deployment deploy` accepts:

```
--otel-endpoint <url>           (default: $IPLANE_OTEL_ENDPOINT)
--otel-headers KEY=VALUE        (default: parsed from $IPLANE_OTEL_HEADERS, comma-separated)
```

These translate to env vars on the deployed pod:

```
OTEL_EXPORTER_OTLP_ENDPOINT=<url>
OTEL_EXPORTER_OTLP_HEADERS=KEY1=VAL1,KEY2=VAL2
```

These are the **standard** OTel SDK env vars; any OTel-instrumented
engine reads them without iplane-specific knowledge. vLLM, Triton,
custom engines built on the OTel Python / Go / C++ SDKs all
participate.

`--env KEY=VALUE` overrides --otel-endpoint / --otel-headers — power
users can pin protocol (`OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf`),
add resource attributes, or set per-signal endpoints.

## Choosing a protocol over the tunnel path

The cloudflared quick-tunnel works best with **OTLP/HTTP** (HTTP/1.1
or HTTP/2 with a normal Content-Type). OTLP/gRPC technically works
through the tunnel but trips on HTTP/2 stream multiplexing edge cases
in some configurations. The 03 demo pins
`OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf` on the engine env for
this reason.

The hosted-OTLP path supports both — most providers accept gRPC and
HTTP. If you switch the demo to a hosted sink and want gRPC, drop
the `OTEL_EXPORTER_OTLP_PROTOCOL` env or set it to `grpc`.

## What the engine actually ships

vLLM v0.7+ exports:

- **Traces** for each `/v1/chat/completions` and `/v1/completions`
  call: spans cover queue wait → prefill → decode → response.
- **Metrics** for request count, queue depth, tokens generated,
  cache hit rate, GPU memory.

The Grafana dashboard at `deploy/grafana/provisioning/dashboards/inference-plane-v01.json`
renders the v0.1 selection (request rate, GPU memory, tokens/sec).

## Troubleshooting

- **"IPLANE_OTEL_ENDPOINT is not set"** when running the 03 demo: the
  demo hard-fails on a missing endpoint by design — telemetry is the
  Phase 3 chapter beat, not an optional aside. Pick a sink and export.

- **`iplane telemetry url` errors with "no trycloudflare.com URL"**:
  cloudflared is still starting (give it 5–10 s) or the tunnel profile
  isn't active. Verify with `docker ps | grep cloudflared`.

- **No data showing up in Grafana / hosted backend**: confirm
  `OTEL_EXPORTER_OTLP_ENDPOINT` is set on the pod with
  `iplane deployment describe <id> -o json | jq '.env'`. If it's
  missing, the operator's IPLANE_OTEL_ENDPOINT wasn't set when the
  deploy ran — re-deploy.

- **vLLM logs a TLS error against trycloudflare**: cloudflared quick
  tunnels expose HTTPS, and some older vLLM builds default to gRPC
  with an `http://` scheme. Add `OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf`
  to the deploy env (the 03 demo does this).
