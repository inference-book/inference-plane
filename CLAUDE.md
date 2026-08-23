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
| `make load`        | Synthetic traffic generator; defaults to `MODEL=mock/mock`, pass `MODEL=<exact string>` for a real deployment |
| `make test`        | Unit tests (no live stack needed)                      |
| `make build`       | Compile `bin/iplane`                                   |
| `make dist`        | Cross-compile `dist/iplane-linux-{amd64,arm64}` with the version stamped in (what the engine agent is fetched from) |
| `make check-pins`  | Verify `pinned-versions.env` matches book's `.tex`     |
| `make check-names` | Verify generated names match `metric-names.yaml`       |
| `make check-constraints` | Verify architectural constraints (CP/DP-1, ...)  |
| `hack/capacity-sample.sh` | Sample what every provider would rent, appended to JSONL. Free and read-only; see [hack/README.md](hack/README.md) |
| `make gen-names`   | Regenerate `internal/telemetry/names.go` + book `.tex` |
| `cd protos && make gen` | Regenerate proto code into `gen/`                 |

## Conventions

- **Generated code is committed** (`gen/`, `internal/telemetry/names.go`, book's `metric-names.tex`). Regen + commit together; `make check-names` and `make check-pins` run as CI gates.
- **Versioned releases** map to book parts: `release/v0.1` (Ch 6), `release/v0.2` (Ch 7–10), `release/v0.3` (Ch 11–12), `release/v0.4` (Ch 13–15). `v1.0` carries no chapters and is cut after the book is done. Tag `vX.Y.Z` per chapter plus an immutable `chNN-final`. See [RELEASE.md](RELEASE.md) for the lifecycle (active branch is a moving snapshot until the chapter is cut; revisits cherry-pick forward).
- **gRPC server is source of truth.** Connect-RPC adapters and grpc-gateway are HTTP bindings on top — both dial the in-process gRPC server.
- **Build only what can be tried on real hardware.** A capability that can only ever be demonstrated against a mock is a claim the book cannot support. Mocks stay as scaffolding for behaviour a real provider can also produce (`mock-engine` keeps the routing demos free and the real runs have happened). See [ROADMAP.md](ROADMAP.md#build-only-what-can-be-tried-on-real-hardware) for the rule and the two capabilities it currently rules out.

- **Architectural constraints in [CONSTRAINTS.md](CONSTRAINTS.md).** Project-wide rules enforced by `make check-constraints`. **CP/DP-1**: data-plane code (`internal/router/`, `internal/dataplane/`) reaches control-plane state only via the generated gRPC client — never via direct `internal/provisioners` import. Makes the eventual data-plane split mechanical, not a refactor. New rules get extracted from real friction, not invented speculatively.
- **Binaries are version-stamped.** `make build` and `make install` both pass `VERSION_LDFLAGS`, because `iplane load --sweep` records the build that produced a measurement and an unstamped binary writes `dev` into a data artifact a book figure is drawn from (#347). `go run ./cmd/iplane` still reports `dev`; use a built binary for anything whose numbers will be published.
- **Dashboards are checked against the metric vocabulary.** `tests/dashboards` parses every panel's PromQL and fails the suite on a metric `metric-names.yaml` does not declare, plus duplicate uids and query panels with no datasource. A panel naming a metric nobody emits renders as an empty graph, which is invisible in review and obvious only in a published screenshot.
- **Check a ticket's acceptance criteria against the code before building to them.** Four in the frontier-MoE epic did not survive contact: #342 asked for a flag vLLM has no such thing for, #343 asked to refuse a degree that divides evenly, #350 asked for a catalog entry in a catalog that does not exist, and #352 asked to provision through an API the provider does not publish. Read the ticket, then check it; a wrong premise costs a rental, not a review round.
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
- **Driving the CLI in a script: three traps that cost a paid rental between them.** `--service-url` **defaults to `http://localhost:8080`**, and `--state-dir` is ignored whenever it is set, so an in-process invocation needs `--service-url ""` explicitly or it silently talks to whatever daemon is up. `deployment describe` has `--output`, no `-o` shorthand, so `-o json` errors and a script parsing its stdout gets an empty string. And `grpcAddr` is a hardcoded `127.0.0.1:9090` const with no flag, so **only one `iplane serve` can run per machine**; a script that needs its own state should drive the CLI in-process rather than standing up a second daemon. Any readiness guard built on these must fail *safe*: read "could not determine the state" as "keep going", never as "it failed". Reading it as failure tore down a healthy 72B mid-run.
- **Packages carry their own lore; read the NOTES.md before trusting a field.** `internal/vrambudget/NOTES.md` (the fit arithmetic), `internal/modelstores/huggingface/NOTES.md` (the hub read), `internal/router/NOTES.md` (the scheduler default, prefix-affinity, the endpoint plurality trap), `internal/backends/NOTES.md` (what the mock does and does not model), and per-provider: `internal/provisioners/vast/NOTES.md` (marketplace host selection, the two real broken-host failures, fabric measurement, cold-start phases, volumes) and `internal/provisioners/runpod/NOTES.md` (status fields that describe the rental rather than the container, staging via the v2 log stream, what RunPod cannot report at all). Read the one for the adapter you are touching before trusting a status field.
- **`make check-names` false-positive locally**: when the sibling `../book/` checkout exists, `git diff --quiet -- internal/X ../book/Y` flips into compare-two-files mode and trips spuriously. CI (no book checkout) is unaffected. Tracked as issue #108; do not chase the diff it prints unless `make gen-names` actually changed `internal/telemetry/names.go`.
- **Examples assume a prebuilt `iplane`; they do not build it.** The CLI-driving demos (02, 04) resolve a binary via `common.ResolveIplane` (`--bin` → `$IPLANE_BIN` → repo `bin/iplane` → `$PATH`) and fail fast with a `make build` hint if none exist. They used to compile `cmd/iplane` on the fly, but once `examples/` became its own module that `replace`s the parent, building `cmd/iplane` from inside the examples module pulled the CLI's full transitive deps into `examples/go.sum`. Don't reintroduce an in-demo build; if you need to drive the local checkout, run `make build` (or `make install`) first.
- **`stores/file` `Update`/`Read` are goroutine-safe only via an in-process `sync.Mutex`.** The flock guards across *processes*; under `LockForLifetime` (`iplane serve`) the flock is held once and individual `Update` calls skip re-acquiring it, so without the mutex two concurrent goroutines both read-modify-write and the last writer clobbers the other (lost update). This bit multi-replica-from-scratch deploys whose per-slot endpoint patches land near-simultaneously (external provider's instant deploy); slow cloud provisioning and demo 06's one-at-a-time scaling hid it. Fixed + regression-tested in `internal/provisioners/stores/file/`. Don't remove the mutex, and don't call `store.Update`/`Read` from *inside* an `Update` closure (re-entrant lock → deadlock).
- **The shared deploy path has its own lore: [internal/provisioners/NOTES.md](internal/provisioners/NOTES.md).** The three timeouts a real deploy has to align, the `enginewait` loop and what a tick can see, `FailureReporter` and why RunPod deliberately does not implement it, how cost is measured from the instance (and why a rate is stamped at rent time rather than when the engine serves), the two VRAM figures and which labels are binary, why a provider panic is contained rather than fatal, and why `external`'s hollow lifecycle methods are deliberate.

- **Warm-cache pinning (Ch 9).** A model is pre-staged onto a provider volume (`iplane model pin`) so a deploy mounts the weights instead of re-downloading them. The seam is provider-agnostic and there are two fail-closed guards, both about refusing rather than silently running cold. See [docs/modelstore.md](docs/modelstore.md).
- **Interconnect health (Ch 10) essentials.** A tensor-parallel group that loses one NVLink keeps serving *correct* tokens at a fraction of the speed, and nothing else in the system can see it: `/health` says serving (true), in-flight counts look normal, no memory or utilisation gauge moves. `internal/engineagent/nvlink.go` reads `nvidia-smi nvlink -s`; `provisionerv1.InterconnectHealth` rides on `EngineNode` (per node, since links are one machine's hardware); `iplane fleet status` shows a LINKS column. **`available=false` is the load-bearing field**: a PCIe box and a container without the NVIDIA tooling both mean *no reading*, never *zero links up* — collapse that and every PCIe pool invents a fault. The threshold is deliberately just "fewer up than reported"; a link up but *retrying hard* needs per-link error counters and a threshold nobody can yet calibrate. Composed via `engineagent.AnyDegraded`, because `HTTPProbe` structurally cannot return Degraded. Simulate GPU-free with `iplane mock-engine --links 8 --degrade-after 10s`.
- **A read that depends on privileged inputs is a service operation.** The test for what needs an RPC is *where the inputs live*, not whether the call writes. Provider credentials live in the daemon's environment and the state file is the daemon's, so a verb reading either belongs behind the wire contract even when it mutates nothing. Applying the write-shaped test instead ("does the daemon hold the state lock") shipped the same bug twice: `iplane capacity` answered from the CLI host while claiming to have asked a remote control plane (#304), and `iplane model ls` refused to list because a daemon was running (#307). A lock-free *local* read is safe because `stores/file` writes through a temp file and an atomic rename, so a reader without the flock is stale and never torn; that safety depends on the rename and would quietly stop holding if a write path skipped it.

- **Whether a model fits is arithmetic, and it lives in one package.** `internal/vrambudget` sums four claims per card and returns `fits | tight | overcommitted`; `iplane model budget` / `model describe` are the operator surface and the deploy path reads the same arithmetic before renting (#326). The lore that will bite you is in [internal/vrambudget/NOTES.md](internal/vrambudget/NOTES.md) and [internal/modelstores/huggingface/NOTES.md](internal/modelstores/huggingface/NOTES.md).
- **Only filter on facts a catalog row genuinely bounds.** VRAM and fabric yes; disk, system RAM and reclaimability are properties of the rented shape rather than the card, so filtering on them can only wrongly exclude. Learned three times (#281, #283, #288); the rule and all three receipts are in `internal/provisioners/skucatalog/skucatalog.go`.


- **Format only what you touch.** `gofmt -l` flags roughly 25 pre-existing files including generated code, so `gofmt -w <dir>` silently reformats files the change has nothing to do with and they land in the diff. Run it per-file. (Walked into twice in one session; `git diff --cached --name-only` before committing catches it.)
- **The mock engine is not a capacity model unless you configure one.** By default it admits everything and a concurrency sweep against it is a straight line. `--kv-budget-tokens` gives it a ceiling and `--token-latency` makes inter-token latency measurable. See [internal/backends/NOTES.md](internal/backends/NOTES.md).
- **Open loop and closed loop answer different questions.** `iplane load --rps` fixes the arrival rate; `--sweep` holds N in flight and makes the rate an output. Method, steady-state detection, the committed artifact format and cost per token are all in [docs/load-measurement.md](docs/load-measurement.md).

## CLI surface

Single binary `iplane` with cobra subcommands. The Docker image
`ENTRYPOINT` is the same binary, default `CMD` is `serve`.

| Subcommand           | Purpose                                                |
| -------------------- | ------------------------------------------------------ |
| `iplane serve`       | Run the control plane (gRPC + HTTP + v0.2 router on :8080) |
| `iplane up`          | One-shot: provision + deploy + chat REPL + teardown (the Ch 6 flagship) |
| `iplane instance`    | `create` / `list` / `describe` / `destroy` / `ssh` / `wait`. `create auto <id>` picks the cheapest fitting candidate across every configured provider and records the vendor it chose (in-process state file OR `--service-url <remote>`) |
| `iplane deployment`  | `deploy` / `describe` / `destroy` / `list` / `query` / `wait` / `watch` / `models` / `status` / `touch` / `migrate`. `migrate <id> --to <provider>` moves a running deployment without changing its id: grows onto the destination, waits for it to serve, then drains the source. `--dry-run` says whether the warm cache follows. `deploy --provider external --engine-endpoints url1,url2` attaches to operator-run engines (one replica per URL, no provisioning) |
| `iplane telemetry`   | `url` — discover the cloudflared tunnel URL (for engine OTLP propagation) |
| `iplane model`       | `pin` / `ls` / `unpin` / `describe` / `budget`. Warm-cache pin registry (Ch 9): `pin <model> --provider --region` pre-stages weights onto a provider volume so deploys mount instead of re-downloading. `pin`/`unpin` are in-process and need the state lock, so pin **before** `iplane serve`; the read verbs take no lock and honour `--service-url` (#307). `describe` reports a model's trained shape, including the expert shape and per-step active parameters for a mixture-of-experts model; `budget <model> --vram-gb N` answers whether it fits, per card count, renting nothing (Ch 12), and `budget --sessions-at 8k,128k,1M` answers how many concurrent sessions fit instead. `budget --expert-parallel` sizes the other arrangement, where a row is the expert width and `--tp` is the tensor width inside it. See [docs/modelstore.md](docs/modelstore.md) |
| `iplane capacity`    | `--provider a,b,c` / `--all` — ask providers what they would rent, renting nothing. Read-only and free by contract; takes no state lock, so it answers while `serve` is running. Providers that cannot answer say so rather than returning an empty list. `--reclaim yes` prices the interruptible tier; `--fabric` drops candidates whose interconnect nobody vouched for. RunPod rows carry the datacenter and `datacenter_storage`, which is what says where a warm cache can live (#399) |
| `iplane fleet`       | `status` / `drain` — verbs over engines that registered themselves via the control channel. A member is one engine (one endpoint, one model) over a span of cards and nodes; single-card and distributed engines appear in the same list, differing only by the span column. The LINKS column carries interconnect health (Ch 10) |
| `iplane engine-agent`| Runs next to an engine on the rented box and registers it, renewing on the cadence the control plane returns. Silence past the lease is how the control plane learns the engine is gone. Reads its own card count and link health; everything else (identity, endpoint) is injected at deploy time |
| `iplane load`        | Fire synthetic OpenAI traffic. `--rps` is the open loop (fixed arrival rate); `--sweep 1,2,4,8` is the closed loop that walks a concurrency ladder, waits for steady state, discards the warm-up and reports throughput, achieved batch and latency/TTFT/ITL percentiles. `--prompt-tokens N` sets context length from a public-domain corpus. `iplane load session` is the closed-loop multi-turn driver (`--sessions/--turns/--think-time`, stamps `X-IPlane-Session`) for the Ch 8 sticky-routing demo. See [docs/load-measurement.md](docs/load-measurement.md) |
| `iplane mock-engine` | (hidden, dev/CI) standalone OpenAI-compatible mock engine over the mock backend; pair with `--provider external` for a GPU-free multi-replica harness. `--latency <dur>` for a fixed fast latency (routing demos); default is the realistic bimodal-with-tail mixture. `--links N` simulates an NVLink board so the Ch 10 degraded-not-dead state and the `fleet status` LINKS column are demonstrable GPU-free. `--kv-budget-tokens N` gives it a capacity ceiling that falls as context rises; `--token-latency <dur>` paces streamed frames so inter-token latency is measurable. Both default off |
| `iplane gen-names`   | Regenerate Go consts + book LaTeX from `metric-names.yaml` |

`--config <path>` is a persistent flag. Each subcommand has its own
flags; `iplane <cmd> --help` for the full list. State-changing
subcommands (`instance create`, `instance destroy`) accept `--dry-run`
to preview the action without provider calls or state-file writes —
see [docs/cli-dry-run.md](docs/cli-dry-run.md) for the pattern.

## Env vars

Viper binds env vars with the `IPLANE_` prefix; nested config keys
flatten to underscore (so `backend.engine` → `IPLANE_BACKEND_ENGINE`).

| Var                                | Purpose                                  |
| ---------------------------------- | ---------------------------------------- |
| `IPLANE_BACKEND_ENGINE`            | `mock` (default) or `vllm`               |
| `IPLANE_BACKEND_URL`               | Backend base URL (vllm only)             |
| `IPLANE_SERVICE_URL`               | `iplane instance` / `iplane deployment` remote transport (e.g., `http://localhost:8080`); in-process state file when unset |
| `IPLANE_RUNPOD_DEBUG`              | `1` logs RunPod HTTP request/response bytes (sans Authorization) to stderr |
| `IPLANE_RUNPOD_PHASE_TRACE`        | `1` prints one TSV line per engine-readiness tick with the raw cold-start signals (`elapsed`, `phase`, `machine`, `createdAt`, `lastStartedAt`, `health`) and keeps probing pod status past engine-init. Diagnostic for issue 208; observability-only, never changes phase emission or readiness |
| `IPLANE_ENGINE_READY_TIMEOUT`       | Go duration overriding how long ANY deploy path waits for the engine to answer `/health`. Applies to the sshdocker (Vast, Lambda) path as well as RunPod. Default 10m on both |
| `IPLANE_ENGINE_ABORT_ON_SLOW_DOWNLOAD` | `0` stops a deploy abandoning itself when the agent's measured download rate projects past the engine-ready deadline. On by default. Only ever fires on a positive, sustained, clearly-too-slow rate; a stalled download runs to the deadline as before |
| `IPLANE_RUNPOD_ENGINE_READY_TIMEOUT` | Same, scoped to RunPod, and wins over the generic one when both are set. Kept for operators who already export it |
| `IPLANE_SKIP_MODEL_VALIDATION`     | `1` bypasses the HF pre-flight check on `CreateDeployment` (offline / firewalled / non-HF models) |
| `IPLANE_OTEL_ENDPOINT`             | OTLP URL **propagated to the engine pod** as `OTEL_EXPORTER_OTLP_ENDPOINT`. Either a hosted OTLP URL or `$(iplane telemetry url)` for the cloudflared tunnel. |
| `IPLANE_OTEL_HEADERS`              | Comma-separated `KEY=VALUE` auth headers paired with `IPLANE_OTEL_ENDPOINT`. Required for hosted providers; unused for the tunnel. |
| `OTEL_EXPORTER_OTLP_ENDPOINT`      | OTLP collector address for `iplane serve` itself (control-plane traces/metrics) |
| `HF_TOKEN`                         | Propagated to engine pods for gated-model fetches; HF pre-flight check also uses it for gated-model existence probes |
| `RUNPOD_API_KEY`                   | Required for `iplane instance create runpod ...` — must be a new-style scoped key (`rpa_...` prefix) with **Full** access (REST scope is NOT covered by legacy keys or `api.runpod.ai`-only scopes — both silently 401 on `rest.runpod.io/v1`) |
| `IPLANE_PROVIDER`                  | Default provider for CLI commands and demo binaries when `--provider` is omitted. Falls back to `runpod` if unset (preserves Ch 6 behavior). Example: `IPLANE_PROVIDER=vast iplane deployment deploy llama --model ...` |
| `VAST_API_KEY`                     | Required when `IPLANE_PROVIDER=vast` (or `--provider vast`). Bearer-token auth on `console.vast.ai`. |
| `IPLANE_VAST_MIN_INET_DOWN_MBPS`   | Lower bound on a Vast host's advertised download bandwidth, pushed into the offer search. Default 1000. `0` disables the floor. |
| `IPLANE_VAST_MIN_RELIABILITY`      | Lower bound on Vast's `reliability2` host score, same search. Default 0.98. `0` disables the floor. Both exist because the cheapest offer is cheap for a reason: see the marketplace gotcha below. |
| `LAMBDA_API_KEY`                   | Required when `IPLANE_PROVIDER=lambdalabs` (or `--provider lambdalabs`). HTTP-Basic auth on `cloud.lambdalabs.com` (apiKey as username, empty password). |

Future provider API keys (not yet implemented): `EQUINIX_AUTH_TOKEN`, `EQUINIX_PROJECT_ID`. See `.env.local.example`.

The provider→API-key mapping lives in `internal/provisioners/apikey.go` (`ProviderAPIKeyEnv`, `EnsureProviderAPIKey`). Add new providers there; cmd/ and examples/common/ pick them up automatically.

## Stack dependencies

- `github.com/panyam/servicekit` — graceful shutdown + HTTP middleware (Tier-1, mature)
- `connectrpc.com/connect` — gRPC + Connect + HTTP/JSON on one handler
- OpenTelemetry Go SDK + OTLP/gRPC exporters
