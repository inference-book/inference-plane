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

## Sizing a sweep so its numbers survive review

The first GLM-5.2 sweep deployed correctly, served correctly, and produced
nothing publishable. Every fault was in how the measurement was asked for, and
all of them are flags rather than hardware:

| what was set | what it meant |
| --- | --- |
| `--sweep-window 3s` while a request took 22s | a "settled window" held 0.13 of one request, so the steady-state detector was reading noise |
| `--sweep-duration 45s` | 5 to 28 requests per level, making p95 the second-highest of 28 observations |
| `--sweep-warmup-max 90s` | expired at one level, which was then measured anyway and reported a third of the throughput of a lower concurrency |
| streaming off | `ttft_samples` and `itl_samples` both 0 |
| `--max-tokens 256` against an 8k prompt | a 40:1 prefill:decode ratio, so the run measured prefill |

Three rules come out of it.

**A window has to be longer than one request.** Steady state cannot be
detected on a timescale shorter than the thing being measured. Set
`--sweep-window` from the latency you expect, not from the default.

**Percentile grade follows sample count, and long context cannot afford a
p95.** At 120k a request can take 45 seconds, so even ten minutes at four-way
concurrency buys tens of requests. Those rows support a median and not a tail,
and saying so is better than printing a number that will not reproduce.
`hack/measure-run.sh` grades every row (`p95` at 100+ successes, `p50` at 30+,
`UNUSABLE` below that or when a level never settled) so a thin measurement
announces itself.

**Stream, or the two most useful latencies do not exist.** TTFT and ITL are
what separate prefill cost from decode cost, and that separation is the whole
amortisation argument. They also carry the statistical power: end-to-end
percentiles stay thin at low concurrency, while every request contributes one
TTFT and hundreds of ITLs. Measured against `iplane mock-engine --latency
200ms --token-latency 8ms`, a streamed sweep reports TTFT p50 201ms and ITL
p50 9.97ms from 20,572 inter-token samples at four-way concurrency, against
zero samples for both with streaming off.

## What a running sweep tells you

A sweep used to print its header and then nothing until every level had
finished. On the GLM-5.2 run that was 48 identical minutes while an 8x H200
billed at $32.88/hr, and the same run fired its first sweep at a closed port,
produced no traffic at all, and looked exactly like a level still warming up
for 13 minutes (#438).

It now narrates to stderr, one line per window and one per finished level:

```
iplane load --sweep: levels [1 2], 6s per level after steady state -> http://…
  n=1 warming up: 7 ok
  n=1 warming up: 14 ok
  n=1 warming up: 21 ok (settled)
  n=1 measuring: 2s of 6s, 6 ok
level 1/2 (n=1): settled after 6s, 21 requests, 3.50 req/s, 84.0 tok/s, p50 277ms
```

**Successes and errors, not the request count.** That distinction is the
whole point and it is not obvious. A refused connection returns instantly, so
a sweep pointed at a closed port completes tens of thousands of attempts per
window at a beautifully stable rate, and the steady-state detector settles on
it. Narrating the attempt count would report `23287 requests in the last 2s`,
which reads healthier than a working engine. The same failure now reads:

```
  n=1 warming up: 0 ok, 23287 errors
```

Errors are named only when there are some, so a healthy run stays quiet.

**stdout is still exactly the artifact.** All of this goes to stderr, so
`--output csv > sweep.csv` captures data and nothing else. `hack/measure-run.sh`
tees it, so the lines reach both the run log and the operator watching.


## Two regimes, and which one you are in

Measured on GLM-5.2, 8x H200, tp8, 512 output tokens, the same harness at two
context lengths:

| | 8k | 120k |
| --- | --- | --- |
| tok/s at B=1 | 76.4 | 17.9 |
| best $/M output | **28.13** (at B=32) | **491** (at B=1) |
| direction with concurrency | falls 4.25x | **rises** |
| TTFT share of latency at B=1 | 13% | 73% |
| ITL p50 at B=1 | 10.75ms | 10.93ms |
| ITL p50 at B=8 | 15.01ms | 15.88ms |

At 8k the run is decode-dominated and batching works: a batched decode step
reads the active weights once regardless of how many sequences share it, so
cost per token falls as `94.43/B + 25.18` toward a floor set by per-token
FLOPs.

At 120k the run is prefill-dominated and batching inverts. Prefill has
nothing to amortise, because every one of 120,000 prompt tokens is processed
for every request, so added concurrency adds competing work rather than
sharing a fixed cost.

**The inter-token gap is the control that turns this from a guess into an
argument.** It barely moves between the two contexts at matched concurrency.
Decode speed is unchanged; the whole difference is prefill. Without that
column the throughput collapse at long context could equally be memory
pressure or scheduling.

**The practical consequence is that context length is a bigger cost lever
than concurrency.** Batching at 8k buys 4.25x. Moving from 8k to 120k costs
17.5x. A reader tuning batch size is optimising the smaller variable.

The regime flips somewhere between the two, and nothing has measured where.

## Reading a row that cannot be trusted

Three columns exist because a measurement that is quietly wrong has cost more
here than one that fails loudly.

**`truncated_requests`** counts responses the measurement window closed on
before the engine finished them. Neither a success nor an error: nothing
failed, and there is no completed request to measure. An HTTP client returns
the response as soon as the streaming headers arrive, so a cut-off request
looks like a 200 and was previously counted as a success carrying a latency
the engine never took and a token count it never produced. A large value
beside a small `successes` says the level was too short for its context
length rather than that the engine was slow.

**The self-consistency note** fires when a level's two independent token
counts disagree. Tokens come from the engine's `usage` block; inter-token
gaps come from counting frames as they arrive. On a level that measured what
it claims these agree within a few percent; on the truncated 120k level they
differed by 17x, and every published figure came from the smaller one. The
check takes no view on what a correct value looks like, only that two
measurements of one quantity should agree, which is what makes it work
against defects nobody anticipated.

**The grader** (`hack/measure-run.sh`) grades each row p95 / p50 / UNUSABLE
on its sample count, and grades the TTFT columns on `ttft_samples` rather
than on `successes`, because the two can describe different populations.

Note that `ttft_samples` may now exceed `successes`. A truncated request
contributes a real time-to-first-token, since the first token genuinely
arrived, while contributing no completed request.
