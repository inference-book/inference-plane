# 01-end-to-end

Walks through the full v0.1 provisioner lifecycle (create → describe → idempotent re-create → list → destroy) via a [demokit](https://github.com/panyam/demokit) demo as one client of a real connect-rpc server. Two providers wired:

- **`local`** (default): provisions against your laptop. Zero cost, no API key required.
- **`runpod`**: provisions a real RunPod pod. ~$0.02 per run. Requires `RUNPOD_API_KEY` in the **server's** environment.

The example is **two processes** that mirror what `iplane serve` + the typed client will look like in production:

- `make serve` boots the iplane provisioner service on `:9091`, mounting the generated connect-rpc handler for `ProvisionerService`. Any client speaking connect, gRPC, or gRPC-Web can talk to it.
- `make demo` runs the demokit walkthrough as **one** such client. Point your own tooling at `:9091` to exercise the same surface independently.

## Run (local, the default)

```bash
# Terminal 1
make serve

# Terminal 2
make demo
```

End-to-end in seconds, no money spent, no env vars needed. Records actor sequence diagrams and step transcripts (TUI-styled when `make demo` is used; `make demo` passes `--tui`).

## Run (runpod)

```bash
# Terminal 1
export RUNPOD_API_KEY=...
make serve

# Terminal 2
go run . --tui --provider runpod
```

Cost: ~$0.02 per run (one small-class RunPod pod up for ~60 seconds at metered rates). The demo always defer-terminates; Ctrl-C terminates via signal handler. If something goes wrong, the pod URL is printed and you can clean up manually via the RunPod console.

## Record a trace

```bash
go run . --tui --record /tmp/run.json
```

Then render the walkthrough as markdown for offline reading:

```bash
go run . --doc md --from /tmp/run.json > RUN.md
```

The committed `RUN.md` in this directory is the output of one such recorded run (local provider — reproducible at zero cost).

## What the demo exercises

| Step | What happens |
|---|---|
| Check the service is reachable | `ListInstances` (local source) — fails fast if `make serve` not running |
| Create with `class=small` shorthand | Service expands constraints → provider resolver picks SKU → state-file `pending` → provider call → state-file `active` |
| Describe (local view) | `DescribeInstance{source=local}` — reads the state file |
| Idempotent re-create | Same spec.id; **zero provider calls**, returns existing record |
| List local | All local records |
| List remote (runpod only) | Queries the provider directly; confirms iplane-id tag is server-visible |
| Destroy | State-file patch to terminating → provider call → patch to terminated |

Each step's `Arrow` calls render as sequence-diagram edges between the four actors (Operator, iplane, State, Provider) in the demokit transcript.

## What's not covered (yet)

- **Phase 2's deploy primitive** — `docker run` on the provisioned instance. Lands in `examples/deploy/`.
- **`--dry-run` mode** — phase 1.5; the demo would gain a "what would happen" branch.
- **Provider failure paths** — quota exceeded, no capacity in region, cleanup failure. Useful additions; would extend the demokit definition with branching choices.
- **Multi-provider routing** — when Lambda Labs lands (planned for v0.2, possibly promoted to v0.1 if RunPod proves unreliable for chapter readers), this example will demonstrate spawning the same workload across providers via a `--provider` switch (already wired) and a future ClusterManager (v0.3).

## Manual cleanup (runpod only)

If the demo crashes badly enough that cleanup doesn't fire, find the pod here:

```
https://www.runpod.io/console/pods
```

The pod name will be `iplane-demo-<timestamp>`. Click Terminate.

Local instances need no cleanup — they exist only in iplane's state file; rerunning the demo overwrites them.
