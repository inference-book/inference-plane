## inference-plane

Reference implementation of the control plane for *Inference Is All You Need* (Apress, 2026). See [README.md](README.md) for the layout, [ARCHITECTURE.md](ARCHITECTURE.md) for the design, and [RELEASE.md](RELEASE.md) for the branch / tag / forward-merge / cherry-pick workflow.

## Quick-ref commands

| Command            | Purpose                                                |
| ------------------ | ------------------------------------------------------ |
| `make help`        | List all targets                                       |
| `make infra-up`    | Bring up infra only (obs services); pair with per-demo `make serve` |
| `make infra-down`  | Tear down infra services                               |
| `cd examples/<demo> && make serve` | Run `iplane serve` with that demo's config.yaml (falls back to `deploy/config.yaml` if none) |
| `make up`          | Full stack incl. controlplane container (`--profile fullstack`; readers' path) |
| `make down`        | Tear the full stack down                               |
| `make rebuild`     | Rebuild local Docker images without starting           |
| `make smoke`       | Go integration tests against a live stack              |
| `make load`        | Synthetic traffic generator (works against mock or vllm) |
| `make test`        | Unit tests (no live stack needed)                      |
| `make build`       | Compile `bin/iplane`                                   |
| `make check-pins`  | Verify `pinned-versions.env` matches book's `.tex`     |
| `make check-names` | Verify generated names match `metric-names.yaml`       |
| `make check-constraints` | Verify architectural constraints (CP/DP-1, ...)  |
| `make gen-names`   | Regenerate `internal/telemetry/names.go` + book `.tex` |
| `cd protos && make gen` | Regenerate proto code into `gen/`                 |

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
- **RunPod machine field**: freshly-rented pods return `"machine": {}` empty from the follow-up GET; the populated record arrives a few seconds later. Adapter's `gpuSKU` / `gpuVRAMGB` helpers are nil-defensive. **"A few seconds" is optimistic** — traced live 2026-08-09, `machine` stayed `{}` for a whole 2m14s deploy while the **top-level `machineId`** populated immediately, so treat `machineId` as the host-assignment signal (`machinePresent()` keys on both).
- **Cold-start phase boundaries must be state, not timestamps** (issue 208, fixed 2026-08-09). RunPod's `lastStartedAt` reads like "container started" and is not: it is stamped at pod-record creation, 5-7 ms *before* `createdAt`, on every pod observed. Keying the ladder on it made `classifyEnginePhase` short-circuit to `engine:init` on the first probe, so `runpod:scheduling` and `runpod:image-pull` were structurally unreachable and image-pull measured exactly 0s on every run (including both book A/B runs). The real container-start signal is the **`runtime` block on v2 `GET /v2/pods/{id}`**, null until the container runs; REST v1 omits `runtime` entirely, GraphQL v1 and REST v2 both have it. Two sub-traps: RunPod returns present-but-empty objects (`"machine": {}`, and `runtime` can arrive the same way) so presence checks must test for real content; and `runtime.uptimeInSeconds` is coarse/cached (read 37 three times across 24s) so it is evidence, not a clock. After the fix, a 1.5B cold start splits 109s image-pull / 43s engine-init — **the image pull is ~70% of a small-model cold start, and a warm deploy pays all of it**. Debug with `IPLANE_RUNPOD_PHASE_TRACE=1`.
- **`make check-names` false-positive locally**: when the sibling `../book/` checkout exists, `git diff --quiet -- internal/X ../book/Y` flips into compare-two-files mode and trips spuriously. CI (no book checkout) is unaffected. Tracked as issue #108; do not chase the diff it prints unless `make gen-names` actually changed `internal/telemetry/names.go`.
- **Examples assume a prebuilt `iplane`; they do not build it.** The CLI-driving demos (02, 04) resolve a binary via `common.ResolveIplane` (`--bin` → `$IPLANE_BIN` → repo `bin/iplane` → `$PATH`) and fail fast with a `make build` hint if none exist. They used to compile `cmd/iplane` on the fly, but once `examples/` became its own module that `replace`s the parent, building `cmd/iplane` from inside the examples module pulled the CLI's full transitive deps into `examples/go.sum`. Don't reintroduce an in-demo build; if you need to drive the local checkout, run `make build` (or `make install`) first.
- **Scheduler defaults to OFF.** `router.queue.servicers` defaults to `0` in `deploy/config.yaml`, which means no scheduler is constructed and the router takes the direct-forward path (Beat 1 behavior). The v0.2 release/v0.2 snapshot ships with this default to avoid surprising operators on existing deploys; demo 05 requires `servicers > 0` (and `in_flight_cap > 0` for the queue-pressure story to be visible). Documented in `examples/05-fair-queueing/README.md`'s troubleshooting section.
- **Priority is request-level, not deployment-level.** When tempted to put routing/queueing policy on a runtime artifact (Deployment, Instance), check whether the property describes the artifact itself or the *traffic flowing through it*. If traffic, it belongs at the routing layer, not on the artifact. See `protos/provisioner/v1/types.proto`'s reserved-field-22 comment for the receipt; PR 131 corrected this mid-review.
- **`stores/file` `Update`/`Read` are goroutine-safe only via an in-process `sync.Mutex`.** The flock guards across *processes*; under `LockForLifetime` (`iplane serve`) the flock is held once and individual `Update` calls skip re-acquiring it, so without the mutex two concurrent goroutines both read-modify-write and the last writer clobbers the other (lost update). This bit multi-replica-from-scratch deploys whose per-slot endpoint patches land near-simultaneously (external provider's instant deploy); slow cloud provisioning and demo 06's one-at-a-time scaling hid it. Fixed + regression-tested in `internal/provisioners/stores/file/`. Don't remove the mutex, and don't call `store.Update`/`Read` from *inside* an `Update` closure (re-entrant lock → deadlock).
- **`engine_endpoint` (singular) is legacy; `engine_endpoints` (plural) is the real per-replica set.** The plural, parallel to `instance_ids`, is what the router routes over (`effectiveEndpoints`). The singular is only stamped for the `count==1` case (`recordCreateSlots`) and by slot-0's deploy emit. A multi-replica deployment created *directly* (not scaled up from 1) never stamps the singular — so any readiness/eligibility check must be plural-aware (`hasStampedEndpoint`), not singular-only. Demo 06 dodged this by starting at 1 replica then scaling; the external provider (first from-scratch multi-replica path) exposed it.
- **External is a non-owning provider — its hollow lifecycle methods are intentional.** `internal/provisioners/external` registers a RUNNING deployment pointing at an operator-managed engine URL (no provisioning). `Terminate` detaches (never destroys the engine), `Describe`/`List` are empty, `Spawn`/`Deploy` fabricate from the endpoint. It skips the GPU-requirement gate, the image requirement, and model validation (nothing to fetch). Don't "fix" the empty methods. See its package doc; the hosted-API provider (Ch 11, issue 182) is external + auth + per-token cost.
- **Prefix-affinity (Ch 8) essentials.** Toggle via `router.routing_policy: round_robin | prefix_affinity` on `iplane serve` (config/env, a startup property — not per-request). `router.affinity_overload_threshold` (0 = off) enables the load-aware override: when a pinned replica has >= N in-flight, spill that turn to the coolest replica but *keep the pin* (temporary detour; the session snaps back when it cools). Affinity keys on `X-IPlane-Session`; header-less clients (a plain OpenAI SDK) get a body-derived key — `hash(first system msg + first user msg)` — but ONLY on the flat `/v1/chat/completions` URL, which already parses the body for `model`. The deploy-id URL streams the body unparsed and stays header-only. The `iplane.router.affinity.total` hit-rate is a router-side routing-locality **proxy**, not the engine's `gpu_prefix_cache_hit_rate` (real engine scraping is deferred to issue 51); don't conflate them. `iplane mock-engine --latency 3ms` keeps routing demos fast — see `examples/07-prefix-affinity/`.

- **Warm-cache pinning (Ch 9) essentials.** A model is pre-staged onto a provider volume (`iplane model pin`), so a deploy mounts the weights instead of re-downloading them. The seam is deliberately provider-agnostic: `provisionerv1.VolumeMount` / `modelstores.Mount` carry `{volume_id, host_path, mount_path, provider}` and never a provider primitive — the RunPod adapter maps a mount onto `networkVolumeId`, the sshdocker executor onto a `docker run -v` bind, and providers with no volume mechanism take the cold path. `provider` on the mount is enforced: `CreateDeployment` refuses a mount whose provider ≠ the replica's placement provider (no silent cold fallback). A volume is a **shared cache** (many models under one HF layout, one per-region record in `State.Volumes`); pinning is additive. Deploy **auto-resolves** a pinned volume by `(model, provider, region)` from the registry when no `model_cache` config mount is set — config wins, heterogeneous fleets stay cold. Cold-start is sliced by a `storage_tier` (cold|warm) label on the deploy phase/provision metrics (see the deployment Grafana dashboard's "Engine-init: warm vs cold" panel). **Pin runs in-process before `iplane serve`** (the daemon holds the state-dir flock); remote `--service-url` pin is a follow-up. **`RunPodVolumeStore`-era naming is dead** — the generic store is `internal/modelstores/volumecache`. See [docs/modelstore.md](docs/modelstore.md).
- **RunPod staging completion is detected via v2 pod logs, not pod status** (validated live 2026-07-28). RunPod gives NO signal that a one-shot pod's command finished: v1 `desiredStatus` stays `RUNNING`, and even v2 `status` stays `RUNNING` because the exited container is auto-restarted. So `internal/provisioners/runpod/stage.go`'s `StageModel` runs `hf download` (NOT the removed `huggingface-cli`), has the command print `__IPLANE_STAGE_DONE__` / `__IPLANE_STAGE_FAIL__` then `sleep infinity`, and `waitForStageComplete` polls `GET api.runpod.io/v2/pods/{id}/logs` (SSE, `WithLogsBaseURL`-overridable) for the marker via the pure `stageSignal` scanner; the `defer Terminate` still cleans the pod. Fake-harness tested against the logs endpoint. **Large models also need `hf download --max-workers 2`** (in the staging command): the CPU staging pod has only ~4 GB RAM, and the default 8 parallel workers streaming multi-GB shards through the FUSE volume mount OOM-kill the download (SIGKILL, rc 137) on a 70B+. A small model is unaffected; validated live on 72B FP8.
- **Real deploys need three timeouts aligned, not one.** A cold RunPod deploy is image-pull-bound (~10 GB engine image on community capacity) and routinely runs 4-12 min, so: (a) the CLI `deployment deploy --timeout` (client ctx, default 8m) must cover it; (b) the daemon's `server.write_timeout_sec` (default 600) must exceed the wait or `CreateDeployment` severs with `unexpected EOF` mid-provision; (c) the provider engine-`/health` wait (`runpod.WithEngineReadyTimeout`, default 10m, now wired to `IPLANE_RUNPOD_ENGINE_READY_TIMEOUT`) must not fail a slow-but-fine pull; and (d) the **container disk** (`--min-disk-gb`) must fit image + downloaded weights or a big-model cold deploy fills the disk mid-download and fails. The RunPod deployer sizes `ContainerDiskInGB` from `min_disk_gb` (was hardcoded 20 GB); `min_disk_gb` now **sizes the container disk and no longer filters SKUs** (`DefaultDiskGb` was never a per-SKU ceiling — disk is an independent create param). `examples/08-scaling-30b/08a-cold-start-distance/` sets the three timeouts (20-45m) and `--min-disk-gb` (150 for a 72B FP8). Cross-cutting: the 72B FP8 cold-start run needed a **Blackwell-capable vLLM image** (`vllm/vllm-openai:v0.26.0-cu129`, not the demo default `v0.7.0`) because the only available FP8 card was a B200; FP8 needs Hopper/Ada/Blackwell.

## CLI surface

Single binary `iplane` with cobra subcommands. The Docker image
`ENTRYPOINT` is the same binary, default `CMD` is `serve`.

| Subcommand           | Purpose                                                |
| -------------------- | ------------------------------------------------------ |
| `iplane serve`       | Run the control plane (gRPC + HTTP + v0.2 router on :8080) |
| `iplane up`          | One-shot: provision + deploy + chat REPL + teardown (the Ch 6 flagship) |
| `iplane instance`    | `create` / `list` / `describe` / `destroy` / `ssh` / `wait` (in-process state file OR `--service-url <remote>`) |
| `iplane deployment`  | `deploy` / `describe` / `destroy` / `list` / `query` / `wait` / `watch` / `models` / `status` / `touch`. `deploy --provider external --engine-endpoints url1,url2` attaches to operator-run engines (one replica per URL, no provisioning) |
| `iplane telemetry`   | `url` — discover the cloudflared tunnel URL (for engine OTLP propagation) |
| `iplane model`       | `pin` / `ls` / `unpin` — warm-cache pin registry (Ch 9). `pin <model> --provider --region` pre-stages weights onto a provider volume so deploys mount instead of re-downloading. In-process only (pin **before** `iplane serve`; the daemon holds the state-dir lock). See [docs/modelstore.md](docs/modelstore.md) |
| `iplane load`        | Fire synthetic OpenAI traffic. `iplane load session` is the closed-loop multi-turn driver (`--sessions/--turns/--think-time`, stamps `X-IPlane-Session`) for the Ch 8 sticky-routing demo |
| `iplane mock-engine` | (hidden, dev/CI) standalone OpenAI-compatible mock engine over the mock backend; pair with `--provider external` for a GPU-free multi-replica harness. `--latency <dur>` for a fixed fast latency (routing demos); default is the realistic bimodal-with-tail mixture |
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
| `IPLANE_RUNPOD_PHASE_TRACE`        | `1` prints one TSV line per engine-readiness tick with the raw cold-start signals (`elapsed`, `phase`, `machine`, `createdAt`, `lastStartedAt`, `health`) and keeps probing pod status past engine-init. Diagnostic for issue 208; observability-only, never changes phase emission or readiness |
| `IPLANE_RUNPOD_ENGINE_READY_TIMEOUT` | Go duration (e.g. `20m`) overriding the RunPod engine-`/health` wait (default 10m). Extend for large images / slow cold pulls. Read at `iplane serve` startup |
| `IPLANE_SKIP_MODEL_VALIDATION`     | `1` bypasses the HF pre-flight check on `CreateDeployment` (offline / firewalled / non-HF models) |
| `IPLANE_OTEL_ENDPOINT`             | OTLP URL **propagated to the engine pod** as `OTEL_EXPORTER_OTLP_ENDPOINT`. Either a hosted OTLP URL or `$(iplane telemetry url)` for the cloudflared tunnel. |
| `IPLANE_OTEL_HEADERS`              | Comma-separated `KEY=VALUE` auth headers paired with `IPLANE_OTEL_ENDPOINT`. Required for hosted providers; unused for the tunnel. |
| `IPLANE_DEPLOYMENT_PROVIDER` / `_GPU_TYPE` / `_BILLING_MODE` / `_INSTANCE_ID` | Cost-metric labels |
| `OTEL_EXPORTER_OTLP_ENDPOINT`      | OTLP collector address for `iplane serve` itself (control-plane traces/metrics) |
| `HF_TOKEN`                         | Propagated to engine pods for gated-model fetches; HF pre-flight check also uses it for gated-model existence probes |
| `RUNPOD_API_KEY`                   | Required for `iplane instance create runpod ...` — must be a new-style scoped key (`rpa_...` prefix) with **Full** access (REST scope is NOT covered by legacy keys or `api.runpod.ai`-only scopes — both silently 401 on `rest.runpod.io/v1`) |
| `IPLANE_PROVIDER`                  | Default provider for CLI commands and demo binaries when `--provider` is omitted. Falls back to `runpod` if unset (preserves Ch 6 behavior). Example: `IPLANE_PROVIDER=vast iplane deployment deploy llama --model ...` |
| `VAST_API_KEY`                     | Required when `IPLANE_PROVIDER=vast` (or `--provider vast`). Bearer-token auth on `console.vast.ai`. |
| `LAMBDA_API_KEY`                   | Required when `IPLANE_PROVIDER=lambdalabs` (or `--provider lambdalabs`). HTTP-Basic auth on `cloud.lambdalabs.com` (apiKey as username, empty password). |

Future provider API keys (not yet implemented): `EQUINIX_AUTH_TOKEN`, `EQUINIX_PROJECT_ID`. See `.env.local.example`.

The provider→API-key mapping lives in `internal/provisioners/apikey.go` (`ProviderAPIKeyEnv`, `EnsureProviderAPIKey`). Add new providers there; cmd/ and examples/common/ pick them up automatically.

## Stack dependencies

- `github.com/panyam/servicekit` — graceful shutdown + HTTP middleware (Tier-1, mature)
- `connectrpc.com/connect` — gRPC + Connect + HTTP/JSON on one handler
- OpenTelemetry Go SDK + OTLP/gRPC exporters
