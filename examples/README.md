# Examples

Runnable walkthroughs of iplane capabilities. Each example pairs a `main.go` (runnable via `go run`) with a `RUN.md` (rendered from a recorded trace; readable offline). Built on [demokit](https://github.com/panyam/demokit) so they branch on user choice and emit reproducible transcripts.

Organized by topic, not by chapter — these are the product surface, not chapter exercises. Number prefixes order examples within a topic.

## Topics

- **[provisioning/](provisioning/)** — Acquire and release GPU instances. Provider adapters (RunPod, Local), the failure-mode contract, idempotency, the state file.
- _deploy/_ (not yet) — Push the engine container onto a provisioned instance.
- _serving/_ (not yet) — Request queue, batching, OpenAI-compat surface.
- _scaling/_ (not yet) — Multi-instance fleets, ClusterManager.
- _routing/_ (not yet) — Backend router (workload-aware, cost-aware).
- _cost/_ (not yet) — Cost guardrail, cross-provider snapshot.

New topics land as the matching capabilities ship. See [ROADMAP.md](../ROADMAP.md) for the version-by-version scope.

## Running examples

Each example's README explains its prerequisites (API keys, env vars, expected cost). Most provisioning examples cost real money — typically pennies, but the README is explicit. Always set the env vars first; the demo refuses to start without them.

```bash
go run ./examples/<topic>/<NN-name>/                       # interactive
go run ./examples/<topic>/<NN-name>/ --record /tmp/r.json  # save the run
go run ./examples/<topic>/<NN-name>/ --doc md --from /tmp/r.json  # render to markdown
```

The committed `RUN.md` in each example folder is the rendered output of one such recorded run. Readers who do not want to spend money can read it instead.
