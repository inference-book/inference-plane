# Examples

Runnable walkthroughs of iplane capabilities. Each example pairs a `main.go` (runnable via `go run`) with a `RUN.md` (rendered from a recorded trace; readable offline). Built on [demokit](https://github.com/panyam/demokit) so they branch on user choice and emit reproducible transcripts.

Numbered in roughly chronological order — earlier examples walk smaller capabilities, later ones build on them. New examples land as the matching capabilities ship; see [ROADMAP.md](../ROADMAP.md) for the version-by-version scope.

## What's here

- **[01-end-to-end/](01-end-to-end/)** — Full instance lifecycle (create → describe → idempotent re-create → list → destroy) via a demokit walkthrough against the programmatic `provisioners.Service`. Two providers wired: `local` (default, $0, no API key) and `runpod` (real pod, ~$0.02 per run, requires `RUNPOD_API_KEY`). Exercises the failure-mode contract (idempotency, state-file hygiene, list with self-heal) against either backend.
- **[02-cli-end-to-end/](02-cli-end-to-end/)** — Same instance lifecycle, but driven through the `iplane instance ...` CLI verbs against a running `iplane serve`. Shows the operator-facing surface; same providers, same cost shape as 01.
- **[03-deploy-end-to-end/](03-deploy-end-to-end/)** — Full deployment lifecycle on top of an instance: provision → interactive model-size choice (1.5B / 3B / 7B Qwen) → wire-telemetry (OTel endpoint discovery) → CreateDeployment with `Wait=true` → verify `/v1/models` → interactive chat REPL → observe (Grafana / hosted UI) → destroy. RunPod-only (local instances have no SSH endpoint so v0.1 cannot deploy to them). Cost depends on model size (~$0.05 for 1.5B up to ~$0.25 for 7B).
- **[04-router-in-path/](04-router-in-path/)** — v0.2 Beat 1 closer. Drives traffic *through* the in-process router (flat URL + deploy-id URL), fires `iplane load` to populate the v0.2 Grafana dashboard, points at the resulting Tempo trace. Detects-and-reuses an existing RUNNING deployment for the chosen model so demos 05 / 06 can attach to the same `iplane serve`. `--no-idle-destroy` pins the pod so the reaper won't evict it between demo runs. RunPod-only; same cost shape as 03. A zero-cost mock-engine variant is filed as #126.

## Running

Each example's README explains its prerequisites (API keys, env vars, expected cost). Most examples cost real money — typically pennies, but the README is explicit. Always set the env vars first; the demo refuses to start without them.

```bash
go run ./examples/<NN-name>/                       # interactive
go run ./examples/<NN-name>/ --record /tmp/r.json  # save the run
go run ./examples/<NN-name>/ --doc md --from /tmp/r.json  # render to markdown
```

The committed `RUN.md` in each example folder is the rendered output of one such recorded run. Readers who do not want to spend money can read it instead.
