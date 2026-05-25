# Deploy end-to-end

Provision a GPU instance, deploy vLLM with an OpenAI-compatible API, hit /v1/models to prove it serves, then tear it all down.

## What you'll learn

- **Check the service is reachable** — CLI form:
- **Choose a model size** — All three are open-weight Qwen models that fit on a 24 GB small-class GPU. Bigger = more capable but slower cold-start and more $.
- **Deploy: provision a pod running the engine image, wait for RUNNING** — One step. CreateDeployment with no instance_id auto-provisions: the control plane rents a small-class pod whose container IS the engine image (image-as-pod). The engine port is reverse-proxied via the provider's HTTPS proxy (no publicIp allocated -- cheapest community capacity), and we HTTP-poll /health on the proxy URL until 2xx. No SSH, no docker-in-docker, no NAT. The instance + deployment are recorded 1:1 (two views -- GPU and model -- of the same pod). Want shell access for debugging? Re-run with --debug-shell (pays the publicIp fee + restricts placement).
- **Hit /v1/models to prove the engine serves** — vLLM's OpenAI-compatible surface exposes /v1/models for the served-model list. A 2xx here means a real OpenAI SDK can hit /v1/chat/completions next.
- **Serve a real inference request** — The chapter's payoff. Single-turn /v1/chat/completions: prompt in, tokens out. iplane is NOT in the data path -- this POST goes from the operator's laptop straight to the engine_endpoint (the provider's HTTPS proxy URL). What iplane delivered was 'a dialable endpoint serving an OpenAI-compat API'; this step proves the operator can use it.
- **Destroy the deployment (tears down the pod)** — Terminates the engine pod. Because this deployment auto-provisioned its instance (1:1), the pod IS the instance -- destroying the deployment terminates the pod and marks both records TERMINATED. (For an explicitly-placed deployment on a shared instance, the instance would survive.) Idempotent: already-TERMINATED is a no-op.

## Flow

```mermaid
sequenceDiagram
    participant Operator as You
    participant iplane as Provisioner + Deployment Service
    participant State as state.json
    participant RunPod as GPU provider
    participant Pod as Provisioned GPU instance
    participant Engine as vLLM container on the pod

    Note over Operator,Engine: Step 1: Check the service is reachable
    Operator->>iplane: ListInstances (empty filter)

    Note over Operator,Engine: Step 2: Choose a model size

    Note over Operator,Engine: Step 3: Deploy: provision a pod running the engine image, wait for RUNNING
    Operator->>iplane: CreateDeployment{image=vllm, model=qwen, class=small, wait=true}
    iplane->>State: write PENDING (instance + deployment)
    iplane->>RunPod: create pod with engine image + model
    iplane->>Engine: HTTP-poll /health until 2xx
    iplane->>State: patch RUNNING + engine endpoint

    Note over Operator,Engine: Step 4: Hit /v1/models to prove the engine serves
    Operator->>Engine: GET /v1/models

    Note over Operator,Engine: Step 5: Serve a real inference request
    Operator->>Engine: POST /v1/chat/completions (single-turn)
    Engine->>Operator: {choices:[{message:{...}}], usage:{...}}

    Note over Operator,Engine: Step 6: Destroy the deployment (tears down the pod)
    Operator->>iplane: DestroyDeployment{id}
    iplane->>RunPod: terminate pod
    iplane->>State: patch deployment + instance to TERMINATED
```

## Steps

### Setup

This walkthrough deploys a model with one command. The control plane provisions a GPU pod whose container IS the engine image (image-as-pod) -- no SSH, no docker-in-docker. The instance + deployment are recorded 1:1 (the instance shares the deployment id: two views, GPU and model, of the same pod).
Target URL:    http://localhost:9091
Provider:      runpod
Deployment id: demo-llama-20260525t005443 (the instance shares this id)
Cost depends on chosen model size + cold-start. The 1.5B default is ~$0.02 for a full run; 7B is ~$0.12. Defer-terminates on exit / Ctrl-C.

### Step 1: Check the service is reachable

CLI form:
  iplane instance list --service-url http://localhost:9091

### Step 2: Choose a model size

All three are open-weight Qwen models that fit on a 24 GB small-class GPU. Bigger = more capable but slower cold-start and more $.

### Step 3: Deploy: provision a pod running the engine image, wait for RUNNING

One step. CreateDeployment with no instance_id auto-provisions: the control plane rents a small-class pod whose container IS the engine image (image-as-pod). The engine port is reverse-proxied via the provider's HTTPS proxy (no publicIp allocated -- cheapest community capacity), and we HTTP-poll /health on the proxy URL until 2xx. No SSH, no docker-in-docker, no NAT. The instance + deployment are recorded 1:1 (two views -- GPU and model -- of the same pod). Want shell access for debugging? Re-run with --debug-shell (pays the publicIp fee + restricts placement).

CLI form:
  iplane deployment deploy demo-llama-20260525t005443 --provider runpod --class small --image vllm/vllm-openai:v0.7.0 --model <chosen> --service-url http://localhost:9091

### Step 4: Hit /v1/models to prove the engine serves

vLLM's OpenAI-compatible surface exposes /v1/models for the served-model list. A 2xx here means a real OpenAI SDK can hit /v1/chat/completions next.

CLI form (no native verb; uses the engine_endpoint from `iplane deployment describe`):
  endpoint=$(iplane deployment describe demo-llama-20260525t005443 --service-url http://localhost:9091 -o json | jq -r .engine_endpoint)
  curl -fsS "${endpoint}/v1/models"

### Step 5: Serve a real inference request

The chapter's payoff. Single-turn /v1/chat/completions: prompt in, tokens out. iplane is NOT in the data path -- this POST goes from the operator's laptop straight to the engine_endpoint (the provider's HTTPS proxy URL). What iplane delivered was 'a dialable endpoint serving an OpenAI-compat API'; this step proves the operator can use it.

CLI form:
  iplane deployment query demo-llama-20260525t005443 "Say hello in one sentence." --service-url http://localhost:9091

### Step 6: Destroy the deployment (tears down the pod)

Terminates the engine pod. Because this deployment auto-provisioned its instance (1:1), the pod IS the instance -- destroying the deployment terminates the pod and marks both records TERMINATED. (For an explicitly-placed deployment on a shared instance, the instance would survive.) Idempotent: already-TERMINATED is a no-op.

CLI form:
  iplane deployment destroy demo-llama-20260525t005443 --service-url http://localhost:9091

### Done

Pod terminated -- billing stopped. Because the deployment auto-provisioned its instance (1:1), destroying the deployment tore down the pod; the instance record is marked TERMINATED in the same step.
The instance + deployment records remain in the state file as TERMINATED -- an audit trail of what ran. Re-running provisions a fresh pod (each run gets a new timestamped id).

## Run it

```bash
go run ./examples/03-deploy-end-to-end/
```

Pass `--non-interactive` to skip pauses:

```bash
go run ./examples/03-deploy-end-to-end/ --non-interactive
```
