# 0001 — Provisioner

**Status:** Proposed
**Phase:** v0.1, Phase 1.1
**Depends on:** ROADMAP.md (v0.1), capabilities.yaml (`provisioner_runpod`, `provisioner_local`, `dry_run_mode`)
**Blocks:** v0.1 phases 1.2 – 1.5

## Why this doc exists

v0.1 introduces the first iplane primitive that mutates the world outside the operator's laptop: a GPU instance, billed by the hour. Phases 1.2 – 1.5 build the RunPod adapter, the local adapter, the CLI surface, and the dry-run flag against the same Provisioner contract. If that contract drifts mid-phase, every downstream piece pays for it. Locking the shape now — interface, types, state-file schema, failure-mode contract, cost guardrail — is what this doc does. No Go code lands in this PR; phase 1.2 implements against the shape proposed here.

The Provisioner is also the interface that has to age the best in the whole v0.1 surface. Lambda Labs lands in v0.2, Vast.ai and AWS in v0.3, multi-operator state sync in v1.0. The shape we pick has to absorb those without a re-cut, and the state file has to be readable by a future remote backend without a migration.

## Decisions

### Provisioner interface

```go
type Provisioner interface {
    Spawn(ctx context.Context, spec Spec) (*Instance, error)
    Terminate(ctx context.Context, id ProviderInstanceID) error
    Describe(ctx context.Context, id ProviderInstanceID) (*Instance, error)
    List(ctx context.Context, filter ListFilter) ([]InstanceRef, error)
}
```

Four methods. Context is on every call so cancellation, deadlines, and trace propagation reach the provider SDK. A control-C during a RunPod spawn that hangs at the API layer needs to abort the HTTP round-trip, not orphan a goroutine.

`Spawn` returns the fully-resolved `Instance` (provider ID, region as actually scheduled, hourly rate as actually billed, SSH coordinates) rather than a handle the caller has to re-query. The provider already paid the network round-trip; making the caller call `Describe` immediately afterward is wasted work and a window where state is partially written.

`Terminate` is idempotent by contract. Calling it twice on the same ID returns nil the second time. Without that property, the state-file recovery path (terminate a `terminating` record on next boot) has to special-case "already gone," which is exactly the kind of cleanup logic the abstraction is supposed to hide.

`Describe` is the *truth* call for a known provider ID. What does the provider think this instance is, right now. It must not consult local state; if the local state-file is wrong, `Describe` is how we find out. The qualifier matters: `Describe` only helps when we already have a `ProviderID`, which is precisely the field we are missing after a crash between Spawn's request and Spawn's response.

`List` covers the case `Describe` cannot. Two failure modes force it into v0.1:

1. **Ghost-record recovery.** Write `pending` locally, call `Spawn`, crash (or HTTP timeout) before the provider ID round-trips back. The local record has no `ProviderID`, so `Describe` is useless. `List` with the iplane-id tag filter is how the next CLI invocation finds the orphan and completes the record.
2. **Leaked-instance detection.** A pod exists under the operator's account that we have no record of. Either we wrote a `pending`, succeeded at the provider, and then lost the state-file write; or another tool created it; or a v0.2-or-later iplane wrote it under a different schema. `List` with no iplane-id filter is the only way to see it.

The state-file-as-intent-log contract leans on both. Without `List`, the abstraction can deliver "we will write before we call" but cannot deliver "we will heal." For the same reason multi-state lifecycle (`pending`, `terminating`) was worth two extra states, bidirectional reconcile is worth one extra method.

```go
type ListFilter struct {
    // Match-all on tags. Empty filter returns every instance under the
    // provider account that the iplane-operator tag matches; see the tag
    // convention below.
    Tags map[string]string
}

type InstanceRef struct {
    ProviderID    ProviderInstanceID
    ProviderState string             // provider's literal status string, not iplane's
    Tags          map[string]string  // includes iplane-id / iplane-operator if iplane created it
    HourlyRateUSD float64            // what the provider currently bills
    CreatedAt     time.Time          // provider-reported creation time
}
```

`InstanceRef`, not `*Instance`. Some providers return less information from `list` than from `describe` (RunPod is one), and returning a partial `Instance` with zero-valued SSH or region fields lies about what `List` can promise. `InstanceRef` carries the minimum needed to reconcile against local records; callers who need more call `Describe(ref.ProviderID)`.

`ProviderState` is the provider's literal status string. The provider package documents the mapping from that string to iplane's state machine (RunPod's `EXITED` → iplane's `terminated`, `RUNNING` → `active`, etc.). Pushing the mapping into the provider package keeps the core agnostic and keeps the provider's vocabulary visible for debugging.

Pagination: not in v0.1. Single-page list is fine while the operator has at most a few dozen instances; the v0.3 fleet phase is when this stops being true, and by then we have callers driving the shape of a cursor or limit-offset choice.

No `Update`. Instances are immutable from iplane's perspective. Resize, change image, change region — all destroy-and-recreate. v0.3's drain/migrate primitives will compose Terminate + Spawn rather than mutating in place.

Per-instance, not per-cluster. `Spawn` returns one `*Instance`, not `[]*Instance` and not a fleet handle. v0.2 has the first real N>1 use case (multiple pods sharing a `RunPodVolumeStore` cache) and v0.3 has the proper fleet primitives (`iplane fleet status`, `iplane fleet drain`), and the question whether `Spawn` should take a `Count` and return `[]*Instance` is reasonable to ask now. The answer is no, for the same reason `Spawn` does not call RunPod's container-at-creation API: AWS has ASG, GCP has MIG, and RunPod/Vast/Lambda/local have nothing comparable. A fleet-shaped Provisioner forces every non-ASG provider to loop internally and swallow partial-failure semantics the caller actually wants to see. Cluster lifecycle lands in a sibling abstraction: v0.2 groups instances via a `cluster=<id>` tag in the state file (no interface change), v0.3 introduces a `ClusterManager` that owns the forming/ready/draining state machine and calls `Provisioner.Spawn` N times. Providers with a native fleet primitive can advertise it later via an optional `SpawnGroup(ctx, spec, n)` capability that the ClusterManager type-asserts for; the core Provisioner shape does not move. Same layering as Kubernetes — kubelet manages pods, controllers manage Deployments.

### Tag convention

Every `Spawn` stamps the provider instance with two tags:

- `iplane-id`: the iplane-side identifier, supplied by the operator via `Spec.ID`. Human-readable (e.g., `my-pod`, `qwen-demo`). Tenant-globally unique across all providers. The bridge from provider-side enumeration back to a local record and the key the idempotency lookup matches on.
- `iplane-operator`: the operator ID (v0.1 always `"default"`). The bridge to v1.0's multi-operator state.

`List` filters on these. `iplane instance list` (default) calls `List(ctx, {iplane-operator: "default"})` and self-heals any local `pending` records by matching `iplane-id`. `iplane instance list --remote` calls the same and surfaces *every* instance the provider sees under the operator, flagging ones with no matching local record as candidates for reconcile or destroy.

Providers that do not natively support tag filtering (some APIs only support listing all and filtering client-side) implement the filter in the adapter. The interface contract is "filter applied, results returned"; how is the adapter's problem.

### Instance and Spec types

```go
// What the operator asks for. Provider-agnostic.
type Spec struct {
    ID        string             // required. Operator-supplied. Tenant-globally unique across all providers.
    Provider  string             // "runpod" | "local"
    Region    string             // required (provider-specific identifier)
    GPU       GPUSpec
    BaseImage string             // Docker-capable base image ref; phase 2 docker-runs on top
    Tags      map[string]string  // operator-supplied labels (cost attribution, owner, etc.)
}

type GPUSpec struct {
    Class string // "small" | "medium" | "large" | "xlarge"  -- primary surface
    SKU   string // provider-specific override; empty unless --gpu-sku used
    Count int    // default 1
}

// What came back. Provider-resolved.
type Instance struct {
    ID         string             // mirror of Spec.ID. Iplane-side canonical identifier.
    ProviderID ProviderInstanceID // provider's internal handle (RunPod pod id, AWS instance-id, etc.)
    Provider   string

    Spec Spec // snapshot of what was asked for

    Region        string  // where it actually landed
    GPU           GPUInfo // class + sku + count + vram_gb as scheduled
    HourlyRateUSD float64 // what the operator is actually being billed

    State        State      // see State below
    CreatedAt    time.Time
    ActivatedAt  *time.Time // nil until State transitions to active
    TerminatedAt *time.Time

    SSH SSHTarget // empty until State==active

    // Provider-specific extension data, opaque to iplane core.
    // Stored as raw JSON so a v0.1 reader can deserialize state written
    // by a future version without losing or rejecting unknown fields.
    ProviderData json.RawMessage
}
```

Two identity fields, not one, but only one is operator-facing. `ID` is what the operator types on the CLI; `ProviderID` is the provider's internal handle. Provider handles are not stable across providers (RunPod returns opaque strings, AWS returns `i-…`, local returns a PID), and the operator-facing identifier must survive provider swaps — the day a deployment moves from RunPod to AWS in v0.3, the `ID` does not change; only `ProviderID` does. Without the split, every CLI command would either expose provider-specific handles or invent a translation layer at the CLI boundary.

`ID` is operator-supplied and mandatory, not auto-generated. The earlier version of this design carried both an auto-generated UUID *and* a separate user-provided name, and they were collapsed for two reasons: (a) two uniqueness constraints means two places for inconsistency to creep in, and (b) nothing load-bearing actually depended on the UUID specifically — `ProviderID` is the stable handle for distinguishing physical instance lifetimes in logs/metrics across destroy-and-recreate, and `(ID, created_at)` composites cover the rest. The `iplane-` prefix on the ID is reserved (CLI rejects operator-supplied IDs starting with it) so a future relaxation to auto-generated IDs can use it without colliding with anything that exists.

The `Spec` snapshot inside `Instance` matters for two reasons. Reconcile needs to know what was *asked* for to detect drift against what the provider currently reports. And errata cherry-picks across release branches will inevitably change the class→SKU mapping; the snapshot lets us answer "what did class:large mean on the day this pod was created" without grepping the release tag in use at that moment.

GPU class is the primary surface; SKU is the escape hatch. Class is portable across providers; SKU is the override when class does not capture what the operator needs (NVLink topology, specific generation, VRAM at the boundary). Each provider ships a class→[SKU] table; phase 1.2 puts RunPod's table in `internal/provisioners/runpod/skus.go`. The chapter taxonomy (`small`=24 GB, `medium`=40–48 GB, `large`=80 GB, `xlarge`=96 GB+) is the same vocabulary across every provider, which is what lets the book teach one mental model and the operator pick the provider afterward.

`ProviderData` carries provider-specific fields that iplane core does not need to understand: RunPod's networkVolumeId, AWS's launch-template ID, Vast's bid history. Keeping it as `json.RawMessage` means a v0.1 binary reading a state file written by a v0.2 binary parses cleanly; the unknown fields round-trip untouched. Without this, every new provider feature would force a state-file migration.

### Identity and idempotency

`Spec.ID` is mandatory. Every Spawn requires the operator to supply an ID, and Spawn is idempotent on `(operator, id)`: two invocations with the same pair return the same instance, not two. Uniqueness is tenant-global across all providers — an operator cannot have `my-pod` on RunPod *and* `my-pod` on Lambda Labs simultaneously, because the ID identifies one logical instance regardless of where it currently runs.

The motivation is not aesthetic. Without idempotency, every caller that wants retry safety — `iplane up`, the phase-1.5 wrapper, a future `ClusterManager.Form()`, the operator hitting up-arrow after a network blip — has to wrap Spawn in its own List-then-Spawn dance, and each of them has to get the partial-failure semantics right independently. Pushing the contract down to the Spawn call means the dance is implemented once, correctly, where the state machine already lives. Mandatory rather than optional, because optional-with-hatch is operationally identical to default-optional: the operators who would skip the safety are the operators who most need it (the chapter's own act-2 story is a forgotten pod). The cost — typing `--id my-pod` — is not real friction.

The lookup order (always run, no opt-in branch):

1. **Local state.** The CLI keys the state file by `(operator, id)`, cross-provider. If a record exists in `pending` or `active` state for this ID on *any* provider, return it. No provider call. A spec asking for a provider different from the existing record's provider is a CLI error — destroy and recreate to move providers.
2. **Provider state (target only).** If local state has no record, call `provisioner.List(ctx, {Tags: {"iplane-id": id, "iplane-operator": operator}})` on the target provider from the spec. If a match exists with the provider reporting active-equivalent status, write it to the local state file (heal) and return it.
3. **Fresh spawn.** If neither has a record, call `provisioner.Spawn(ctx, spec)`. The adapter stamps `iplane-id` and `iplane-operator` on the provider instance.

Step 2 is what makes the idempotency contract honest across laptops and across wiped state files on the target provider. The narrower-than-step-1 scope (target provider only, not all providers) is deliberate: scanning every configured provider on every Spawn doubles or triples API cost on the happy path, and the failure it would catch — an ID-duplicate on a non-target provider with no local record — is what the post-v0.1 reconcile loop is for.

Collision semantics:

- Match in `pending` or `active`: return existing, no Spawn call. The operator's intent ("make sure this exists") is satisfied.
- Match in `terminating`, `terminated`, or `failed`: treat as gone. Spawn a fresh instance under the same ID. The ID is reusable; the previous record stays in the state file for audit but is no longer the active binding.
- The new spawn gets a new `ProviderID`; the iplane ID is the same. Logs and metrics keyed only by `iplane-id` will blur the two lifetimes — keys that need lifetime granularity use `(id, created_at)` or `ProviderID` instead.

ID is immutable once written. Rename is destroy-and-recreate, same rule as resize.

Format constraint: DNS-safe, `^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`, plus the reserved prefix rule — IDs may not start with `iplane-`. The DNS constraint eliminates a class of escaping problems before IDs start appearing in OTel resource attributes, cluster manager records, or hostnames in v0.2 onward. The reserved prefix preserves the `iplane-<random>` namespace for a future relaxation to auto-generated IDs, without committing to that path now.

Concurrency limit, documented honestly: two same-ID invocations on the same laptop are serialized by the state-file flock, so the race window is closed for v0.1's single-operator case. Two operators on different laptops racing on the same ID on the same target provider will both succeed at step 3 and produce two provider instances; the next `iplane instance list --remote` surfaces the duplicate. The full cross-laptop dedup contract waits for v1.0's multi-operator backend, where the lock is a real distributed primitive rather than a `flock(2)` on `~/.iplane`.

### State enumeration

```
pending      // record written, provider call has not returned
active       // provider returned success, SSH reachable, billing started
terminating  // terminate requested, provider call has not returned
terminated   // provider confirms gone (or returned not-found)
failed       // provider returned an error; resolution requires operator action
```

The two `*ing` states exist so partial-failure recovery can tell "we asked but do not know" from "we tried and failed." Single-state models cannot distinguish those, and the cost of getting it wrong is either silent over-charging (assume active) or silent leakage (assume terminated).

### State-file schema

Path: `~/.iplane/state.json`. Atomic write via write-temp-then-rename; flock the directory on write to keep concurrent CLI invocations from racing.

```json
{
  "schema_version": "1",
  "backend": "local-file",
  "operator_id": "default",
  "instances": {
    "my-pod": {
      "id": "my-pod",
      "provider": "runpod",
      "provider_id": "rp-7c8e",
      "state": "active",
      "spec": { "...": "..." },
      "region": "US-CA-1",
      "gpu": { "class": "small", "sku": "RTX 4090", "count": 1, "vram_gb": 24 },
      "hourly_rate_usd": 0.39,
      "created_at": "2026-05-10T18:22:11Z",
      "activated_at": "2026-05-10T18:23:47Z",
      "ssh": { "host": "...", "port": 22, "user": "root" },
      "provider_data": {}
    }
  }
}
```

Four fields earn their space at the top level. `schema_version` is the migration anchor; a v1.0 reader inspects it and decides whether to up-convert before parsing. `backend` records *who wrote this*: v0.1 always writes `local-file`, but v1.0's remote backend will fall back to file mode in degraded states and needs to know which writer last touched the file. `operator_id` is `"default"` in v0.1 and exists so multi-operator sync in v1.0 has somewhere to namespace each operator's instances without re-shaping every record. `instances` is a map keyed by `id`, not a list — O(1) lookup by the same key the CLI and tag system use, and avoids the ordering problems lists invite during concurrent edits.

The map is keyed by `id` (cross-provider), not `(provider, id)`. This matches the tenant-global uniqueness rule on `Spec.ID`: an operator cannot have two records with the same ID even if they're on different providers, so a flat keying surfaces collisions at write time rather than letting them hide behind a compound key.

The forward-compatibility property the schema must hold: a v0.1 binary reading a v1.0-written file must either parse it (preferred) or refuse cleanly with "this file was written by a newer version, upgrade." Unknown top-level keys are ignored; unknown fields inside an instance are preserved in `provider_data`. This is the minimum we owe a reader who pinned to `ch06-final` two years from now.

What we are *not* baking in: per-operator file paths, file-watch semantics, conflict-resolution policy. Those are all v1.0 concerns. But none of them require schema changes — they layer on top.

### Dry-run pattern

Every state-changing CLI command accepts `--dry-run`. With it, the command prints the planned operations and the projected cost, then exits with code 0 having performed no provider calls and written nothing to state.

The Provisioner interface gains no dry-run method. Dry-run lives in the CLI layer, not the Provisioner. The CLI:

1. Resolves the spec (class→SKU lookup, region validation against the bundled table).
2. Looks up the hourly rate from `providers.yaml`.
3. Runs the cost guardrail (see below).
4. Prints the resolved spec, the rate, the state-file changes that would occur, and any guardrail outcome.
5. Exits.

The alternative — a `Validate(ctx, spec) error` method on the Provisioner — was considered for catching "this SKU does not exist in this region" before exit. Rejected for v0.1: it requires a provider API call for honest output, and the dry-run promise of *zero provider side-effects* is more valuable than catching SKU-typo errors a moment earlier. Dry-run shows intent; Spawn validates reality. The cost is documented on the `--dry-run` help text.

Phase 1.1 commits to the seam: every `iplane instance` subcommand parses `--dry-run` and short-circuits before the provisioner call. Phase 1.5 wires it through every state-changing command added by phases 1.2 – 1.4 and adds the global flag.

`List` is not state-changing and is exempt; `iplane instance list` and `iplane instance list --remote` always read live provider state. `--dry-run` on a read-only command would be a no-op and is rejected at flag parse.

### Error model

Provider errors surface up with the original message preserved. The wrapping type:

```go
type ProviderError struct {
    Provider string // "runpod" | "local"
    Op       string // "spawn" | "terminate" | "describe" | "list"
    Cause    error  // the original error from the provider SDK or HTTP layer
    HTTP     int    // HTTP status if available, 0 otherwise
}

func (e *ProviderError) Error() string { ... }
func (e *ProviderError) Unwrap() error { return e.Cause }
```

`errors.Is` and `errors.As` walk through. The cost-guardrail check, the retry logic that lands in phase 1.5, and the future reconcile loop all key off the cause. Normalizing provider errors into iplane-canonical codes was considered and rejected: when a RunPod 429 hits, the operator needs to see the rate-limit header and the retry-after, and a translated `ErrTransient` strips that off.

What we will not do: swallow errors at boundaries. If `state.WriteAtomic` fails after `Spawn` succeeded, the leaked-instance scenario is real and the operator needs both errors. Return them with `errors.Join` and let the CLI's error printer surface both lines. The recovery procedure (manual destroy via provider console, then `iplane instance reconcile --remove-stale`) is documented in the runbook section of the chapter.

### Failure-mode contract

The state-file is an *intent log*, not a snapshot. Three sins to avoid:

1. Call `provisioner.Spawn`, then crash before recording state → leaked instance, no local record.
2. Record `active` before the provider call returns → ghost record, no instance.
3. Treat partial failure as fatal → operator cleans up by hand.

The contract for `iplane instance create`:

1. Run the idempotency lookup (local state cross-provider, then `List` by `iplane-id` tag on the target provider). On a hit in `pending` or `active`, return the existing record without writing a new one. On a hit in `terminated`/`failed`, fall through to step 2 with the same ID.
2. Write a `pending` record carrying the spec, the ID, and a `created_at` timestamp.
3. Call `provisioner.Spawn`.
4. On success, patch the record to `active`: fill `provider_id`, `activated_at`, `ssh`, `hourly_rate_usd`, `region` as scheduled, `gpu` as scheduled, `provider_data`.
5. On failure, patch the record to `failed`, attach the wrapped error.

For `iplane instance destroy`:

1. Patch the record to `terminating`.
2. Call `provisioner.Terminate`.
3. On success, patch to `terminated` with `terminated_at` set.
4. On failure, patch to `failed` and surface the error.

Crash recovery: on next `iplane instance list`, the CLI walks any record in `pending` or `terminating` and calls `provisioner.List(ctx, {iplane-id: record.id})` to ask the provider what it actually knows. If a match comes back, the record is patched to its real state (`active` or `terminated`). If no match comes back, the record is flagged as "uncertain" and surfaced with the suggested follow-up (`iplane instance destroy --force` to clear, or wait for provider eventual consistency). This is the v0.1 scope: per-record self-heal for records the operator already knows about. Full reconcile — surfacing leaked provider instances the operator has *no* local record of — composes the same `List` call with no tag filter and ships post-v0.1.

### Cost guardrail

`iplane instance create` refuses any spec whose computed hourly rate exceeds `$1/hr` unless the operator passes `--yes-i-know`.

Mechanism: the CLI looks up the rate from `providers.yaml` after resolving class→SKU. If the rate exceeds the threshold and `--yes-i-know` is absent, the CLI prints the rate, the threshold, and the override flag, and exits with code 2 (distinct from generic failure). With `--dry-run`, the guardrail outcome is part of the printed plan whether or not it would block.

The threshold default of $1/hr was chosen because the chapter's happy path (24 GB consumer GPU on RunPod) runs at ~$0.30 – 0.50/hr and is unblocked, while an accidental H100 selection (~$2 – 3/hr) is caught before the API call. The override is a flag, not a config setting, so the rejection appears in shell history and the audit trail is intact for "why did we spend $400 on Saturday."

The flag spelling — `--yes-i-know` rather than `--force` or `--confirm` — was chosen because it does not look like a routine option. Operators don't tab-complete it absentmindedly. The phrasing reads as the operator answering a question, which is the social contract: iplane asks "are you sure," the flag is the answer.

What is deferred to v1.0: live spend caps (per-day, per-month), per-operator budgets, and any policy that requires querying the multi-operator state. The threshold itself is a single number in v0.1; making it a policy expression is a v1.0 concern.

### Provisioner / deploy split on RunPod

Phase 1 provisions a generic Docker-capable base image. Phase 2 (`iplane deploy`) does `docker run` over SSH on the acquired instance.

The alternative was to lean on RunPod's container-at-creation API and skip the SSH step entirely. That would save ~2 minutes on bring-up for the chapter's happy path. Rejected because:

- Vast.ai's container model differs from RunPod's, and Lambda Labs is bare-metal-only.
- AWS does not have a comparable primitive; you boot a VM and run a container service.
- If the v0.1 abstraction is shaped to RunPod's fast path, every other provider becomes a special case from v0.2 onward.

The slower bring-up is an explicit trade. The base image ref is a field on `Spec`, so providers that *do* have an optimized image (RunPod's `runpod/pytorch:...`) can ship one without bending the interface, and the phase-2 deploy primitive layered on top stays the same code path across providers.

What this means for the chapter narrative: act 2 (the manual path) gets the SSH + docker dance honestly, and act 3 (the iplane move) shows a two-line bring-up that hides it. The portability argument is part of act 4 (design choices). Without committing to the Docker-base shape now, act 4 would have to back-explain the RunPod-specific path in v0.2.

### GPU spec language

Primary surface: `--gpu-class small|medium|large|xlarge`. Maps to the chapter taxonomy:

- `small`: 24 GB consumer (RTX 4090, RTX 5090). Ch 6 default.
- `medium`: 40 – 48 GB (A6000, A100 40 GB). Used by Ch 9 (30B class).
- `large`: 80 GB (A100 80 GB, H100 80 GB). Used by Ch 12 (70B class).
- `xlarge`: 96 GB+ (H100 96 GB, H200, B100/B200 as they ship). Used by Ch 13 (400B class, multi-host).

Escape hatch: `--gpu-sku <provider-sku>`. Sets `Spec.GPU.SKU` directly, bypassing the class table. The two flags are mutually exclusive; supplying both is a CLI error.

Each provider ships a class→[SKU] table in its package (`internal/provisioners/runpod/skus.go`, `…/local/detect.go`). When a class has no SKU mapping on a provider, `Spawn` returns a `ProviderError` listing the available classes for that provider. We do not silently degrade to a different class; the operator has to make the choice explicit.

The reason this lives at the *class* level and not the SKU level on the primary surface: the book teaches one taxonomy, not N provider vocabularies. The chapter on 70B deployment talks about "large" instances; the operator may run that chapter on RunPod, Lambda, or local without re-learning what to type. The SKU escape hatch absorbs the cases the taxonomy does not cover and keeps the abstraction honest.

### Region selection

`Spec.Region` is required. No auto-region in v0.1. The CLI rejects `iplane instance create` if `--region` is missing, with the error listing known regions for the chosen provider.

Auto-region needs three inputs we do not have reliably in v0.1: per-region capacity signal (providers do not expose this consistently), locality preference (closest to operator? closest to user traffic?), and per-region cost variation. v0.2 brings `ModelStore` caching, which pins data to a region anyway, at which point region selection becomes load-bearing for performance, not just latency.

Forcing operator choice now means we do not teach a v0.1 auto-region rule that v0.2 then has to unteach. The bundled region list per provider is a static table in v0.1; refreshing it via a provider API is post-v0.1.

## Open questions resolved

| Question (from ROADMAP.md)                         | Decision                                                                            |
|----------------------------------------------------|-------------------------------------------------------------------------------------|
| Provisioner / deploy split on RunPod               | Generic Docker base in phase 1; `docker run` over SSH in phase 2. Portability over speed. |
| State-file hygiene                                 | Two-phase write: `pending` → `active` (Spawn), `terminating` → `terminated` (Destroy). |
| Cost guardrail                                     | $1/hr default, `--yes-i-know` override. Exit code 2 distinct from generic failure.   |
| GPU spec language                                  | `--gpu-class` primary, `--gpu-sku` escape hatch. Mutually exclusive.                |
| Region selection                                   | Required field. Auto-region deferred to v0.2 alongside ModelStore caching.          |
| State-file schema forward-compat                   | `schema_version`, `backend`, `operator_id` top-level; `provider_data` as raw JSON.   |
| Identity model (added during 1.1 review)           | One field `Spec.ID`, mandatory, operator-supplied, tenant-globally unique. Positional on the CLI. `iplane-` prefix reserved. |
| Cluster shape (added during 1.1 review)            | Provisioner stays per-instance. Cluster lifecycle layers above via `cluster=<id>` tag (v0.2) and a `ClusterManager` sibling (v0.3). Optional `SpawnGroup` capability for native-fleet providers. |

## What this enables

Phase 1.2 implements the interface and ships the local adapter (no provider API key required to read the chapter). Phase 1.3 ships the RunPod adapter against the same interface, and the existence of two implementations from day one is what proves the abstraction is honest — single-implementation interfaces lie. Phase 1.4 adds the `iplane instance` CLI, which talks only to the Provisioner interface and the state file; it does not import `runpod` or `local` directly. Phase 1.5 layers `--dry-run` globally on top of the seam phase 1.1 commits to.

Outside v0.1, the contract earns its space in four places:

- v0.2's Lambda Labs adapter slots into the same interface. The class table is the work; the rest is plumbing.
- v0.2's `RunPodVolumeStore` work groups N pods sharing a cache via the `cluster=<id>` tag without an interface change.
- v0.3's `ClusterManager` composes Spawn N times, derives member identity as `<cluster-id>-<ordinal>` from `Spec.ID`, and gets retry safety for free.
- v1.0's multi-operator state sync replaces the storage backend. The schema's `backend` and `operator_id` fields, plus the `iplane-operator` tag stamped on every instance, are the seams.

The reconcile loop (post-v0.1) layers on top of the per-record self-heal already in v0.1: same `List` call, no tag filter, surfacing leaked instances the operator has no record of. The state machine and tag convention designed here are what make that recovery loop trivial.

## What is deferred

- Plan-value APIs from the Provisioner. Dry-run is a CLI concern in v0.1; if dry-run later needs to surface SKU-existence errors, a `Validate(ctx, spec) error` method is the path. Adding another method is non-breaking.
- Full reconcile workflow (leaked-instance detection across the whole operator account, batch import, drift diff). v0.1 ships per-record self-heal via `List` by `iplane-id` tag; the no-filter walk and the surfacing UX wait for `iplane instance reconcile` post-v0.1.
- Cross-laptop dedup on `Spec.ID`. v0.1 serializes via state-file flock on one laptop; the distributed lock waits for v1.0's multi-operator backend.
- Cross-provider orphan detection at Spawn time. Step 2 of the idempotency lookup only consults the target provider; a duplicate ID with no local record sitting on a non-target provider is caught by the post-v0.1 reconcile loop rather than at Spawn.
- `SpawnGroup(ctx, spec, n)` capability for providers with native fleet primitives (AWS ASG, GCP MIG). Type-asserted by the future `ClusterManager`. Optional; not part of the core interface.
- Live spend caps and per-operator budgets. v1.0, alongside multi-operator state.
- Per-region capacity feedback. v0.3, when provider mix is broad enough for the routing layer to consume it.
- Auto-region. v0.2, paired with `ModelStore` caching that makes region choice load-bearing for performance.
- Networking-aware specs (NVLink/RDMA flags). v0.3, when multi-host deploys force the question.

## Notes for the chapter draft

Ch 6's four-act sketch, against the framework in `book/REVIEWS.md`:

- **Act 1 (principle):** Self-hosted inference is GPU-hours times rate. The operator's first move is acquiring the GPU, and the cost of getting that wrong (typo'd SKU, forgotten pod, wrong region) is denominated in real dollars per hour.
- **Act 2 (manual path):** RunPod web console → SKU select → region select → SSH key paste → boot wait → SSH in → docker pull → docker run → wait for `/health`. Each step a place to mis-click or forget. The forgotten-pod-cost-me-$80 story is in this act, with the receipt.
- **Act 3 (iplane move):** `iplane instance create my-pod --provider runpod --gpu-class small --region US-CA-1` returns a record. Re-running the same command after a network blip returns the same record, not a duplicate. `iplane instance destroy my-pod` returns it.
- **Act 4 (design choices):** Why mandatory IDs (every dollar of spend has your name on it). Why class-not-SKU. Why state-pending/state-active. Why the cost guardrail defaults on. Why dry-run is in the CLI layer, not the interface. Why provider errors surface raw.
- **Act 5 (what is next):** The instance is acquired but empty. Phase 2 (`iplane deploy`) puts the engine on it.
