## inference-plane

Reference implementation of the control plane for *Inference Is All You Need* (Apress, 2026). See [README.md](README.md) for the layout, [ARCHITECTURE.md](ARCHITECTURE.md) for the design, and [RELEASE.md](RELEASE.md) for the branch / tag / forward-merge / cherry-pick workflow.

## Quick-ref commands

| Command          | Purpose                                                |
| ---------------- | ------------------------------------------------------ |
| `make help`      | List all targets                                       |
| `make up`        | Bring up the full stack (vLLM + control plane + obs)   |
| `make down`      | Tear it down                                           |
| `make smoke`     | Go integration tests against a live stack              |
| `make load`      | Synthetic traffic generator (works against mock or vllm) |
| `make test`      | Unit tests (no live stack needed)                      |
| `make build`     | Compile `bin/controlplane`                             |
| `make build-image` | Build the controlplane Docker image without starting |
| `make check-pins`  | Verify `pinned-versions.env` matches book's `.tex`   |
| `make check-names` | Verify generated names match `metric-names.yaml`     |
| `make check-constraints` | Verify architectural constraints (CP/DP-1, ...) |
| `make gen-names` | Regenerate `internal/telemetry/names.go` + book `.tex` |
| `cd protos && make gen` | Regenerate proto code into `gen/`               |

## Conventions

- **Generated code is committed** (`gen/`, `internal/telemetry/names.go`, book's `metric-names.tex`). Regen + commit together; `make check-names` and `make check-pins` run as CI gates.
- **Versioned releases** map to book parts: `release/v0.1` (Ch 6), `release/v0.2` (Ch 7–10), etc. Tag `vX.Y.0` for the immutable chapter snapshot. See [RELEASE.md](RELEASE.md) for the lifecycle (active branch is a moving snapshot until the chapter is cut; revisits cherry-pick forward).
- **gRPC server is source of truth.** Connect-RPC adapters and grpc-gateway are HTTP bindings on top — both dial the in-process gRPC server.
- **Architectural constraints in [CONSTRAINTS.md](CONSTRAINTS.md).** Project-wide rules enforced by `make check-constraints`. **CP/DP-1**: data-plane code (`internal/router/`, `internal/dataplane/`) reaches control-plane state only via the generated gRPC client — never via direct `internal/provisioners` import. Makes the eventual data-plane split mechanical, not a refactor. New rules get extracted from real friction, not invented speculatively.
- **No shell scripts for behavior tests.** Use Go integration tests (build tag gated). Shell is fine for orchestration (`make` targets, `docker compose` wrappers) but not for assertions.
- **OTel name vocabulary** (`metric-names.yaml`) is paired with the book. Edit YAML → `make gen-names` → both `names.go` and the book's `metric-names.tex` regenerate together.
- **Default engine is `mock`** for local dev. Real inference (`engine: vllm`) requires `--profile gpu` on the compose stack and an NVIDIA host. See `deploy/config.yaml` for the toggle.
- **Branch-specific pins**: `main` carries `CP_VERSION=dev`; release branches carry `vX.Y.0`. `check-pins.sh` skips these.

## Gotchas

- Generated proto code lives in `gen/go/`. Don't hand-edit; regen via `cd protos && make gen`.
- The gRPC server binds `127.0.0.1:9090` only. It's an in-process implementation detail, not a public surface. Public traffic hits the HTTP server on `:8080`.
- `cd protos && buf generate` needs `buf.lock` populated — run `buf dep update` once after cloning.
- `gen/go/google/api/` is generated via `include_imports: true`. Without it, the gateway code wouldn't compile.
- **State-file flock**: `internal/provisioners/stores/file/file.go`'s `lock()` returns `*os.File` (not `int`) — the runtime's finalizer will close the underlying FD if the `*os.File` goes out of scope, which silently releases the flock and can tear down recycled FDs (gRPC stream sockets, etc.). Regression-tested in `internal/provisioners/stores/file/file_test.go`.
- **RunPod machine field**: freshly-rented pods return `"machine": {}` empty from the follow-up GET; the populated record arrives a few seconds later. Adapter's `gpuSKU` / `gpuVRAMGB` helpers are nil-defensive.
- **`make check-names` false-positive locally**: when the sibling `../book/` checkout exists, `git diff --quiet -- internal/X ../book/Y` flips into compare-two-files mode and trips spuriously. CI (no book checkout) is unaffected. Tracked as issue #108; do not chase the diff it prints unless `make gen-names` actually changed `internal/telemetry/names.go`.
- **Examples that build the iplane CLI**: anchor the source path via `runtime.Caller` rather than a literal `../../cmd/iplane`. See `examples/04-router-in-path/main.go`'s `buildIplane()`. The relative-path form in `examples/02-cli-end-to-end/main.go` is buggy from any cwd today; do not copy it.
- **Scheduler defaults to OFF.** `router.queue.servicers` defaults to `0` in `deploy/config.yaml`, which means no scheduler is constructed and the router takes the direct-forward path (Beat 1 behavior). The v0.2 release/v0.2 snapshot ships with this default to avoid surprising operators on existing deploys; demo 05 requires `servicers > 0` (and `in_flight_cap > 0` for the queue-pressure story to be visible). Documented in `examples/05-fair-queueing/README.md`'s troubleshooting section.
- **Priority is request-level, not deployment-level.** When tempted to put routing/queueing policy on a runtime artifact (Deployment, Instance), check whether the property describes the artifact itself or the *traffic flowing through it*. If traffic, it belongs at the routing layer, not on the artifact. See `protos/provisioner/v1/types.proto`'s reserved-field-22 comment for the receipt; PR 131 corrected this mid-review.

## CLI surface

Single binary `iplane` with cobra subcommands. The Docker image
`ENTRYPOINT` is the same binary, default `CMD` is `serve`.

| Subcommand           | Purpose                                                |
| -------------------- | ------------------------------------------------------ |
| `iplane serve`       | Run the control plane (gRPC + HTTP + v0.2 router on :8080) |
| `iplane up`          | One-shot: provision + deploy + chat REPL + teardown (the Ch 6 flagship) |
| `iplane instance`    | `create` / `list` / `describe` / `destroy` / `ssh` / `wait` (in-process state file OR `--service-url <remote>`) |
| `iplane deployment`  | `deploy` / `describe` / `destroy` / `list` / `query` / `wait` / `watch` / `models` / `status` / `touch` |
| `iplane telemetry`   | `url` — discover the cloudflared tunnel URL (for engine OTLP propagation) |
| `iplane load`        | Fire synthetic OpenAI traffic                          |
| `iplane gen-names`   | Regenerate Go consts + book LaTeX from `metric-names.yaml` |

`--config <path>` is a persistent flag. Each subcommand has its own
flags; `iplane <cmd> --help` for the full list. State-changing
subcommands (`instance create`, `instance destroy`) accept `--dry-run`
to preview the action without provider calls or state-file writes —
see [docs/cli-dry-run.md](docs/cli-dry-run.md) for the pattern.

## Env vars

Viper binds env vars with the `IPLANE_` prefix; nested config keys
flatten to underscore (so `deployment.provider` → `IPLANE_DEPLOYMENT_PROVIDER`).

| Var                                | Purpose                                  |
| ---------------------------------- | ---------------------------------------- |
| `IPLANE_BACKEND_ENGINE`            | `mock` (default) or `vllm`               |
| `IPLANE_BACKEND_URL`               | Backend base URL (vllm only)             |
| `IPLANE_SERVICE_URL`               | `iplane instance` / `iplane deployment` remote transport (e.g., `http://localhost:8080`); in-process state file when unset |
| `IPLANE_RUNPOD_DEBUG`              | `1` logs RunPod HTTP request/response bytes (sans Authorization) to stderr |
| `IPLANE_SKIP_MODEL_VALIDATION`     | `1` bypasses the HF pre-flight check on `CreateDeployment` (offline / firewalled / non-HF models) |
| `IPLANE_OTEL_ENDPOINT`             | OTLP URL **propagated to the engine pod** as `OTEL_EXPORTER_OTLP_ENDPOINT`. Either a hosted OTLP URL or `$(iplane telemetry url)` for the cloudflared tunnel. |
| `IPLANE_OTEL_HEADERS`              | Comma-separated `KEY=VALUE` auth headers paired with `IPLANE_OTEL_ENDPOINT`. Required for hosted providers; unused for the tunnel. |
| `IPLANE_DEPLOYMENT_PROVIDER` / `_GPU_TYPE` / `_BILLING_MODE` / `_INSTANCE_ID` | Cost-metric labels |
| `OTEL_EXPORTER_OTLP_ENDPOINT`      | OTLP collector address for `iplane serve` itself (control-plane traces/metrics) |
| `HF_TOKEN`                         | Propagated to engine pods for gated-model fetches; HF pre-flight check also uses it for gated-model existence probes |
| `RUNPOD_API_KEY`                   | Required for `iplane instance create runpod ...` — must be a new-style scoped key (`rpa_...` prefix) with **Full** access (REST scope is NOT covered by legacy keys or `api.runpod.ai`-only scopes — both silently 401 on `rest.runpod.io/v1`) |
| `IPLANE_PROVIDER`                  | Default provider for CLI commands and demo binaries when `--provider` is omitted. Falls back to `runpod` if unset (preserves Ch 6 behavior). Example: `IPLANE_PROVIDER=vast iplane deployment deploy llama --model ...` |
| `VAST_API_KEY`                     | Required when `IPLANE_PROVIDER=vast` (or `--provider vast`). Lands with PR for #150. |
| `LAMBDA_API_KEY`                   | Required when `IPLANE_PROVIDER=lambdalabs` (or `--provider lambdalabs`). Lands with PR for #151. |

Future provider API keys (not yet implemented): `EQUINIX_AUTH_TOKEN`, `EQUINIX_PROJECT_ID`. See `.env.local.example`.

The provider→API-key mapping lives in `internal/provisioners/apikey.go` (`ProviderAPIKeyEnv`, `EnsureProviderAPIKey`). Add new providers there; cmd/ and examples/common/ pick them up automatically.

## Stack dependencies

- `github.com/panyam/servicekit` — graceful shutdown + HTTP middleware (Tier-1, mature)
- `connectrpc.com/connect` — gRPC + Connect + HTTP/JSON on one handler
- OpenTelemetry Go SDK + OTLP/gRPC exporters
