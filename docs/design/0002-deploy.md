# 0002 — Deploy

## Why this doc exists

v0.1 Phase 2 adds the second iplane primitive that mutates the world outside the operator's laptop: a containerized engine running on a previously-provisioned instance, billed by uptime. Phase 2 builds the `iplane instance deploy` verb against the Provisioner contract from [0001-provisioner.md](0001-provisioner.md). If the deployment contract drifts mid-phase, every downstream piece (telemetry seeding, model store, the `iplane up` wrapper) pays for it. Locking the shape now — verb semantics, resource model, state-file schema, failure-mode contract, SSH key management — is what this doc does. No Go code lands in this PR; Phase 2 implements against the shape proposed here.

This doc takes 0001's decisions as given. Where Phase 2 reuses a Phase 1 pattern (idempotency on `(operator, id)`, state-file hygiene, list self-heal), the rationale is in 0001 and this doc just points there.

## Decisions

### Declarative semantics under imperative verb name

`iplane instance deploy <id> --image X --model Y` is **idempotent**: re-running with the same `(image, model)` for a Deployment whose container already matches is a no-op (zero SSH calls, zero `docker run`). If the container is missing, drifted, or crashed, the same command repairs to desired state.

Two questions answered together:

- *Should the verb be named `deploy` or `ensure`?* Phase 1's `create` is already this pattern (imperative-named, declarative-semantics, idempotent on `(operator, id)`) and the dissonance has not bitten operators in practice. Keeping `deploy` matches operator muscle memory (docker, kubectl, terraform all use action-shaped verbs that behave declaratively under the hood). The convention this doc locks in: **imperative verb names, declarative semantics**. Subsequent phases follow this — `iplane deployment reload`, `iplane fleet drain`, `iplane up` all behave the same way.

- *Should iplane deploy track its own state, or be a thin SSH+docker wrapper?* The honest case for "thin wrapper" is strong: the container on the instance IS the source of truth (`docker ps` is authoritative), and ansible/terraform can do SSH+docker too. We chose track-our-own-state for one reason: **multi-engine-per-instance**. v0.3's fleet phase and v0.2's "multiple pods sharing one cached model" scenario both want to address individual workloads on a shared instance. A Deployment record (parallel to Instance) gives operators a stable handle for that; a "the container IS the record" model can't address one of N engines without inventing a deployment id later.

The narrow-focus pushback (use ansible if you want orchestration) is still valid for everything Phase 2 *doesn't* do: rolling updates, canary, blue/green. iplane's deploy verb is "configure this instance to run this engine with this model." Bigger orchestration stays out of scope (Out of scope, below).

### Resource model: Instance vs Deployment split

**Two top-level resources, cross-referenced by `instance_id`:**

- **Instance** (Phase 1): a GPU machine. Created/destroyed deliberately. Costs money per hour. Long-lived across many workload changes. ID is operator-supplied, tenant-globally unique.
- **Deployment** (Phase 2): an engine container running on an Instance. Cheap to start/stop/swap. Multiple Deployments can target one Instance (v0.2+; v0.1 ships single-deployment-per-instance but the schema supports N). ID is operator-supplied, tenant-globally unique, **separate namespace from Instance ids**.

Different lifecycles, different cost models, different operator mental model. Conflating them under one `ensure` verb (deploy implicitly creates the instance if missing) is tempting but couples a cheap action to a costly one — chapter readers should see the cost gate at the instance level and cheap iteration at the deployment level.

The cross-reference goes one way: `Deployment.instance_id → Instance.id`. Listing "what is running on instance X" is a query, not a sub-record on the Instance. `list deployments where instance_id = X` instead of `instance.deployments[]`. Schema scales to multi-deployment without restructuring.

### Deployment type

```protobuf
message Deployment {
  string id = 1;                      // operator-supplied, tenant-globally unique
  string instance_id = 2;             // foreign key into Instance.id

  // Desired-state axis. Drift detection in v0.1 is exact-match on (image, model).
  string image = 3;                   // docker image ref (e.g., "vllm/vllm-openai:0.7.0")
  string model = 4;                   // model ref (e.g., "Qwen/Qwen2.5-7B-Instruct")

  // Pass-through configuration. Carried in the Deployment record for audit
  // (so operators can see what env this deploy ran with) but NOT used to
  // detect drift in v0.1. Changing engine_args between deploys is a silent
  // no-op on the next deploy unless image or model also changes. v0.2's
  // --engine-config <yaml> promotes this into desired state.
  repeated string engine_args = 5;    // ["--gpu-memory-utilization", "0.9", ...]
  map<string, string> env = 6;        // KEY=VAL pairs for the container env
  int32 engine_port = 7;              // port the engine listens on in the container

  DeploymentState state = 8;
  string failure_reason = 9;          // set when state == FAILED

  // LRO progress. Updated by the executor goroutine as the deploy proceeds.
  // Operators read these via `iplane instance deployment status / watch`.
  string current_phase = 10;          // free-form e.g. "ssh:connecting", "docker:pulling"
  string progress_message = 11;       // free-form e.g. "pulling layer 4/12"

  google.protobuf.Timestamp created_at = 12;     // when PENDING was written
  google.protobuf.Timestamp started_at = 13;     // when STARTING entered
  google.protobuf.Timestamp ready_at = 14;       // when RUNNING entered
  google.protobuf.Timestamp terminated_at = 15;  // when TERMINATED entered

  // Container handle on the remote box. Filled when state >= STARTING.
  // Used by status to query the live container via `docker inspect`.
  string container_id = 16;
}

enum DeploymentState {
  DEPLOYMENT_STATE_UNSPECIFIED = 0;
  DEPLOYMENT_STATE_PENDING = 1;      // record created, executor not started
  DEPLOYMENT_STATE_STARTING = 2;     // SSH + docker pull + docker run in progress
  DEPLOYMENT_STATE_CONFIGURING = 3;  // container started, engine loading model
  DEPLOYMENT_STATE_RUNNING = 4;      // engine healthy, serving inference
  DEPLOYMENT_STATE_DEGRADED = 5;     // container alive, engine unhealthy
  DEPLOYMENT_STATE_TERMINATING = 6;  // destroy in progress
  DEPLOYMENT_STATE_TERMINATED = 7;   // terminal success
  DEPLOYMENT_STATE_FAILED = 8;       // terminal failure (deploy or runtime)
}
```

Eight states fit Phase 2; v0.2's "multi-engine, M of N active" scenario will likely add a `LOADED` state ("container exists, model warmed in memory, not currently routed to") — deferred until v0.2 has a router that knows what "active" means.

### Lifecycle + LRO

Deploy is **asynchronous by default**. `iplane instance deploy ...` returns as soon as the Deployment record is written in PENDING; the executor goroutine (in-process Service mode) or background worker (remote mode) progresses the record through STARTING → CONFIGURING → RUNNING. Operators read progress via:

```
iplane deployment status <id>           # one-shot read
iplane deployment watch <id>            # stream state transitions until terminal
iplane deployment wait <id> [--until=RUNNING] [--timeout=5m]   # block; exit 0 on success
```

`--wait` is also a shortcut flag on `deploy` itself for the common synchronous case:

```
iplane instance deploy my-llama --on my-pod --image vllm:0.7 --model qwen3.5-8b --wait
```

Async-by-default sets up the LRO pattern v0.2's `deployment reload` / `fleet drain` / `instance migrate` need anyway. Sync-via-wait stays the chapter narrative's default ("type the command, see it finish").

### CLI surface

Two cobra groups. `iplane instance` (existing) keeps create/list/describe/destroy. `iplane deployment` (new) takes the deployment verbs:

```
iplane deployment deploy <id> --on <instance-id> --image <ref> --model <ref> [flags]
iplane deployment status <id>
iplane deployment watch <id>
iplane deployment wait <id> [--until=<state>] [--timeout=<duration>]
iplane deployment list [--on <instance-id>] [--state <state>]
iplane deployment describe <id>
iplane deployment destroy <id> [--dry-run]
```

`--on <instance-id>` is required on `deploy` (a deployment must target a known instance — see Failure-mode contract for what happens if the instance is gone).

Positional `<id>` on `deploy`, `status`, `watch`, `wait`, `describe`, `destroy` matches `instance` convention. `deploy` does NOT take a positional provider (the provider is inherited from the target instance, looked up via the cross-reference).

Alias: `iplane instance deploy <id> --on <instance-id> ...` is a re-export of `iplane deployment deploy ...` for muscle-memory continuity with Phase 1's `iplane instance create / destroy`. Same code path, two entry points.

### State-file schema

Two top-level tables, parallel structure. The state file (currently `instances` only) grows a `deployments` map:

```json
{
  "schema_version": "1",
  "backend": "local-file",
  "operator_id": "default",
  "instances": {
    "my-pod": { ...Instance record... }
  },
  "deployments": {
    "my-llama": {
      "id": "my-llama",
      "instance_id": "my-pod",
      "image": "vllm/vllm-openai:0.7.0",
      "model": "Qwen/Qwen2.5-7B-Instruct",
      "engine_args": ["--gpu-memory-utilization", "0.9"],
      "env": {"HF_TOKEN": "..."},
      "engine_port": 8000,
      "state": "DEPLOYMENT_STATE_RUNNING",
      "current_phase": "engine:serving",
      "container_id": "abcd1234...",
      "created_at": "...",
      "started_at": "...",
      "ready_at": "...",
      "engine_endpoint": "http://1.2.3.4:8000"
    }
  }
}
```

Cross-reference is by string match on `instance_id`. v1.0's remote backend keeps the same structure; only the storage changes. The `schema_version` bumps to `"2"` when this lands (readers pinned to `ch06-final` don't see deployments; readers at `ch07+` do).

The unchanged contracts from 0001: atomic write via temp + rename, flock-on-`.lock` for cross-process serialization (with the `*os.File`-retained-by-caller fix from PR 19), forward-compat for unknown fields.

### Drift detection

**Exact match on `(image, model)`, full stop, for v0.1.**

- Image differs → stop old container, pull new, run new.
- Model differs → stop old container, start with new model.
- Image and model both match → no-op (zero SSH calls beyond the initial `docker inspect` to check).
- Container is gone (instance restarted, manual `docker rm`) → re-run with desired (image, model).
- Container exists but `docker inspect` shows it exited → re-run.

Engine args, env vars, ports are **not** drift signals. Changing `--gpu-memory-utilization` between deploys does not cause a re-deploy. Reasoning: v0.1 ships pass-through engine configuration without a typed schema; we can't reliably compare "did the operator actually mean this change" vs "they forgot to repeat a flag." v0.2's `--engine-config <yaml>` introduces a typed config blob whose hash CAN be a drift signal.

The engine args are still recorded in the Deployment record (for audit — operator can see what flags this deploy was created with). They just don't trigger reconciliation. Documented gotcha for chapter readers: if you change a flag and want it to take effect, also bump the image tag (or destroy + recreate the deployment).

### Implementation: single concrete executor, no Deployer interface

`internal/deployments/sshdocker/` ships as one concrete package. No `Deployer` interface. Reasoning: every provider in v0.1/v0.2 returns an Instance with `ssh.host + ssh.port`; the executor's strategy ("SSH in, run docker, watch") doesn't vary by provider. There's nothing to abstract over yet.

When v0.3 adds providers whose deploy strategy differs (k8s-native pod runtimes? bare-metal where docker isn't available? cloud-native container services?), the interface lands then with N=2 concrete impls. Premature abstraction here would force every future executor through `Deployer.Deploy(ctx, spec)` even though "deploy" looks different on each — better to discover the right shape from real second/third implementations.

The executor's responsibility:

1. Open SSH connection to the target instance (using the operator's SSH key from oneauth — see below).
2. `docker inspect` the container by name (deployment id is the docker container name; `iplane-deployment-<id>`).
3. If container matches desired (image, model): update `current_phase: "engine:serving"`, return.
4. If container drifted or missing: `docker pull <image>`, `docker run -d --name iplane-deployment-<id> ...`, wait for engine `/health` to return 200.
5. Update Deployment record's state through STARTING → CONFIGURING → RUNNING. Each transition is a state-file patch (same Update-under-flock pattern as Phase 1).
6. On failure: patch to FAILED with `failure_reason` set. Mirrors Phase 1's Spawn failure path.

The executor runs **in the same process** as the Service (in-process mode) or as a goroutine in `iplane serve` (remote mode). v0.1 has no external queue.

### SSH key management

**Operator-invisible.** Chapter 6 narrative shows zero ssh-keygen, zero `~/.ssh/`, zero `--ssh-key` flag. The key dance happens internally on first `iplane instance create runpod ...`.

**Storage**: oneauth's `FSKeyStore` (`stores/fs/fskeystore.go`) at `~/.iplane/keys/signing_keys/<safeID>.json`. One file per key, 0600 perms (matches the runpodctl + ssh-keygen pattern). Storage path is configurable via `--keys-dir`; default sits under the existing iplane state directory.

**Encryption at rest**: deferred to filesystem perms for v0.1 (same trust model as `~/.ssh/id_rsa`). oneauth's `EncryptedKeyStorage` currently only wraps HMAC algorithms (`keys/encrypted.go:70`); asymmetric pass-through is a tracked oneauth gap (see Open questions below) that doesn't block Phase 2.

**ClientID convention**: `iplane:ssh:<operator>:<provider>`, e.g., `iplane:ssh:default:runpod`, `iplane:ssh:default:lambda` (v0.2). Per-operator-per-provider scoping means rotation/revocation is provider-scoped (rotate the RunPod key without touching the Lambda one) and a leaked key has narrower blast radius.

**Generation**: Ed25519 (smaller, faster, more modern than RSA). `crypto/ed25519.GenerateKey` + `golang.org/x/crypto/ssh.MarshalAuthorizedKey` for the public form + standard PEM for the private. Comment string on the public key: `iplane-<operator>-<provider>-<created-at-rfc3339>` so iplane can identify its own entries on the provider side.

**Per-provider upload**: each Provider adapter implements an optional `KeyRegistrar` interface:

```go
type KeyRegistrar interface {
    EnsurePublicKey(ctx context.Context, publicKey []byte, comment string) error
}
```

The Service calls this at instance-create time, before Spawn. If the provider doesn't satisfy the interface (e.g., local), it's a no-op. RunPod implements it via the GraphQL `updateUserSettings(input: {pubKey: "..."})` mutation (REST doesn't cover SSH keys — confirmed against `https://rest.runpod.io/openapi.json`). The mutation is read-modify-write of a newline-concatenated `authorized_keys` blob; iplane parses existing keys, skips upload if its key is already present, otherwise appends and writes the full blob back.

**Scoped-key constraint**: RunPod's scoped `rpa_` keys cannot mutate user settings (user-settings isn't a listed scope per the [scoped-keys blog](https://www.runpod.io/blog/scoped-api-keys-runpod)). Operators need a Full-access RUNPOD_API_KEY for the one-time SSH bootstrap. Documented in the README + surfaced as a clear error if a scoped key returns 403 on the upload.

**iplane package shape**: `internal/sshkeys/` (small, ~200 LOC): key-gen, marshal helpers, the oneauth wrapper. The RunPod GraphQL client lives in the runpod adapter (`internal/provisioners/runpod/keyregistrar.go`).

### Failure-mode contract

Mirrors Phase 1's three-step pattern:

1. **Critical section**: check local Deployment state for this id; if PENDING/STARTING/CONFIGURING/RUNNING/DEGRADED exists, return the existing record (idempotent). If TERMINATED/FAILED exists, treat as gone (id is reusable). Otherwise claim a PENDING record.
2. **Outside the critical section**: SSH + `docker inspect` to detect a container with the matching name on the remote box. If found AND matches desired state, adopt it (write CONFIGURING/RUNNING based on engine health, no `docker run`). Same self-heal as Phase 1's leaked-instance recovery.
3. **Execute**: `docker pull` + `docker run`, watch for engine `/health` ready, patch the Deployment record through state transitions.

**Instance-gone-during-deploy**: hard error. If `instance_id` resolves to no record, or the record is in TERMINATED state, deploy fails immediately with `instance "%s" does not exist or is terminated`. v0.1 does not auto-reprovision — the cost gate must stay visible to operators. v1.0's reconciler can opt into auto-reprovisioning later.

**SSH unreachable**: deploy enters FAILED with `failure_reason: "ssh: <wrapped error>"`. Retry on the operator's next `deploy` invocation (idempotent — re-checks current state, attempts again). No automatic retry loop in v0.1; that's v0.2's reconciler territory.

**Container crashes mid-deploy**: deploy enters DEGRADED if the container exits during CONFIGURING. Operator sees the crash via `deployment describe` (which surfaces the container's exit code + last logs lines). Re-deploy attempts to repair.

**Container crashes post-RUNNING**: not detected actively in v0.1 (no background reconciler). Next `deployment status` query catches it and reports DEGRADED. v0.2's reconciler adds active health checks.

### Dry-run

`iplane deployment deploy --dry-run` follows the [docs/cli-dry-run.md](../cli-dry-run.md) pattern. Reads instance + existing deployment record + SSH + `docker inspect`, prints:

```
[dry-run] would deploy "my-llama" on instance "my-pod"
[dry-run]   image:      vllm/vllm-openai:0.7.0
[dry-run]   model:      Qwen/Qwen2.5-7B-Instruct
[dry-run]   plan:       container missing, will docker pull + docker run
[dry-run]   target ssh: root@1.2.3.4:22
[dry-run] no provider calls made, no state file changes.
```

If the container already matches:

```
[dry-run] would no-op: deployment "my-llama" matches desired state (image + model)
```

`iplane deployment destroy --dry-run` reads the record + `docker inspect`, prints what would be stopped. Same pattern as `iplane instance destroy --dry-run` from Phase 1.5.

### What is NOT in the Service interface

- **No `ReloadModel` method.** v0.1 swaps the model via `deploy <id> --model <new>` (drift detection picks it up). v0.2 may add a faster in-container model reload that doesn't require `docker run`.
- **No `Drain` method.** v0.3 fleet phase.
- **No `Migrate` method.** v0.3 + v1.0.
- **No `Restart` method.** `deploy <id>` re-runs the executor if the container is gone; explicit restart is a separate concern that v0.2 may surface.

### Service interface

```go
type DeploymentService interface {
    CreateDeployment(ctx, *CreateDeploymentRequest) (*CreateDeploymentResponse, error)
    DescribeDeployment(ctx, *DescribeDeploymentRequest) (*DescribeDeploymentResponse, error)
    ListDeployments(ctx, *ListDeploymentsRequest) (*ListDeploymentsResponse, error)
    DestroyDeployment(ctx, *DestroyDeploymentRequest) (*DestroyDeploymentResponse, error)
    WatchDeployment(ctx, *WatchDeploymentRequest, stream) error  // server-streaming
}
```

Naming follows `CreateInstance` / `DestroyInstance` from 0001 — `CreateDeployment` not `Deploy` so the proto namespace stays consistent. The CLI verb is `deploy`; the proto verb is `Create`. WatchDeployment is server-streaming (state transitions + progress messages until terminal).

## Open questions resolved

Listed for the record, with the call that landed:

- **Q1 — Deployment ID auto-derived or operator-supplied?** Operator-supplied, tenant-globally unique. Same reasoning as Instance id (the "collapse-redundant-identifiers" lesson from 0001): one canonical ID the operator types, idempotency on `(operator, id)`. Auto-derived from `(instance_id, image, model)` hash means "redeploy with new model" creates a *different* deployment, which is confusing.
- **Q2 — Engine config in desired state?** Out for v0.1. Pass-through at deploy time, recorded in the Deployment record for audit, NOT used for drift detection. v0.2's `--engine-config <yaml>` promotes it.
- **Q3 — Wait-for-ready: async or sync?** Async-by-default; sync via `--wait` flag on `deploy` or via `iplane deployment wait <id>`. LRO pattern (state transitions + progress) sets up v0.2's reload / drain / migrate.
- **Q4 — SSH credentials: ~/.ssh fallback or managed?** Managed, via oneauth's `FSKeyStore`. Per-operator-per-provider ClientID. Encryption-at-rest deferred to filesystem perms (0600). Zero operator-facing flags in v0.1; chapter narrative stays "iplane just works."

## What this enables

- Phase 3 (Telemetry seeding): pass `OTEL_EXPORTER_OTLP_ENDPOINT` env to the deployed container.
- Phase 4 (ModelStore): substitute the model ref handling — pull from HF on first start.
- Phase 5 (`iplane up`): chain `instance create` → `deployment deploy` → `wait` in one command.
- v0.2 (`RunPodVolumeStore`, multi-engine): the Deployment-per-instance schema absorbs N deployments per instance without restructuring.
- v0.3 (fleet, multi-backend routing): Deployments are the unit a router routes to.

## What is deferred

Captured here so the architecture's intentional gaps are obvious:

- **Container reconciliation loop**: no background goroutine watching deployed containers in v0.1. Status is read on demand (when operator queries). v0.2 adds active health checks.
- **Auto-reprovision when instance is gone**: hard-error in v0.1 (cost gate visible). v1.0 reconciler opts into auto.
- **Rolling updates / canary / blue-green**: v0.2+ at earliest. Use ansible/argocd if you need this in v0.1.
- **`LOADED` deployment state** (engine warmed but not actively routed to): v0.2 when routing layer lands.
- **Engine config in drift detection**: v0.2 with typed config.
- **Active health check + restart policy**: v0.2 reconciler.
- **Multi-container deployments per record** (sidecar logging, etc.): v0.3.
- **Per-deployment secrets**: v0.3 secret store. v0.1 passes secrets via `--env KEY=VAL` (operator's responsibility).
- **Encryption-at-rest for SSH private keys beyond filesystem perms**: tracked oneauth gap (asymmetric in `EncryptedKeyStorage` currently HMAC-only). Not v0.1-blocking.
- **Rotation of operator SSH keys**: machinery is there (`FSKidStore` persists kid→key with grace-period expiry), but iplane v0.1 doesn't wire it. v0.2+ adds the rotation policy + CLI verbs.

## Followup oneauth issues (optional, none v0.1-blocking)

1. **Asymmetric encryption support in `EncryptedKeyStorage`** — `keys/encrypted.go:70` only encrypts when `isHMACAlgorithm(rec.Algorithm)`. Asymmetric private keys (Ed25519, RSA PEMs) pass through plaintext. v0.1 iplane doesn't block on this (filesystem perms suffice), but production SaaS consumers will want it.
2. **Re-export `NewFSKeyStore` + `NewFSKidStore` from `keys/`** — minor convenience so consumers don't need to import `stores/fs` for the common single-binary case. One-line PR.

(An earlier draft of this doc listed "persisted KidStore rotation" as a gap — that was based on a stale agent survey. `FSKidStore` already lives at `stores/fs/fskidstore.go` and satisfies `keys.KidStorage` with file-per-kid + expiry.)

## Notes for the chapter draft

- The chapter teaches "you provision, you deploy, you get an OpenAI endpoint" — two verbs, one engine, no orchestration sprawl. The deploy verb's declarative-under-imperative semantics are *not* explained in the chapter narrative; the operator just sees that re-running `deploy` is safe.
- The SSH key dance is invisible. Chapter mentions "iplane handles SSH access transparently" once; the curious reader can find the details in this doc + `internal/sshkeys/`.
- The cost gate (instance create costs money; deploy doesn't) is a chapter teaching beat. Phase 1 already establishes this; Phase 2 reinforces it by NOT auto-reprovisioning.
- The async-with-LRO pattern shows up in the chapter as `--wait` (synchronous-looking). The async machinery underneath is mentioned in passing; readers who care can read this doc.
- `iplane instance deploy <id> --on <instance-id> --image X --model Y --wait` is the single command the chapter's act-3 demo runs. Output should fit on one terminal screen.
