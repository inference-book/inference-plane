# Measuring an engine with `iplane load`

Two questions, two loop shapes, and using the wrong one produces a number
that looks fine and means nothing.

## Open loop: does this deployment survive a known arrival rate

`iplane load --rps 5 --duration 60s` offers requests at a fixed rate and
records what happens. The arrival rate is an input because in the real
world it is a fact: users arrive when they arrive.

Past saturation an open loop accumulates a queue, so **every latency
number becomes a statement about how long the run was**. A ten-minute run
reports worse latency than a two-minute one from the same engine at the
same speed. That is not a bug in the tool; it is what an overloaded
system does, and it is the answer to the question being asked.

Back-pressure shows up as `skipped (full)` rather than as a lower offered
rate, because the ticker drops a tick instead of blocking. The A/B
comparator in `examples/09-multi-gpu/09e-fabric-ab/` treats an arm whose
`actual_rps` falls below 80% of target as saturated and declares its
latency rows not a measurement of the thing under test.

## Closed loop: what is this engine's throughput curve

`iplane load --sweep 1,2,4,8,16` holds exactly N requests in flight,
replacing each as it completes, so the arrival rate becomes an **output**.
The queue cannot run away, and throughput against N is the curve the cost
model needs: cost per token is the rented hour over the tokens it
produced, and N is the only knob that moves the denominator.

### Steady state, not a warm-up sleep

A fixed sleep is wrong in both directions at once, too short at the levels
that take longest to fill the batch and pure waste at the levels that
settle in two seconds. The sweep watches the thing that has to stop
moving: it samples completion counts every `--sweep-window` and starts
measuring once `--sweep-stable-windows` of them sit within
`--sweep-tolerance` of their **running mean**.

Against the mean rather than against the previous window, because during
a gradual ramp consecutive windows are always close to each other and
never close to where the level started. A pairwise check calls a ramp
stable.

**The tolerance has a floor of one completion per window.** A level
running at five requests a second against a one-second window alternates
between four and five forever; that is a 20% swing that no amount of
waiting removes, and reporting it as instability is the tool being wrong
rather than the engine. At counts high enough to resolve the configured
tolerance, the configured one dominates.

A level that never settles inside `--sweep-warmup-max` is measured and
flagged rather than omitted. A gap in the ladder reads as an oversight.

Workers keep running across the warm-up boundary; only the recording is
switched on. Restarting would drain the engine's batch and hand the
measured window the exact ramp the warm-up just discarded. (This is why
`loadStats` tolerates a nil receiver.)

### Reading the columns

- **`batch`** is throughput times mean latency, Little's law against the
  offered level, so it should equal the label. When it sags, the row is
  describing a smaller batch than it claims. Two causes: workers erroring
  rather than holding a request, or, more often, requests still in flight
  when the window closed, which are disproportionately the slow ones. A
  level queueing at an admission gate shows exactly this. Lengthen
  `--sweep-duration` before trusting the numbers beside it.
- **`ttft` and `itl` are separate on purpose.** Time to first token is
  dominated by queueing and prefill; the gaps after it are decode. A
  growing batch often leaves the first roughly alone and stretches the
  second, and a single latency number blends them into something that
  looks like the engine generating more slowly. Both need `--stream`; a
  non-streamed reply arrives in one piece, so its first and last token
  land at the same instant.
- **`discarded` and `warmup`** say what was excluded, so two sweeps can
  be compared knowing what each is made of.

## Context length

`--prompt-tokens N` draws a prompt of roughly that length from an
embedded public-domain corpus (see `cmd/iplane/cmd/corpus/README.md`).

Prose rather than generated filler for two reasons that both bear on the
measurement. Filler tokenizes unrealistically, so the prompt costs a
different number of tokens than its length suggests. And filler makes
every request's prefix identical, so an engine with prefix caching
reports a hit rate describing the load generator rather than the
workload. The window rotates per request so concurrent requests overlap
the way real traffic does instead of being clones or strangers.

`charsPerToken` is 4, matching what the mock reports, so a mock run has
the requested and reported counts agree. Against a real engine expect
10–20% drift and read the engine's own `prompt_tokens` as the truth.

A prompt longer than the corpus tiles it. At 1M tokens that is the same
book about twenty-seven times, and a prefix-caching engine will notice;
say so in any figure drawn from a run at that length.

`iplane load session --system-prompt-tokens` covers the multi-turn growth
case and draws from the same corpus.

## Making the mock have a ceiling

The mock admits everything by default, so a sweep against it is a
straight line with no knee. `iplane mock-engine --kv-budget-tokens N`
adds token-denominated admission and `--token-latency D` paces streamed
frames so inter-token latency is measurable. Both default off, so every
routing demo is unaffected. See `internal/backends/NOTES.md` for what the
mock does and does not model.

A worked capture, `--kv-budget-tokens 40000`, sweeping 1/4/16/64:

| `--prompt-tokens` | shape of the curve |
|---|---|
| 500 | straight to 64 concurrent, 22k tok/s |
| 4000 | knee between 16 and 64; TTFT 31ms → 96ms |
| 16000 | ceiling below 16; at 64 throughput *falls*, TTFT 542ms, batch 39.6 of 64 |

Inter-token latency stays at 2.1–2.5ms throughout, which is the mock
being honest about modelling admission and not decode contention.

## The sweep as a data artifact

A Part IV figure costs thousands of dollars of rented hardware to
reproduce, so no number in one should reach a caption by being retyped
out of a terminal. `--output json` and `--output csv` write the sweep to
stdout while the human table goes to stderr, so a plain redirect captures
data and nothing else.

```
iplane load --sweep 1,2,4,8,16 --target glm --service-url http://localhost:8080 \
  --model zai-org/GLM-5.2 --prompt-tokens 8000 --stream \
  --output csv > figures/glm-8k.csv
```

CSV is one row per level with the columns derived from the same struct
tags the JSON uses, so the two formats always carry the same column names
and a figure can read either. The provenance sits in leading `#` comment
lines rather than in repeated columns, since it is constant down the file
and pgfplots wants a rectangle of numbers. `\pgfplotstableread` skips
those lines by default.

```
# schema_version 1
# captured_at 2026-08-18T17:14:03Z
# iplane_version v0.4.0
# model zai-org/GLM-5.2
# provider runpod
# gpu_sku B200
# gpu_count 4
# plan tp4
# prompt_tokens 8000
concurrency,achieved_batch,requests_per_sec,tokens_per_sec,...
1,0.96,9.99,772.3,...
```

`schema_version` is the contract. It gets bumped when a column changes
meaning and never when one is added, so a reader keyed by column name
keeps working as the sweep grows fields.

The hardware block comes from the control plane rather than from flags.
In `--target` mode the sweep reads the deployment and each instance
behind it, so `provider`, `gpu_sku`, `gpu_count` and `plan` describe what
was actually rented instead of what somebody typed. A deployment spanning
two providers reports both, joined, because collapsing to one would
describe a run that did not happen. Driving a bare `--url` leaves the
block empty, which reads as nobody having asked.

If that read fails, the sweep warns and keeps going. A run about to spend
an hour of rented time is not worth refusing over a metadata lookup, and
a file missing its hardware block can be annotated later in a way a run
that never happened cannot.

One thing to remember about `iplane_version`: it is the linker stamp, so
`make build` and `make dist` record a real version while `go run
./cmd/iplane` records `dev`. Use a built binary for anything whose numbers
will end up in a figure.

## Cost per token

A throughput curve is only half the economic story. The other half is
what those tokens cost, and it falls as concurrency rises because a
batched decode step reads the active weights once and splits that read
across every sequence in the batch.

Three series carry it. `instance.uptime.seconds.total` counts billed
seconds per rented instance, `instance.rate.usd_per_second` carries the
price the provider quoted when the instance was rented, and
`instance.cost.usd.total` is their product. All three are labeled
`instance_id`, and so is `inference.tokens.generated`, so cost per
million tokens is a division:

```promql
increase(instance_cost_usd_total[$w])
  / increase(inference_tokens_generated[$w]) * 1e6
```

Window it, and set `$w` to the sweep's steady-state window rather than
to the whole run. A cumulative ratio averages every concurrency level
together and the curve disappears, which is the one thing this
measurement exists to show. Sum the numerator and denominator across a
heterogeneous deployment's instances before dividing, since replicas on
two providers cost different amounts per token and the deployment's
figure is the blend.

Two things to know before reading a number off it. An instance whose
provider quoted no rate is absent from the cost series rather than
counted as free, so a fleet with one unpriced instance understates
spend rather than reporting a wrong total. And the product is computed
from the rate as quoted at rent time, which is exact today because no
rental is spot-priced or reclaimable (#333).

`inference.tokens.generated` carries a `context_bucket` label, an
upper-bound band on the prompt length read off the engine's usage block,
so several sweeps at different context lengths can be compared on one
panel. Within a single sweep the value is constant, which is why the
label splits runs rather than requests. A response reporting no prompt
count lands in `unknown` rather than in the shortest band, since folding
those together would let a non-reporting engine drag a cost curve toward
a context length nobody ran.

The **Inference Plane Cost & Concurrency** dashboard
(`deploy/grafana/provisioning/dashboards/inference-plane-cost.json`) is
this watched live: cost per million tokens split by context band, with
achieved concurrency beside it so the pair reads as the curve on a shared
time axis. Its cost panel divides *total* spend by each band's tokens,
which is exact while one context length is in flight at a time and
inflates every band when several run at once. For a figure rather than a
screenshot, use the committed artifact above, where concurrency is an
explicit column.

## The comparison nobody has run yet

`iplane model budget --sessions-at 8k,128k,1M` predicts a concurrency
ceiling from arithmetic; `--sweep` measures one. Pointing them at each
other is the interesting experiment, and against the mock it only tests
the mock's own budget against itself. It wants a real engine (#358).
