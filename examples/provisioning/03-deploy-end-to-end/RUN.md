# Deploy end-to-end

Provision a GPU instance, deploy vLLM with an OpenAI-compatible API, hit /v1/models to prove it serves, then tear it all down.

## What you'll learn

- **Choose a model size** — All three are open-weight Qwen models that fit on a 24 GB small-class GPU. Bigger = more capable but slower cold-start and more $.
- **Provision a small-class pod** — class=small => 24 GB VRAM floor. The RunPod resolver picks the cheapest matching SKU. The Service registers an SSH keypair with the RunPod account on first run (PR 24); the resulting pod has that key pre-installed.
- **Deploy the engine and wait for RUNNING** — CreateDeployment with Wait=true blocks until the engine is healthy or the deploy fails. The Service's executor SSHes in, docker-pulls the image, docker-runs it with --gpus all, and polls localhost:8000/health from inside the pod until 2xx.
- **Hit /v1/models to prove the engine serves** — vLLM's OpenAI-compatible surface exposes /v1/models for the served-model list. A 2xx here means a real OpenAI SDK can hit /v1/chat/completions next.
- **Destroy the deployment** — Stops + removes the engine container on the pod. The instance keeps running so a follow-up deploy could reuse it. Idempotent: already-TERMINATED is a no-op.
- **Destroy the instance** — Tearing down the pod stops billing. The instance + deployment records remain in the state file as TERMINATED -- an audit trail of what ran.

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

    Note over Operator,Engine: Step 3: Provision a small-class pod
    Operator->>iplane: CreateInstance{class=small}
    iplane->>State: write PENDING
    iplane->>RunPod: register pub key + spawn pod
    iplane->>State: patch to ACTIVE

    Note over Operator,Engine: Step 4: Deploy the engine and wait for RUNNING
    Operator->>iplane: CreateDeployment{image=vllm, model=qwen, wait=true}
    iplane->>Pod: SSH in
    iplane->>Engine: docker pull + docker run
    iplane->>Engine: poll /health every 2s
    iplane->>State: patch transitions (STARTING -> CONFIGURING -> RUNNING)

    Note over Operator,Engine: Step 5: Hit /v1/models to prove the engine serves
    Operator->>Engine: GET /v1/models

    Note over Operator,Engine: Step 6: Destroy the deployment
    Operator->>iplane: DestroyDeployment{id}
    iplane->>Pod: SSH: docker stop + docker rm
    iplane->>State: patch to TERMINATED

    Note over Operator,Engine: Step 7: Destroy the instance
    Operator->>iplane: DestroyInstance{id}
    iplane->>RunPod: terminate pod
    iplane->>State: patch to TERMINATED
```

## Steps

### Setup

This walkthrough exercises both the Provisioner and Deployment surfaces end-to-end. The deployment executor SSHes into the provisioned pod and runs docker -- no operator-side docker daemon required.
Target URL:    http://localhost:9091
Provider:      runpod
Instance id:   demo-pod-20260521t213958
Deployment id: demo-llama-20260521t213958
Cost depends on chosen model size + cold-start. The 1.5B default is ~$0.02 for a full run; 7B is ~$0.12. Defer-terminates on exit / Ctrl-C.

### Step 1: Check the service is reachable

### Step 2: Choose a model size

All three are open-weight Qwen models that fit on a 24 GB small-class GPU. Bigger = more capable but slower cold-start and more $.

### Step 3: Provision a small-class pod

class=small => 24 GB VRAM floor. The RunPod resolver picks the cheapest matching SKU. The Service registers an SSH keypair with the RunPod account on first run (PR 24); the resulting pod has that key pre-installed.

### Step 4: Deploy the engine and wait for RUNNING

CreateDeployment with Wait=true blocks until the engine is healthy or the deploy fails. The Service's executor SSHes in, docker-pulls the image, docker-runs it with --gpus all, and polls localhost:8000/health from inside the pod until 2xx.

### Step 5: Hit /v1/models to prove the engine serves

vLLM's OpenAI-compatible surface exposes /v1/models for the served-model list. A 2xx here means a real OpenAI SDK can hit /v1/chat/completions next.

### Step 6: Destroy the deployment

Stops + removes the engine container on the pod. The instance keeps running so a follow-up deploy could reuse it. Idempotent: already-TERMINATED is a no-op.

### Step 7: Destroy the instance

Tearing down the pod stops billing. The instance + deployment records remain in the state file as TERMINATED -- an audit trail of what ran.

### Done

Instance and deployment both terminated. State file holds the audit trail (state=TERMINATED on both records).
Re-running this demo provisions a fresh pod -- ids are reusable but each run gets a new timestamped id by default.

## Run it

```bash
go run ./examples/03-deploy-end-to-end/
```

Pass `--non-interactive` to skip pauses:

```bash
go run ./examples/03-deploy-end-to-end/ --non-interactive
```
