# inference-plane

Reference implementation of the Control Plane built throughout
*Inference Is All You Need: A Systems Engineer's Guide* (Apress, 2026).

This repo and the book are paired in the **Tanenbaum / MINIX** style
— the book teaches principles with readable code that shows the
*shape*; this repo is the production-grade implementation. See
[ARCHITECTURE.md](ARCHITECTURE.md) for the design rationale and the
deployment topology.

The repo evolves alongside the book. Each Part of the book corresponds
to a versioned release of the control plane:

| Version | Chapter | Adds                                                  |
| ------- | ------- | ----------------------------------------------------- |
| v0.1    | 6       | Backend abstraction, gRPC + OpenAI HTTP, observability |
| v0.2    | 7–10    | Auth, rate limiting, caching, queueing                |
| v0.3    | 12–15   | Multi-tenancy, billing, multi-backend routing         |
| v1.0    | 18–19   | Production hardening, autoscaling, full platform      |

Each version is published as a maintained branch (`release/vX.Y`) with
fixes cherry-picked from `main`, plus an immutable tag (`vX.Y.0`)
marking the chapter's text-match snapshot. Check out the branch by
default; pin to the tag if you need byte-identical match to the book.

## v0.1 (this branch)

Single-backend control plane proxying to **vLLM** serving
**Qwen 3.5 8B**, exposed on two transport surfaces:

- **gRPC + Connect-RPC** at `/inferenceplane.v1.{Service}/{Method}`
- **OpenAI-compatible HTTP** at `/v1/completions`, `/v1/chat/completions`, `/health`

Observability via the OpenTelemetry Collector exporting to a local
Grafana stack (Tempo, Loki, Mimir).

## Quick start

**Local dev (any host, no GPU)** — control plane + full observability stack with the mock backend:

```sh
git checkout release/v0.1
make up         # builds controlplane + brings up obs stack (skips vllm)
make load       # synthetic traffic so dashboards populate
make dashboards # open Grafana at http://localhost:3000
```

Mock backend returns synthetic OpenAI-shaped responses with realistic latency and token counts. Runs on Mac, Linux, CI — no GPU required.

**Real inference (NVIDIA GPU host)** — switch the engine to `vllm` and bring the stack up with the `gpu` profile:

```sh
# In deploy/config.yaml: backend.engine: "vllm"
docker compose --profile gpu --env-file pinned-versions.env -f deploy/docker-compose.yaml up -d --build
make smoke      # Go integration tests against the live stack
```

See `Makefile` for the full target list, or run `make help`.

## Repository layout

```
cmd/
  controlplane/       main.go entrypoint (runs gRPC + HTTP servers)
  gennames/           code-gen for OTel name vocabulary
protos/               buf-managed proto sources
  inferenceplane/v1/  service + type definitions
gen/                  generated proto code (committed; regenerate via
                       `cd protos && make gen`)
internal/
  backends/           Backend interface + vLLM implementation
  config/             YAML + env config loading
  services/           gRPC service implementations
  telemetry/          OTel SDK setup + generated names.go
  web/server/         HTTP layer: connect-rpc adapters + grpc-gateway
deploy/
  config.yaml         reference deployment config (mounted into compose)
  docker-compose.yaml vLLM + control plane + OTel collector + Grafana stack
  grafana/            provisioned dashboards and datasources
  *-config.yaml       per-component configuration
tests/smoke/          Go integration tests (build-tag gated)
metric-names.yaml     canonical OTel name vocabulary (paired with book)
pinned-versions.env   model + engine + stack versions (paired with book)
providers.yaml        multi-provider rate table for cost gauges
```

## Topology

One process, two listeners. The gRPC server is the source of truth for
the API; HTTP surfaces (gRPC-gateway and Connect-RPC) are bindings on
top, both calling the same in-process gRPC server. See
[ARCHITECTURE.md](ARCHITECTURE.md) for details.

## License

Apache 2.0. See `LICENSE`.
