# Architecture

This document describes the v0.1 architecture of the inference-plane control plane. For day-to-day operations and commands, see [CLAUDE.md](CLAUDE.md). For the chapter-by-chapter feature roadmap, see [README.md](README.md) and the book's `TODO.md`.

## Pairing with the book

The repo and the book follow the **Tanenbaum / MINIX** model:

- **Book**: principles + readable code that shows the *shape*. Interfaces, design rationale, layer order, what each abstraction earns. Code listings are concise and pedagogical.
- **Repo (this)**: a real, maintained, production-grade implementation. Uses production libraries (servicekit, OTel SDK, connect-rpc). Versioned alongside book parts; carries fixes via maintained branches.

Two audiences, one project. Readers can take three doors in: clone-and-run as a product user, read the book to understand the design, or read the code to see how the implementation actually works.

## Topology (v0.1)

One process, two listeners.

```
                              ┌────────────────────────┐
  external clients  ────►     │ HTTP server   :8080   │
  (OpenAI SDK,                │ (public surface)       │
   curl, web UI,              │                        │
   typed gRPC clients)        │ • grpc-gateway routes  │
                              │   /v1/completions      │
                              │   /v1/chat/completions │
                              │   /health              │
                              │ • Connect-RPC handlers │
                              │   /inferenceplane.v1.* │
                              └──────────┬─────────────┘
                                         │ in-process gRPC
                                         ▼
                              ┌────────────────────────┐
                              │ gRPC server            │
                              │ 127.0.0.1:9090         │
                              │ (loopback only)        │
                              │                        │
                              │ • InferenceService     │
                              │ • HealthService        │
                              └──────────┬─────────────┘
                                         │ Backend.Generate()
                                         ▼
                              ┌────────────────────────┐
                              │ backends.Backend       │
                              │   VLLMBackend          │
                              │     ▼ HTTP             │
                              │   vLLM container       │
                              └────────────────────────┘
```

The gRPC server is the source of truth for the API. The HTTP surface dials the gRPC server in-process via `grpc.NewClient`/loopback for both gateway and connect handlers. Single implementation, multiple bindings — the same shape used in lilbattle and other projects in this stack.

## Why gRPC-first

- **Single source of truth for the API contract** — the proto file. Buf generates protobuf, gRPC, connect-rpc, and grpc-gateway code; clients pick whichever transport fits.
- **Future external services scale naturally** — when the backend split into multiple services in Part III (e.g., separate auth / quota / cache / billing services), they're already gRPC-shaped.
- **Type safety end-to-end** — typed clients (Go, web via Connect, Python via gRPC) get compile-time/runtime guarantees that JSON-only HTTP doesn't provide.
- **OpenAI compat is a binding, not a constraint on the contract** — the OpenAI HTTP layer is configured via `runtime.WithMarshalerOption` (snake_case JSON) and a custom error handler (OpenAI's `{"error": {...}}` envelope). The gRPC contract stays clean.

## Repository layout (detail)

| Path                 | Role                                                         |
| -------------------- | ------------------------------------------------------------ |
| `cmd/controlplane/`  | Binary entrypoint. Loads config, inits telemetry, starts gRPC server, builds HTTP layer, runs via servicekit graceful runner. |
| `cmd/gennames/`      | Code-gen tool: `metric-names.yaml` → `internal/telemetry/names.go` + `book/src/styles/metric-names.tex`. |
| `protos/`            | Buf-managed proto sources. `buf.yaml` (deps), `buf.gen.yaml` (plugins), `Makefile` (`gen` / `lint` / `deps`). |
| `gen/go/`            | Generated proto code (protobuf + gRPC + connect-rpc + grpc-gateway). Committed. |
| `internal/backends/` | Backend interface + vLLM implementation. Transport-agnostic Go types; the engine adapter layer. |
| `internal/config/`   | YAML + env config loading. Defaults < YAML < env. |
| `internal/services/` | gRPC service implementations satisfying the `inferencev1.InferenceServiceServer` and `HealthServiceServer` interfaces. The custom `backend.generate` span lives here. |
| `internal/telemetry/`| OTel SDK init (tracer + meter + OTLP/gRPC exporters); generated `names.go` for metric/attribute/label vocabulary. |
| `internal/web/server/`| HTTP layer: connect adapters wrapping a gRPC client, plus grpc-gateway with OpenAI-shaped marshaler and error handler. |
| `deploy/`            | docker-compose + observability configs (OTel collector, Tempo, Loki, Mimir, Grafana provisioning). |
| `tests/smoke/`       | Go integration tests with `//go:build smoke` tag. Decode responses into the same `backends` types the production code uses. |
| `metric-names.yaml`  | Canonical OTel name vocabulary (paired with book). |
| `pinned-versions.env`| Model + engine + stack version pins (paired with book). |
| `providers.yaml`     | Multi-provider rate table for cost gauges. |

## Code-generation tiers

The project distinguishes two tiers of name management:

1. **Code-gen tier** (vocabularies that grow): metric/attribute/label names. Source: `metric-names.yaml`. Generated to Go consts and LaTeX commands. `make check-names` is a CI gate.
2. **Manual + drift-check tier** (small bounded sets): version strings, model IDs, engine version, branch names. Source: `pinned-versions.env` + book's `pinned-versions.tex`. `make check-pins` is a CI gate.

See [CLAUDE.md](CLAUDE.md) for the commands.

## Observability

OpenTelemetry SDK exports OTLP/gRPC to the collector running in the deploy stack. Three pipelines fan out:

- Traces → Tempo
- Logs → Loki (slog → stdout → docker logging driver, via the collector's filelog/otlp receiver)
- Metrics → Mimir

Resource attributes carry deployment identity: `service.name`, `deployment.environment`, plus `deployment.provider`, `deployment.gpu_type`, `deployment.billing_mode`, `deployment.instance_id` from `config.DeploymentConfig`. Every span and metric carries these labels — cross-provider cost panels and per-instance drill-downs work without per-call labeling.

The custom `backend.generate` span (in `internal/services/inference.go`) carries inference-specific attributes — model, prompt tokens, completion tokens, finish reason. These come from generated constants in `internal/telemetry/names.go`.

Production swap: change `OTEL_EXPORTER_OTLP_ENDPOINT` to point at a hosted backend (Grafana Cloud, Honeycomb, Datadog OTLP). No code changes.

## Cost economics (Chapter 6 highlight)

Self-hosted cost is **GPU-hours × hourly rate**, not per-token. Utilization, not throughput, is the cost lever. Two counters to be added in PR 2 surface this directly:

- `instance.uptime.seconds.total` — wall-clock since the control plane started (what you're billed for in metered mode)
- `inference.active.seconds.total` — time actually serving inference (what you're getting value from)

Combined with per-provider `gpu.effective_rate.usd_per_second` gauges (loaded from `providers.yaml`), the cross-provider snapshot panel shows: "given my current utilization, here's what this would cost on each provider this month." See `metric-names.yaml` for the full vocabulary.

## Why these specific dependencies

- **servicekit** (Tier-1, mature) — graceful shutdown + HTTP middleware. Avoids reimplementing what the project's stack already provides.
- **connect-rpc** — single Go handler serves gRPC + Connect + HTTP/JSON. Cleaner than running separate gRPC and HTTP servers for the typed surface.
- **grpc-gateway** — REST routes from `google.api.http` annotations. Configured for OpenAI's wire format (snake_case JSON, OpenAI error envelope) so existing OpenAI SDKs work unchanged.
- **OTel Go SDK** — industry standard; vendor-neutral OTLP. Production deployments swap the endpoint, not the code.

## What's deferred

Captured here so the architecture's intentional gaps are obvious:

- **Streaming** (`stream: true` on completion requests) — left as a chapter problem.
- **Auth, rate limiting, caching, queueing** — Part II (v0.2).
- **Multi-tenancy, billing, multi-backend routing** — Part III (v0.3).
- **Stores** — no persistence layer in v0.1; first store appears in Ch 7 (auth) under `stores/` (or `internal/stores/` per the in-flight Go-internal convention).
- **Programmatic provider bring-up/shutdown** — `cmd/provision` planned as a separate concern from the control plane proper.

## Testing strategy

- Unit tests run with `go test ./...`. Backends package has 11 httptest-mocked tests covering happy path, error mapping, context cancellation, decode failures.
- Integration smoke tests in `tests/smoke/` with `//go:build smoke` build tag. Run via `make smoke` against the live stack. Decode responses into production types so schema drift surfaces as a typed test failure.
- No bash scripts for behavior tests — see [CLAUDE.md](CLAUDE.md).
