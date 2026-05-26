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

## v0.2 trajectory

When v0.2's `CachedStore` lands, it wraps `huggingface.Store`:

```go
// Sketch — not in v0.1
cs := cachedstore.New(
    huggingface.New(token),
    runpodvolume.NewStore(volumeID),
)
provisioners.WithModelStore(cs)
```

`CachedStore.Resolve` would:

1. Delegate to HF for validation (same as today).
2. If the model isn't cached on the volume, do a one-time download
   inside a setup pod that writes to the network volume.
3. Return `Resolved{EngineModelArg: "/cache/<model>", Mounts: [...]}`
   so the engine loads from the mounted path instead of re-downloading.

The Service code doesn't change; the wrapper just produces a richer
`Resolved`. That's the whole point of the seam.

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
- **`iplane serve` doesn't validate.** The long-running provisioner-
  serve mode doesn't construct a ModelStore today (it isn't wired
  through `serve.go`); only the in-process CLI verbs do. If you use
  `--service-url` against a remote `iplane serve`, validation is
  effectively off regardless of the flag.
