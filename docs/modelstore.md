# ModelStore — operator-supplied model specs → engine-consumable form

## Why it exists

The operator types `iplane up --model Qwen/Qwen2.5-1.5B-Instruct`.
That string flows through the deployment proto into the pod's engine
argv (`vllm serve --model Qwen/...`). At which point the engine
fetches weights from HuggingFace. iplane is **not** in the weight-
download path.

So why have a "ModelStore" at all in v0.1? Two reasons:

1. **Pre-flight validation.** A typo (`Qwen/Qwen2-1.5B-Instruct` vs
   `Qwen/Qwen2.5-1.5B-Instruct`) costs ~$0.10–0.50 today: pod
   provisions, vLLM starts, vLLM fails to load the model, deploy goes
   FAILED ~3 minutes in. Catching it with a single HF API call (one
   round-trip, ~200 ms) before provisioning is a strict win.

2. **Seam for v0.2's caching layer.** v0.2's `RunPodVolumeStore` +
   `CachedStore` wrapper pre-populates a network volume on first
   resolve and returns a mount path on subsequent ones — so N pods
   sharing the same model don't each re-download. The interface lives
   in v0.1 so v0.2's wrappers can drop in without proto changes.

## The interface (v0.1)

```go
// internal/modelstores/modelstore.go
type ModelStore interface {
    Resolve(ctx context.Context, spec string) (Resolved, error)
}

type Resolved struct {
    EngineModelArg string             // what goes into --model
    EnvOverrides   map[string]string  // e.g. HF_TOKEN propagation
    Mounts         []Mount            // future: cached-volume paths
}
```

Service-internal: the Service calls `Resolve` at `CreateDeployment`
time, merges `EnvOverrides` into `dep.Env`, then proceeds with
provisioning. A Resolve error surfaces as `codes.InvalidArgument` —
no pod is created.

## Optional capabilities (v0.3, Ch 12)

`Resolve` is the whole required interface. Anything a store *may* be
able to answer is an optional capability, asserted at the call site,
the way `provisioners` treats `FailureReporter` / `KeyRegistrar` /
`VolumeManager`:

```go
// internal/modelstores/modelstore.go
type ArchitectureSource interface {
    Architecture(ctx context.Context, req *provisionerv1.DescribeModelRequest) (*provisionerv1.DescribeModelResponse, error)
}
```

Only `huggingface.Store` implements it, because it is the only store
with a hub to ask. `DescribeModel` asserts and returns
`codes.Unimplemented` when the assertion fails, which is what
`iplane model describe` and `iplane model budget` answer through.

**A decorator has to forward every capability by hand, and the failure
is silent.** `volumecache.Store` wraps a base store and satisfies
`ModelStore`, so it compiles and passes its own tests while dropping
anything the base could answer beyond `Resolve`. A daemon with
`model_cache` enabled then reports a capability the configured store
genuinely has as absent.

It shipped that way once (issue 324, fixed). Nothing catches it: the
type assertion is the only enforcement, it happens at runtime, and every
test that builds a service with a bare store passes. So a new optional
capability needs four things rather than one. The interface, the
implementation, a forwarding method on every decorator, and a test that
builds the store the way `serve` builds it.

Forwarding creates a second problem the sentinel solves. A wrapper has
to satisfy the interface in order to forward it, so past a decorator the
type assertion no longer separates "cannot report a shape" from "cannot
report this model's". `modelstores.ErrNoArchitectureSource` carries that
distinction, and `DescribeModel` maps it back to `Unimplemented`. The
two codes send an operator to different places, one to their store
configuration and the other to their model spec, so collapsing them
costs a debugging session.

## Two impls

| Impl | When | Behavior |
|------|------|----------|
| `modelstores.Passthrough{}` | Tests, `--skip-model-validation` | Returns the spec unchanged, no env, no validation. Network-free. |
| `huggingface.Store` | Default for CLI verbs | Validates against `huggingface.co/api/models/<id>` with a 5s timeout. Propagates `$HF_TOKEN` if set on the operator's shell. |

## What the HF store catches

The HF model-info endpoint returns 200 / 404 / 401 / 403 with a body
that's easy to map to actionable errors:

| HF response | iplane error |
|-------------|--------------|
| 200, `disabled: false` | proceed, propagate HF_TOKEN |
| 200, `disabled: true` | `model %q has been disabled on huggingface.co` |
| 404 | `model %q not found on huggingface.co (typo? or unpublished revision)` |
| 401 (no token / gated) | `model %q is gated; set HF_TOKEN with read access and retry` |
| 403 (token without perms) | `model %q is gated and HF_TOKEN lacks access; accept the model license on huggingface.co/%s` |
| network error / 5xx | `HF API unreachable; --skip-model-validation to bypass` |

Each error is actionable enough that the operator can recover
without re-reading the docs.

## Operator surface

```bash
# Normal flow — validation on by default
iplane up --model Qwen/Qwen2.5-1.5B-Instruct

# Gated model — HF_TOKEN propagation kicks in
export HF_TOKEN=hf_...
iplane up --model meta-llama/Meta-Llama-3-8B

# Offline / firewalled / self-hosted model
iplane up --model my-org/my-private-model --skip-model-validation

# Same flag works with the per-deployment verbs
iplane deployment deploy my-llama --provider runpod --class small \
    --image vllm/vllm-openai:v0.7.0 \
    --model my-org/my-private-model --skip-model-validation
```

The `--skip-model-validation` flag is a root-level persistent flag, so
it applies to every verb that constructs an in-process Service
(`up`, `deployment deploy`, `instance create`, etc.).

## Warm model cache (v0.2, Ch 9)

`volumecache.Store` wraps a base store (`huggingface.Store`) so a
deployment mounts a pre-staged volume instead of re-downloading the
model on every deploy. It is provider-agnostic: it stamps an opaque
`Mount` (a provider volume handle and/or a host path, plus the
in-container attach path) and redirects `HF_HOME` at it. No
provider-specific branch lives in the store — which is why it carries
no provider name.

```go
vc := volumecache.New(
    huggingface.New(token),               // still validates + propagates HF_TOKEN
    modelstores.Mount{VolumeID: volumeID, MountPath: "/models"},
)
provisioners.WithModelStore(vc)
```

`volumecache.Store.Resolve`:

1. Delegates to the base for validation (same as the plain HF path).
2. Adds `HF_HOME=<mount>/hf` so the engine finds the staged HF cache,
   leaving `--model` as the HF id (the cache hits via the env redirect,
   not by rewriting `--model`).
3. Appends the `Mount`, which the Service stamps onto
   `Deployment.mounts`. Each deploy path maps it onto its own
   primitive: the RunPod adapter onto `networkVolumeId`, the sshdocker
   executor onto a `docker run -v` bind. Paths with no volume mechanism
   take the cold download path.

The Service code doesn't change; the wrapper just produces a richer
`Resolved`. That's the whole point of the seam.

**Activation is per-config**, via the daemon's `model_cache` block (see
`deploy/config.yaml`), so earlier examples keep the cold path and a
forward example opts in by adding the block.

## Pinning (v0.2 Ch 9)

`iplane model pin` populates the volume the `model_cache` block mounts:

```bash
iplane model pin Qwen/Qwen2.5-32B-Instruct-AWQ --provider runpod --region EU-RO-1
# → creates/finds the region's shared cache volume, downloads the model
#   onto it (via a throwaway CPU pod), prints the volume id to paste into
#   model_cache.volume_id.
iplane model ls                       # volumes + the models staged on each
iplane model unpin <vol> --model <m>  # drop one model from the registry
iplane model unpin <vol>              # destroy the whole volume
```

A volume is a **shared cache**: many models accumulate on one per-region
volume (they sit side by side under one HF layout), so `volume_id` in the
config serves every model pinned to that volume. A volume is
datacenter-locked, so serving a model in two regions means pinning it in
each. The `provider` field on the mount is enforced at deploy time: a
deployment refuses a mount whose provider differs from where it lands
(a RunPod volume can't attach on Lambda), rather than silently running
cold.

Pinning runs **in-process, before `iplane serve`** (the daemon holds the
state lock for its lifetime); remote pinning over `--service-url` is a
follow-up.

**Auto-resolve (no config needed).** A deploy whose `(model, provider,
region)` matches a pinned volume mounts it automatically, so after
`iplane model pin` you can just deploy and it's warm without a
`model_cache` block:

```bash
iplane model pin Qwen/Qwen2.5-32B-Instruct-AWQ --provider runpod --region EU-RO-1
iplane deployment deploy qwen32 --model Qwen/Qwen2.5-32B-Instruct-AWQ \
    --provider runpod --region EU-RO-1 --min-vram-gb 24
# warm automatically -- the registry join (model, provider, region) -> volume
```

An explicit `model_cache` mount still wins (config is the override); a
heterogeneous fleet spanning regions stays cold (one mount can't serve
replicas on different providers).

**On a cache miss** the engine still reaches HF (`HF_HOME` is redirected,
not forced offline), so an unpopulated or misconfigured volume degrades
to a cold deploy rather than failing.

## Limitations the operator should know

- **No model-config validation.** iplane doesn't check that the model
  is compatible with the engine image (vLLM vs Triton, AWQ vs FP16,
  etc.). vLLM's startup catches incompatibility; iplane just
  surfaces the resulting `FAILED` state.
- **No license auto-accept.** Gated models on HF require manual
  acceptance on the model page. iplane's 403 error message points
  at the page; clicking through is a manual step.
- **HF rate limits.** The free API tier rate-limits aggressively. A
  CI pipeline running many deploys back-to-back may hit
  `429 Too Many Requests`; use `--skip-model-validation` in CI or
  set `HF_TOKEN` (authenticated requests have higher limits).
- **Remote `--service-url` uses the daemon's store, not yours.** Both
  the in-process CLI verbs and `iplane serve` construct a ModelStore
  (via `buildLocalService`), so validation and the warm cache are the
  daemon's, configured on the daemon. When you drive a remote daemon
  with `--service-url`, your local `--skip-model-validation` and
  `model_cache` settings do not apply -- the daemon's do.
