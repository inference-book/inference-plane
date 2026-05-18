# 02-cli-end-to-end

Walks through the v0.1 provisioner lifecycle (create → describe → idempotent re-create → list → destroy) **driving the iplane CLI binary directly**. The operator-terminal path: every step shows the exact `iplane instance` command and its actual stdout.

This is the sibling of [01-end-to-end/](../01-end-to-end/), which exercises the same lifecycle through the generated gRPC client. v0.1 ships both transports as first-class — see the `provisionerClient` interface in `cmd/iplane/cmd/instance.go`.

## Run (local, the default)

```bash
make demo
```

Zero cost, no env vars. Builds `iplane` from the local checkout into a temp dir and shell-exec's it.

## Run (runpod)

```bash
export RUNPOD_API_KEY=...
make demo PROVIDER=runpod
```

Cost: ~$0.02 per run (one small-class RunPod pod up for ~60 seconds at metered rates). The walkthrough always defer-terminates on success; Ctrl-C also terminates via signal handler. If something goes wrong, the pod URL prints to stderr and you can clean up manually via the RunPod console.

## Record a trace

```bash
go run . --tui --provider local --record /tmp/run.json
```

Then render as markdown for offline reading:

```bash
go run . --doc md --from /tmp/run.json > RUN.md
```

## What the demo exercises

Each step renders both as a sequence-diagram edge and as the literal shell command + stdout block.

| Step | What the operator types | What happens under the hood |
|---|---|---|
| Check the CLI is wired | `iplane instance list` | opens state file, prints `(no instances)` |
| Create with `--class small` | `iplane instance create <id> --provider … --class small` | Service expands constraints → resolver picks SKU → state file PENDING → provider Spawn → state file ACTIVE |
| Describe | `iplane instance describe <id>` | reads from state file, renders the full record |
| Idempotent re-create | same `iplane instance create …` | zero provider calls; state file already has ACTIVE record; output says "Found existing" |
| List (state-file source) | `iplane instance list` | all local records as a tabwriter summary |
| List `--remote` (runpod only) | `iplane instance list --remote --provider runpod` | queries the provider for instances tagged with `iplane-operator` |
| Destroy | `iplane instance destroy <id>` | state file TERMINATING → provider Terminate → state file TERMINATED |

## Why two examples?

Two transports for the same Service contract:

- **01-end-to-end**: gRPC client. Useful when `iplane serve` is running and the operator wants to drive provisioning programmatically (CI, scripts, other tools).
- **02-cli-end-to-end**: shell-exec'd CLI. The path the chapter reader follows in act-3; what an operator types from a terminal.

If you're embedding iplane as a library, read 01. If you're learning what `iplane instance` does from the inside, read 02. Both produce the same state-file deltas at every step.

## What's not covered (yet)

- **`--dry-run`** — lands on the same branch as the CLI. The walkthrough doesn't include a dry-run step; it's documented separately in `docs/cli-dry-run.md`.
- **`--service-url` transport** — the CLI can forward to a running `iplane serve` instead of opening the state file directly. Exercised by `cmd/iplane/cmd/instance_test.go` via httptest; not surfaced here to keep this walkthrough on the operator-typing path.
- **Phase 2's deploy primitive** — `docker run` on the provisioned instance. Lands in `examples/deploy/`.

## Manual cleanup (runpod only)

If the demo crashes badly enough that the deferred destroy doesn't fire:

```
https://www.runpod.io/console/pods
```

Pod name will be `iplane-cli-demo-<timestamp>`. Click Terminate.

Local instances need no cleanup — they exist only in iplane's state file. `rm -rf /tmp/iplane-cli-example/` resets cleanly.
