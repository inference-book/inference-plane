# 09 — Tracking a Distributed Data Plane

Ch 10's closer. A four-card group and a single card in one fleet, watched
through the control channel as the group forms, loses a link, and finally
stops answering.

**GPU-free. Costs nothing.** Everything runs on `iplane mock-engine
--register`, so there is no provider key, no cloud, and no bill.

## What it shows

Four acts, one control plane, two engines:

| Act | State | The point |
| --- | --- | --- |
| 09a | `assembling` | A group exists before it serves, and no outside probe can see that |
| 09b | `serving` / `serving` | `4c/1n` and `1c/1n` in one list, differing only by a column |
| 09c | `serving, link down` | Correct tokens and a dead link, both true at once |
| 09d | `lost` | Silence past a deadline, not a failed health check |

## Prerequisites

```bash
make build          # from the repo root; run.sh needs bin/iplane
```

Nothing else. No `RUNPOD_API_KEY`, no running stack, no Docker.

## Running

```bash
bash examples/09-multi-gpu/run.sh
# or
cd examples/09-multi-gpu && make demo
```

Takes about 90 seconds, most of it act 09d waiting out a lease. Knobs:

```bash
DEMO_ASSEMBLE=3s DEMO_DEGRADE=10s bash run.sh    # move faster
```

The lease (30s) and the sweep interval (10s) are fixed in the control
plane and are why 09d is the slow act. That wait is the demo, not an
inefficiency: the delay is the number the control plane handed out.

## What to look for

**09a — the assembling window.**

```
MEMBER       MODEL      SPAN   STATE       AGE
single-card  mock/mock  1c/1n  serving     0s
tp4-group    mock/mock  4c/1n  assembling  0s
```

`tp4-group`'s processes are alive and its group has not formed. A health
poller cannot report this, and not only because "assembling" is outside
its vocabulary: during assembly **there is no endpoint to connect to**.
An observer outside the machine cannot tell a group that is still forming
from one that is slow, broken, or gone. The agent is inside, so it can.

**09b — the span column.**

```
single-card  mock/mock  1c/1n  serving  10s
tp4-group    mock/mock  4c/1n  serving  10s
```

Both rows are the same kind of thing: one endpoint, one model. A
distributed engine did not become a new object with its own verbs, and
there is no `iplane pool` or `iplane group`. The span is a column.

**09c — degraded, not dead.**

```
single-card  mock/mock  1c/1n  serving             30s
tp4-group    mock/mock  4c/1n  serving, link down  30s

  ...and it is still answering:
    "completion_tokens":122
```

Read those together. The engine returns correct tokens **and** its link is
down. `/health` would say "serving", which is true and useless, because a
degraded group is serving, at a fraction of the hardware you are paying
for. This state has no single-card equivalent, which is why the vocabulary
had to grow instead of being reused.

**09d — the lease expires.**

```
  killing tp4-group's process (no goodbye, the way a real crash goes)
  the member keeps reading as serving until its lease runs out...
tp4-group    mock/mock  4c/1n  serving, link down  35s

  ...waiting out the lease (30s) plus a sweep (10s):
tp4-group    mock/mock  4c/1n  lost                1m
```

The member does not vanish when killed. It keeps reading as it last
reported until its deadline passes. The fleet view is not a probe of what
is alive now, it is a record of what last announced itself plus a
deadline, and reading it that way tells you exactly what bounds the delay.

The default view hides lost members; `--show-lost` reveals them, because a
deleted row and one that never registered look identical.

## What is real here and what is not

Worth being precise, because the demo is deliberately cheap:

- **Real:** every state transition. The same agent code that runs on a
  rented box, through the same registration path, producing the same
  states. Kill the process and the lease expires exactly as it would in
  production.
- **Fabricated:** the span. `mock-engine --span-cards 4` invents four
  cards on one node so the column has something to render. No four-card
  group was rented.
- **Simulated:** the link failure. `--degrade-after` watches a clock where
  a real sensor reads NVLink state and error counters off the cards
  (issue 213). The agent seam is the same one the real sensor plugs into;
  only the source of the reading differs.

## Capturing the book figures

The `fleet status` renders above are the chapter's fleet-view figure and
come from this demo at no cost.

The **NVLink-vs-PCIe throughput A/B** does not, and cannot: it needs two
real multi-GPU pools. Its shape is fixed and deliberately narrow (issue
215):

- Same GPU model in both arms — 4x A100 80GB **SXM** against 4x A100 80GB
  **PCIe**. Matching only the VRAM sum would confound card generation with
  fabric, and the result would not be worth the spend.
- Weights pinned first with `iplane model pin` (Ch 9), so neither arm pays
  a cold start and the measurement is of the fabric rather than of the
  download.
- Same model, same TP degree, same request mix. Capture **TTFT as well as
  throughput**.

Treat it as a falsification test rather than an illustration. The
chapter's current panel is a synthetic stand-in guessing roughly 1.6x. If
the measured delta comes back close to that or smaller, the argument for
selecting on fabric needs to rest on something other than throughput, and
that is the more valuable outcome because it changes the book. Do not tune
toward a target.

## See also

- `examples/07-prefix-affinity/` — the other GPU-free walkthrough, same shape
- `examples/08-scaling-30b/08a-cold-start-distance/` — where `iplane model pin` comes from
- `docs/design/0006-ch10-provider-reality-and-control-channel.md` — why the channel is a lease
- `docs/design/0007-gpu-validation-findings.md` — what one rented box settled
