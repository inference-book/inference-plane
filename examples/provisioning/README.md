# Provisioning examples

Walkthroughs of the v0.1 instance lifecycle: acquire, describe, list, destroy. Each example exercises the same `provisioners.Service` surface that the CLI (phase 1.4) wraps — useful as a starting point for callers embedding iplane as a library, and as a debugging tool when a provider integration misbehaves.

## What's here

- **[01-end-to-end/](01-end-to-end/)** — Full lifecycle (create → describe → idempotent re-create → list → destroy) via a demokit walkthrough. Two providers wired: `local` (default, $0, no API key) and `runpod` (real pod, ~$0.02 per run, requires `RUNPOD_API_KEY`). Exercises the failure-mode contract (idempotency, state-file hygiene, list with self-heal) against either backend.

## What's not here (yet)

- A local-only walkthrough that requires no API key. Likely lands when `--dry-run` (phase 1.5) makes the cost-free path more interesting.
- A multi-provider walkthrough showing the same workload landing on RunPod or Lambda Labs (v0.2).

## Design context

The Service implements the failure-mode contract from [docs/design/0001-provisioner.md](../../docs/design/0001-provisioner.md). Each example narrates one path through that contract so a reader sees the call flow: which calls hit the local state file vs. the provider, which calls are idempotent on retry, where class shorthand expands into numeric constraints.
