## inference-plane

Reference implementation of the control plane for *Inference Is All You Need* (Apress, 2026). See [README.md](README.md) for the layout and [ARCHITECTURE.md](ARCHITECTURE.md) for the design.

## Quick-ref commands

| Command          | Purpose                                                |
| ---------------- | ------------------------------------------------------ |
| `make help`      | List all targets                                       |
| `make up`        | Bring up the full stack (vLLM + control plane + obs)   |
| `make down`      | Tear it down                                           |
| `make smoke`     | Go integration tests against a live stack              |
| `make test`      | Unit tests (no live stack needed)                      |
| `make build`     | Compile `bin/controlplane`                             |
| `make check-pins`  | Verify `pinned-versions.env` matches book's `.tex`   |
| `make check-names` | Verify generated names match `metric-names.yaml`     |
| `make gen-names` | Regenerate `internal/telemetry/names.go` + book `.tex` |
| `cd protos && make gen` | Regenerate proto code into `gen/`               |

## Conventions

- **Generated code is committed** (`gen/`, `internal/telemetry/names.go`, book's `metric-names.tex`). Regen + commit together; `make check-names` and `make check-pins` run as CI gates.
- **Versioned releases** map to book parts: `release/v0.1` (Ch 6), `release/v0.2` (Ch 7–10), etc. Tag `vX.Y.0` for the immutable chapter snapshot.
- **gRPC server is source of truth.** Connect-RPC adapters and grpc-gateway are HTTP bindings on top — both dial the in-process gRPC server.
- **No shell scripts for behavior tests.** Use Go integration tests (build tag gated). Shell is fine for orchestration (`make` targets, `docker compose` wrappers) but not for assertions.
- **OTel name vocabulary** (`metric-names.yaml`) is paired with the book. Edit YAML → `make gen-names` → both `names.go` and the book's `metric-names.tex` regenerate together.

## Gotchas

- Generated proto code lives in `gen/go/`. Don't hand-edit; regen via `cd protos && make gen`.
- The gRPC server binds `127.0.0.1:9090` only. It's an in-process implementation detail, not a public surface. Public traffic hits the HTTP server on `:8080`.
- `cd protos && buf generate` needs `buf.lock` populated — run `buf dep update` once after cloning.
- `gen/go/google/api/` is generated via `include_imports: true`. Without it, the gateway code wouldn't compile.

## Env vars

| Var                          | Purpose                                                |
| ---------------------------- | ------------------------------------------------------ |
| `CP_CONFIG_PATH`             | Path to deployment config (default `deploy/config.yaml`) |
| `CP_BACKEND_URL`             | Override backend URL                                   |
| `CP_PROVIDER` / `CP_GPU_TYPE` / `CP_BILLING_MODE` / `CP_INSTANCE_ID` | Cost-metric labels for this deployment |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | OTLP collector address                                |

Provider API keys for future `cmd/provision` tool (not used in v0.1): `RUNPOD_API_KEY`, `LAMBDA_API_KEY`, `VAST_API_KEY`, `EQUINIX_AUTH_TOKEN`, `EQUINIX_PROJECT_ID`. See `.env.local.example`.

## Stack dependencies

- `github.com/panyam/servicekit` — graceful shutdown + HTTP middleware (Tier-1, mature)
- `connectrpc.com/connect` — gRPC + Connect + HTTP/JSON on one handler
- OpenTelemetry Go SDK + OTLP/gRPC exporters
