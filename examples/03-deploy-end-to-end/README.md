# 03-deploy-end-to-end

Walks through the full v0.1 control plane loop end-to-end:

```
provision -> deploy -> serve -> destroy
```

A demokit walkthrough that exercises both the `ProvisionerService` and the
`DeploymentService` against a real RunPod pod. The deployment executor SSHes
into the provisioned pod and runs docker -- no operator-side docker daemon
required.

## Cost

This example **always** runs against RunPod (local-provider instances have
no SSH endpoint, so v0.1 cannot deploy to them). Cost depends on the model
size you pick in the walkthrough's interactive step:

| Size  | Model                          | VRAM   | Cold start | Run cost |
|-------|--------------------------------|--------|-----------|----------|
| 1.5B  | `Qwen/Qwen2.5-1.5B-Instruct`   | ~3 GB  | 30-60s    | ~$0.02   |
| 3B    | `Qwen/Qwen2.5-3B-Instruct`     | ~6 GB  | 60-90s    | ~$0.05   |
| 7B    | `Qwen/Qwen2.5-7B-Instruct`     | ~14 GB | 90-180s   | ~$0.12   |

All three are open-weights (no HuggingFace gated-access dance) and fit on
a 24 GB small-class GPU. Defaults to 1.5B for the smoke-test path.

The demo always defer-terminates and catches Ctrl-C; if anything goes
wrong, the pod URL is printed and you can clean up via the RunPod console:
https://www.runpod.io/console/pods

## Run

```bash
# Terminal 1 (server)
export RUNPOD_API_KEY=...
make serve

# Terminal 2 (demo)
make demo
```

The walkthrough is interactive: when it gets to "Choose a model size," pick
one of the three options (defaults to 1.5B if you just press Enter).

## What the walkthrough exercises

1. Service reachability (ListInstances ping)
2. **Interactive model-size choice** (1.5B / 3B / 7B)
3. CreateInstance with `class=small` (RunPod resolves to the cheapest 24 GB
   SKU; the Service registers an Ed25519 public key on the RunPod account
   so the new pod gets it pre-installed)
4. CreateDeployment with `Wait=true` (the Service spawns the executor
   goroutine; the executor SSHes in, pulls the vLLM image, runs the
   container with `--gpus all`, and polls `localhost:8000/health` until
   2xx)
5. GET `/v1/models` on the engine endpoint (proves the OpenAI-compatible
   surface is live; an OpenAI SDK pointed here would work)
6. DestroyDeployment (stops + removes the container; instance keeps running)
7. DestroyInstance (terminates the pod; billing stops)

## Record a trace

```bash
make readme
```

Generates `RUN.md` from a real recorded run. The committed `RUN.md` here is
the output of one such recording at the 1.5B default.
