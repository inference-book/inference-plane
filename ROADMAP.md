# iplane Roadmap

This document tracks the feature scope of `inference_plane` against the chapters in `../book/` that consume it. It pairs with [ARCHITECTURE.md](ARCHITECTURE.md) (the *current* design) and the book's [master_outline.md](../book/master_outline.md) (the chapter sequence that drives feature requirements).

**Detail levels:**

- **High** — the active version. Phases are scoped to the implementation, with file-level extension points and acceptance criteria.
- **Medium** — versions one or two ahead. Features are named and mapped to chapters; design decisions are listed but not resolved.
- **Low** — versions further out. Capability promises only. Filled in when they become active.

**Cadence:** iplane leads chapter writing by one or two phases. End each phase with a four-act sketch (see `../book/REVIEWS.md` § *Chapter Structure*) for the chapter that consumes it. If we cannot sketch the manual path or the iplane move in two paragraphs, the primitive needs another pass before we move on. Manual paths are often *discovered* during phase work — capture them in the chapter draft as they surface, not at write-up time.

**Tag convention:** `release/vX.Y` branches as today (per `RELEASE.md`), plus per-chapter immutable tags `chNN-final` cut from the branch when each chapter's content lands. The book's `\cptag` macro references the chapter tag; `\cpbranch` references the maintained branch. For v0.1 (one-chapter version), `ch06-final` ≡ `v0.1.0`.

**Frozen capability snapshots:** What ships at each `chNN-final` tag is recorded in [capabilities.yaml](capabilities.yaml) and rendered as a per-chapter capability table in the book. This roadmap is forward-looking; `capabilities.yaml` is what actually shipped.

---

## v0.1 — Single-Instance Foundation (active, high detail)

**Chapters:** Ch 6 (Building the Control Plane v0.1)
**Branch:** `release/v0.1` · **Tags:** `ch06-feat-{provisioner,deploy,telemetry,modelstore,up}`, final `ch06-final` ≡ `v0.1.0`

### Capabilities at `ch06-final`

- Acquire and release a GPU instance from the operator's laptop with one provider API key (RunPod)
- Run the same Provisioner interface against the operator's local hardware (`--provider local`) as a zero-cost on-ramp
- Deploy the engine container to that instance from the laptop — no SSH or git-clone in the chapter narrative
- Stream OTel traces, metrics, and logs from the data plane back to the laptop's local Grafana stack via a tunnel
- Pull weights from Hugging Face on first start (default `ModelStore` impl)
- Serve the OpenAI-compat API from `iplane serve`, proxying inference to the remote engine
- One-shot UX: `iplane up --provider runpod --model qwen3.5-8b` chains all of the above
- `--dry-run` on every state-changing command prints what would happen and exits

### Phases

| # | Feature                                  | Status   | Scope                                                                                              |
|---|------------------------------------------|----------|----------------------------------------------------------------------------------------------------|
| 1 | Provisioner + RunPod + Local adapters + `iplane instance` CLI + `--dry-run` | done | `internal/provisioners/` (Service + state.Store + local + runpod adapters), `cmd/iplane/cmd/instance{,_create,_list,_describe,_destroy,_test}.go`, `--service-url` remote transport + in-process default, `--output table\|json`, `--dry-run` with cost preview. State at `~/.iplane/state.json`. `--dry-run` pattern documented in `docs/cli-dry-run.md` for phases 2–5 to follow. Demokit walkthroughs at `examples/01-end-to-end/` (gRPC client) and `examples/02-cli-end-to-end/` (CLI binary). |
| 2 | Deploy primitive                         | done | `iplane deployment deploy` — image-as-pod via the `provisioners.Deployer` capability (PRs 44, 45). RunPod's Deployer spawns a pod whose container IS the engine image; engine `/health` is HTTP-polled from the operator side via the provider's HTTPS proxy URL. Cost-aware default: no `publicIp` allocated, `debug_shell: true` opt-in flips `supportPublicIp` + declares `22/tcp` for `iplane instance ssh`. Auto-provision (no `--instance`) collapses instance + deployment 1:1 with a scheduler seam (`placeDeployment`) for v1.0 bin-packing. State-file hygiene: `provider_id` stamped onto instance at emit-time so destroy reaches the provider even after a failed deploy. `WaitForInstanceReady` fails fast when SSH was never requested. Deploy progress streamed to stdout via `WatchDeployment` (elapsed-time + HTTP status). `iplane deployment query <id> "<prompt>"` closes the loop: the operator sees prompt-in / tokens-out. VM-style providers (Lambda Labs in v0.2, raw AWS) fall back to the `sshdocker.Executor` via the Service's capability dispatch — both paths satisfy the same `Deployer` interface. |
| 3 | Telemetry seeding                        | active | Two operator-pickable sinks: (a) **hosted OTLP** (Grafana Cloud Free, Honeycomb, etc.) — zero local infra; (b) **local docker-compose stack** (otel-collector + tempo + loki + mimir + grafana) exposed to remote pods via a cloudflared quick tunnel (`COMPOSE_PROFILES=tunnel`). Both reduce to "set `OTEL_EXPORTER_OTLP_ENDPOINT` on the pod"; iplane just propagates. New `iplane deployment deploy --otel-endpoint --otel-headers` flags (with `IPLANE_OTEL_*` env fallbacks). New `iplane telemetry url` discovers the cloudflared tunnel URL. Demo 03 has a "wire-telemetry" step that hard-fails on a missing endpoint. See [docs/telemetry.md](docs/telemetry.md). |
| 4 | `ModelStore` interface + HF impl         | done | `internal/modelstores/` (interface + Passthrough fallback) + `internal/modelstores/huggingface/` (HF API client). v0.1 use is **pre-flight validation**: `Service.CreateDeployment` calls `ModelStore.Resolve(spec)` before any provider touch; HF impl GETs `huggingface.co/api/models/<id>` with a 5s timeout. Catches typos (404), gated-access (401/403 with HF_TOKEN hints), disabled models, network errors (with bypass flag hint). Propagates `$HF_TOKEN` env onto the pod for gated-model auth. New persistent `--skip-model-validation` root flag (and `IPLANE_SKIP_MODEL_VALIDATION=1` env fallback) for offline / firewalled / non-HF model setups; default is on. No proto changes — the layer is Service-internal. Interface seeded so v0.2's `CachedStore` + `RunPodVolumeStore` wrap `huggingface.Store` without touching Service code. See [docs/modelstore.md](docs/modelstore.md). |
| 5 | `iplane up` UX wrapper                   | done | The chapter's flagship one-liner. Single command provisions an instance, runs the engine image-as-pod, waits for `/health`, drops the operator into a readline-backed chat REPL, and tears everything down on exit (empty prompt OR Ctrl-C OR provision failure -- defer'd `DestroyDeployment` is the leak-protection invariant). Pure orchestration over existing primitives: `CreateDeployment{auto-provision, wait=true}` → `WatchDeployment` stream for cold-start progress → direct `POST /v1/chat/completions` to the engine proxy URL → `DestroyDeployment`. OTel env propagation matches `deployment deploy`; warns (doesn't fail) on missing endpoint, `--no-telemetry` to skip. `--no-chat` mode for "give me the endpoint and block on Ctrl-C." Single-instance / single-model in v0.1; multi-replica / multi-model variants are v0.2 (chapter 7's queue + scheduler) and v1.0 (lab mode) respectively. See [docs/iplane-up.md](docs/iplane-up.md). |

### Open design questions for v0.1

- **Image-as-pod vs SSH+docker (resolved by PR 44).** Original design (0002-deploy.md) used "phase 1 provisions docker-capable base; phase 2 SSH + `docker run`." Failed in practice: RunPod's container runtime doesn't support docker-in-docker on the default `runpod/pytorch` base, every base image is a new OS-compat surface (apt vs apk, systemd vs sysvinit, privileged vs not). The pivot makes image-as-pod the v0.1 default via the `Deployer` capability interface; SSH+docker survives as the fallback for VM-style providers (Lambda Labs in v0.2, raw AWS / GCP). Single interface, dispatched at runtime by provider capability.
- **State-file hygiene.** Write state-pending before the API call, patch to active on success. `iplane instance reconcile` (post-v0.1) diffs provider-side against local state.
- **Cost guardrail.** `iplane instance create` refuses anything above $1/hr unless `--yes-i-know` is passed. Threshold reviewable.
- **GPU spec language.** Primary: `--gpu-class small|medium|large|xlarge`. Escape hatch: `--gpu-sku <provider-sku>`.
- **Region selection.** Required (operator picks). Auto-region waits until ModelStore caching forces region pinning anyway.
- **State-file schema.** Even though v0.1 is single-laptop, design the schema to be amenable to a remote backend later (multi-operator state sync is a v1.0 capability). Avoid baking "local-only" assumptions into the format.

---

## v0.2 — Serving Real Workloads (medium detail)

**Chapters:** Ch 7 (Routing, Queueing, Replicas), Ch 8 (KV / Prefix Caching), Ch 9 (Scaling to 30B)
**Branch:** `release/v0.2` · **Tags:** `ch07-final`, `ch08-final`, `ch09-final`

### Features by chapter

| Chapter | Feature                                  | Notes                                                                                              |
|---------|------------------------------------------|----------------------------------------------------------------------------------------------------|
| Ch 7    | Data plane: router → queue → scheduler   | Three beats forming the chapter's act sequence. **Beat 1 — Router (done, PRs 109–127, demo at `examples/04-router-in-path/`):** iplane sits in front of the engine. Both URL shapes ship: `/v1/<deploy-id>/v1/...` for unambiguous routing, flat `/v1/chat/completions` (router body-peeks `model`) for OpenAI-SDK compat. Request-level metrics (`iplane_router_requests_total`, `iplane_router_request_latency_seconds`, `iplane_router_completion_tokens_total`) tick at the router, populating the new v0.2 Grafana dashboard. W3C TraceContext + Baggage propagate router→engine. Idle-TTL reaper + `TouchDeployment` RPC + `--no-idle-destroy` pin trio protects against leaked deployments. Storage tier landed as `internal/provisioners/stores/{file}/` with a `Store` interface sized for the GORM/GAE siblings to follow. CP/DP-1 constraint enforces the gRPC-only boundary mechanically. **Beat 2 — Queue + scheduler (done, PRs 128–136, demo at `examples/05-fair-queueing/`):** router gains a bounded waiting room in front; `internal/scheduler/` is the new package hosting the dequeue-and-dispatch primitive. Tenant + priority resolution from headers (`X-IPlane-Tenant`, `X-IPlane-Priority`) lanes traffic into per-tenant sub-queues; strict-priority across lanes, weighted-lottery fair-share within a lane (per `router.queue.tenant_weights`). Per-deployment in-flight cap mirrors the engine's `max-num-seqs`. Architectural call: **priority is request-level only**, not a deployment property — engines stay priority-blind. New observability: `iplane.queue.depth` gauge + `iplane.queue.wait.seconds` histogram + `iplane.queue.wait_ms` span attribute; Grafana panels added. `iplane load` gains `--target` / `--priority` / `--tenant` / `--stream` / `--output json` for demo orchestration. **Beat 3 — Multi-replica fan-out (done, PRs 137–157, demo at `examples/06-multi-replica/`):** one stable deployment ID with N backing instances via parallel `instance_ids` / `engine_endpoints` / `unhealthy_instance_ids` arrays; `iplane deployment scale <id> N` as the operator's primary capacity verb; `DEGRADED` aggregate state as the first-class outcome for partial provisioning failure. Router fans out round-robin across the healthy replicas (PR 140), composing with the unhealthy-set so the data path never has to be told which replicas to skip. Per-replica health checker auto-quarantines stuck replicas (PR 141); per-replica `iplane.router.replica.in_flight` gauge + `iplane.router.routing_decisions` counter restore decision visibility (PR 142). Heterogeneous fleets via `ReplicaSpec.replicas` (PRs 148, 149) — one deployment can pull replicas from RunPod, Vast, and Lambda Labs together, with the homogeneous case as the degenerate one-spec form. Routing-policy seam (PR 148) is the interface-only hook Ch 8's prefix-cache affinity composes through. Multi-cloud provider catalog lands as new packages without interface changes: Vast.ai marketplace adapter (PR 152), Lambda Labs fixed-catalog adapter (PR 154, HTTP-Basic instead of Bearer), `IPLANE_PROVIDER` env-driven default + `ProviderAPIKeyEnv` mapping (PRs 153, 155). Hardware spec + metadata map on `Instance` (PR 156) is an internal refactor that lets the provisioner expose GPU SKU / VRAM / region without proto churn. |
| Ch 8    | Prefix-cache observability               | Cache hit-rate metric family. Surface engine-side prefix caching as a configurable.                 |
| Ch 9    | Quantization-aware deploy                | Image catalog gains AWQ/GPTQ variants. `iplane deploy --quantization awq`. Class shifts to "medium". |
| All     | `RunPodVolumeStore` + `CachedStore`      | Network-volume cache wrapper. First multi-instance scenario (N pods sharing one cached model).      |
| All     | Lambda Labs adapter                      | Proves the Provisioner interface beyond N=1 (provider-side; `local` already proves N=2).            |

### Cross-cutting features for v0.2 (named, low detail)

- **Model catalog.** `iplane model list`, `iplane model describe`. Models become first-class objects with metadata (engine compatibility, quant variants, GPU-class fit, license, HF revision). Affects the `ModelRef` shape we lock in v0.1.
- **Engine config per deployment.** `--engine-config <yaml>` or referenced profile. Ch 9 makes `gpu_memory_utilization`, `max-model-len`, etc. load-bearing.
- **Lifecycle commands beyond create/destroy.** `iplane instance drain`, `iplane deploy reload-model`, `iplane deploy restart`. Affects the state machine on `Instance` and `Deployment`.
- **Load generation as iplane primitive.** `iplane load --target <id> --rate 10rps`. Promoted from today's `make load` so chapters benchmark across deploys uniformly.
- **Logs and exec.** `iplane logs <instance>`, `iplane exec <instance> <cmd>`. Convenience for the chapter narrative when something goes wrong.
- **Configuration profiles.** `iplane profile use <name>` bundles (provider, gpu-class, engine, model, store, engine-config) into named recipes (`dev`, `cheap`, `prod`, etc.).
- **Self-observability.** iplane's own logs/metrics: provisioner latencies, deploy success rate, state-file write conflicts.

### Open design questions for v0.2

- Queue persistence: in-memory only, or backed by a store? Affects restart semantics.
- Cache hit attribution: per-prompt-prefix only, or per-tenant-once-auth-lands?
- Image catalog source of truth: in-repo YAML vs. registry-scanning at startup.
- Profile composition rules: full override, layered merge, or named inheritance?

---

## v0.3 — Distributed Inference (medium detail)

**Chapters:** Ch 10 (Multi-GPU), Ch 11 (Multi-Backend Routing), Ch 12 (70B Deployment)
**Branch:** `release/v0.3` · **Tags:** `ch10-final`, `ch11-final`, `ch12-final`

### Features by chapter

| Chapter | Feature                          | Notes                                                                                              |
|---------|----------------------------------|----------------------------------------------------------------------------------------------------|
| Ch 10   | Multi-instance fleet             | Cp tracks N data planes via control-channel registration. `iplane fleet status`, `iplane fleet drain`. |
| Ch 11   | Backend router                   | Workload-aware (small/large), cost-aware (spot/on-demand), health-aware.                            |
| Ch 12   | TP/PP-aware deploy               | `iplane deploy --tp 4 --pp 2`. Image catalog gains multi-GPU variants.                              |
| All     | Vast.ai + AWS adapters           | Provider mix becomes real; cost-aware routing has providers to choose between.                      |
| All     | `S3Store` / `GCSStore`           | Object-storage backends for fleet provisioning at scale.                                            |

### Cross-cutting features for v0.3 (named, low detail)

- **Secrets store.** Provider API keys, HF tokens, OTLP auth headers. Today env vars; eventually a real store with per-deployment scoping.
- **Networking-aware provisioning.** Capability flags on instance requests: `--needs-nvlink`, `--needs-rdma`. Provisioner selects only matching SKUs.
- **Migration primitives.** Move a deployment between providers, upgrade engine version live, drain-and-re-provision. `iplane deploy migrate --to <instance>`.
- **Spot / preemption handling.** Cost-aware deployment on spot instances with graceful preemption (re-provision on a new instance, drain in-flight, cut over).
- **Cost attribution beyond providers.** Per-model, per-route. Per-tenant slices appear once Part V auth lands.

### Open design questions for v0.3

- Control-channel protocol shape: long-lived gRPC stream vs. periodic registration. Decide before Ch 10.
- Routing policy expression: declarative YAML, embedded DSL, or Go plug-in. Affects how Ch 11 teaches it.
- Reconcile semantics across providers when a provider API is temporarily unreachable.
- Secret store backend: file-based (`~/.iplane/secrets.json`), OS keychain, HashiCorp Vault adapter, or all three.

---

## v1.0 — Production Ceiling (low detail)

**Chapters:** Ch 13 (400B + H100), Ch 14 (Production Operations)
**Branch:** `release/v1.0` · **Tags:** `ch13-final`, `ch14-final`

Capabilities promised:

- Deploy 400B-class models on multi-host H100 clusters
- SLO/SLI definitions as iplane primitives, not just Grafana panels
- Auto-scaling on traffic, cost-aware spot fallback
- Runbook tooling: `iplane drain`, `iplane snapshot`, `iplane rollback`
- **Multi-operator state sync.** Shared fleet state across a team. Backend-pluggable (S3, Postgres, etcd). The v0.1 state-file schema should be designed with this in mind.
- **Plugin / extension surface.** Stable interfaces for third-party Provisioners, ModelStores, image catalogs, routing policies. Affects which interfaces we promise to keep stable from v0.3 onward.
- **Alerting / SLO breach detection.** Beyond dashboards: alert rules, paging integrations, breach detection.
- **Backup / disaster recovery.** State snapshots, restore from corrupted state.

Design questions deferred to when this version becomes active.

---

## Part V — Productizing (separate track)

**Chapters:** Ch 15–19. Versioning is orthogonal to v0.1–v1.0; lives behind feature flags on `release/v1.0` (or a sibling branch if it grows large enough). None of this is inference-specific; the chapters call this out and the iplane code keeps the SaaS surface optional.

Capabilities promised:

- Auth, API keys, user management
- Rate limiting and quotas
- Multi-tenancy and tenant isolation
- Per-tenant cost attribution and billing
- Audit log of who provisioned/deployed what, when, where (compliance-relevant)
- CodeLab capstone (commercial AI coding assistant)

---

## Transverse capabilities (apply across all versions)

These are not features of any single version but design properties that should be present from v0.1 onward and preserved as new versions land:

- **Local provider.** Same Provisioner interface, satisfied by laptop hardware. Zero-cost on-ramp for readers; pressure-tests the interface (N=2 impls from day one).
- **Dry-run mode.** Every state-changing command accepts `--dry-run`. Cheap to add in v0.1; expensive to retrofit later.
- **Self-observability.** iplane's own operations (provisioner latencies, deploy success rate, state-file write conflicts) get the same OTel treatment as the data plane. Surfaces in v0.2 once there is enough surface area to be worth instrumenting.
- **State-schema forward-compatibility.** Local state files designed so a remote backend (v1.0 multi-operator sync) can replace them without a migration.
- **Honest error reporting.** Provider errors surface up with the original message preserved; no swallowing or rewriting that breaks debugging.

---

## Explicitly out of scope

Recorded so we (and readers) know we considered them and chose not to build them:

- **Hosted iplane control plane (iplane-as-a-service).** iplane is operator-run infrastructure, not a hosted product.
- **Custom kernel / engine modifications.** We use upstream engines as-is. Ch 5 acknowledges this.
- **Training, fine-tuning, RLHF.** This is an inference book.
- **Browser AI / WebGPU as an iplane target.** Currently Ch 6.5 is low-priority / deferred. Mentioned here so it is not silently forgotten.
- **TPU and non-GPU hardware.** Appendix G covers it conceptually. The Provisioner interface is general enough to absorb a TPU adapter later. No current implementation; named so the interface stays honest.

---

## Updating this document

When a phase moves `designed → active → done`, update the row and adjust the capability bullets at the top of the version section. When a `chNN-final` tag is cut, snapshot `capabilities.yaml` accordingly.

For the four-act narrative pattern each chapter follows, see `../book/REVIEWS.md` § *Chapter Structure*.
