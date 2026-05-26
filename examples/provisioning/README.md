# Provisioning examples

Walkthroughs of the v0.1 instance lifecycle: acquire, describe, list, destroy. Each example exercises the same `provisioners.Service` surface that the CLI (phase 1.4) wraps — useful as a starting point for callers embedding iplane as a library, and as a debugging tool when a provider integration misbehaves.

## What's here

- **[01-end-to-end/](01-end-to-end/)** — Full instance lifecycle (create → describe → idempotent re-create → list → destroy) via a demokit walkthrough against the programmatic Service. Two providers wired: `local` (default, $0, no API key) and `runpod` (real pod, ~$0.02 per run, requires `RUNPOD_API_KEY`). Exercises the failure-mode contract (idempotency, state-file hygiene, list with self-heal) against either backend.
- **[02-cli-end-to-end/](02-cli-end-to-end/)** — Same instance lifecycle, but driven through the `iplane instance ...` CLI verbs against a running `iplane serve`. Shows the operator-facing surface readers will use day-to-day; same providers, same cost shape as 01.
- **[03-deploy-end-to-end/](03-deploy-end-to-end/)** — Full **deployment** lifecycle on top of an instance: provision → **interactive model-size choice** (1.5B / 3B / 7B Qwen) → CreateDeployment with `Wait=true` → GET `/v1/models` to prove the engine serves → destroy. RunPod-only (local instances have no SSH endpoint so v0.1 cannot deploy to them). Cost depends on model size (~$0.02 for 1.5B up to ~$0.12 for 7B).

## What's not here (yet)

- A multi-provider deployment walkthrough showing the same workload landing on RunPod / Lambda Labs / Vast.ai (v0.2).
- A logs-tailing walkthrough (`iplane deployment logs -f`) — tracked as a separate followup.

## Design context

The Service implements the failure-mode contract from [docs/design/0001-provisioner.md](../../docs/design/0001-provisioner.md). Each example narrates one path through that contract so a reader sees the call flow: which calls hit the local state file vs. the provider, which calls are idempotent on retry, where class shorthand expands into numeric constraints.
