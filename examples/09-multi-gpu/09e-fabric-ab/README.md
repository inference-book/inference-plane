# 09e — The fabric A/B

Two arms, identical load, one verdict. This is the harness that replaces Ch 10's
softest claim with a measurement.

The chapter currently says tensor parallelism on the wrong fabric costs you
throughput, quotes a direction and a mechanism, and draws an illustrative
panel. Public apples-to-apples "NVLink vs PCIe-only, same model, same TP
degree" inference benchmarks are thin, so there is no figure to cite. This
produces one.

## Measured results

`results/` holds the raw summaries from the 2026-08-11 run that produced Ch 10's
throughput figure, with `results/RESULTS.md` recording the configuration, the
hosts, and the caveats. Committed because the number is quoted in the book and
a claim in print should have its evidence somewhere a reader can reach.

The headline: **157 tok/s on NVLink against 43 tok/s on PCIe, a 3.65x
difference**, same model and same TP=8 across 8 x A100 40 GB. Read `RESULTS.md`
before quoting anything from it -- half the table is not a fabric measurement,
and it says which half.

## The two modes

**GPU-free (default). Costs nothing.**

```bash
make build                                    # from the repo root
bash examples/09-multi-gpu/09e-fabric-ab/run.sh
```

Two `iplane mock-engine` processes with *different injected latencies* stand in
for the arms. This produces **no fabric finding at all** — mock engines have no
interconnect. What it does is check the rig against ground truth: a known delta
goes in, and the comparator has to recover it. Run this before spending
anything. A measurement rig you have not tested is how you publish noise.

**Paid.** Same load, same comparator, pointed at real hardware. Read
[Choosing hosts](#choosing-hosts-the-part-that-decides-whether-this-is-worth-anything)
first; it is the part that decides whether the run means anything.

```bash
DEMO_PAID=1 \
DEMO_A_SKU=A100_SXM4_40GB DEMO_B_SKU=A100_PCIE_40GB DEMO_GPUS=4 \
bash examples/09-multi-gpu/09e-fabric-ab/run.sh
```

Defaults that matter, and why they are what they are:

- **`--fabric intra-node` on arm A, `none` on arm B.** This is the experiment,
  not decoration. Naming a SKU is not enough: Vast lists bridge-capable cards
  under PCIe names, so a control arm chosen by name alone can silently contain
  NVLink, and the A/B then compares NVLink against NVLink and reports a small
  delta that looks like a finding.
- **`Qwen/Qwen2.5-32B-Instruct`** (override with `DEMO_MODEL`). ~64 GB of
  weights at FP16, so TP=4 across 4x40 GB puts ~16 GB per card with room for
  KV. It has to be big enough that the all-reduce traffic between cards is a
  real cost, or the A/B measures nothing on either fabric.
- **The 40 GB tier**, not 80 GB. That is where the healthy multi-GPU A100
  capacity is; the 80 GB NVLink arm was effectively empty when this was
  written. See #215 for the survey.
- **Three aligned timeouts.** `config.yaml` raises `write_timeout_sec` to 3600
  and the script exports a 40m engine-ready budget. The defaults do not cover a
  4-GPU cold start, and the failure mode is the server severing the response
  mid-provision while the pods keep billing.

## What it does

1. Brings up both arms **before measuring either**. Measuring arm A while arm B
   is still pulling its image compares a quiet host against a busy one.
2. Runs the load `DEMO_REPEAT` times per arm (default 3), **interleaved**
   A,B,A,B,… rather than all-of-A-then-all-of-B. Anything that drifts over the
   session — a noisy neighbour, a thermal ramp, network weather — would
   otherwise land entirely on whichever arm went second and be
   indistinguishable from a fabric effect.
3. Compares throughput, TTFT p50/p95, and latency p50/p95, and prints a verdict.

TTFT is measured, not just throughput, because prefill is where
tensor-parallel all-reduce traffic is heaviest. An interconnect effect can
appear in time-to-first-token while steady-state throughput barely moves, and a
throughput-only A/B would miss it.

## How it refuses to lie to you

The two ways to get this wrong are both quiet, so the comparator is built to
resist them.

**It will not establish a difference from a single run per arm.** Two numbers
are always different. With one run each, any range test trivially "succeeds",
so `--repeat 1` can never produce an established result no matter how large the
gap looks. A difference counts as established only when the arms' observed
ranges are disjoint across repeated runs.

**It checks the arms ran the same experiment.** Different target rps, duration,
or request count between arms produces a number that looks like a fabric result
and is not one. Any mismatch is a loud warning above the verdict.

**A null result is reported as a finding, not a failure.** If nothing is
established, the verdict says so and says why that is interesting: it would
mean the workload is not fabric-bound at this size and shape, which contradicts
the usual claim. That is the outcome most worth publishing, and a comparator
eager to declare a winner would bury it.

**It never claims fabric caused the delta.** The arms are different physical
hosts, so host differences are confounded with interconnect. That limitation is
printed alongside every established result and belongs in the write-up.

It also flags zero TTFT samples (the load generator was not streaming, so the
TTFT rows are meaningless) and any errors (a failing arm understates its own
latency, because failed requests contribute no slow samples).

## Choosing hosts, the part that decides whether this is worth anything

Surveyed on Vast, 2026-08-11. Numbers move, the traps do not.

**Do not use the price-ordered first offer.** On a marketplace the cheapest host
is cheap for a reason. `iplane` applies bandwidth and reliability floors by
default; they are necessary and **not sufficient**. Two hosts that passed every
floor still failed:

| failure | signature | what it cost |
| --- | --- | --- |
| broken IPv6 path to the registry CDN | `error pulling image configuration … connection reset by peer` | 9 min, never served |
| broken NVIDIA CDI config | `failed to inject CDI devices … /gpu=0: unknown` | could not start any GPU container |

Neither is visible in any pre-rent attribute. Both hosts advertised good
bandwidth and high reliability. **Pre-flight each arm with a 1-GPU slice and a
small model before renting the 4-GPU one.** That costs cents and has already
caught a host that would have burned a multi-GPU rental.

**Ask for the control arm explicitly, and know what the answer is worth.** A
host listed as `A100 PCIE` can report `bw_nvlink = 300` — a bridged card. If
one lands in the PCIe arm the A/B measures NVLink against NVLink and reports a
small delta, which is precisely the result the chapter would find most
interesting, and the contamination is invisible in the output.

Set `fabric_scope: NONE` on the control arm. That is a different request from
leaving it unset: unset means "do not care" and admits anything, `NONE` means
"must not have one", and the search pushes a `bw_nvlink <= 0` ceiling. Verified
live on 2026-08-11, it drops both bridged A100 PCIe hosts then on the
marketplace (machine 6566 at 300 GB/s, machine 140749 at 275).

**It excludes measured fabric; it cannot prove absence.** Vast reports 0 both
for "no link" and for "never measured" — the same probe that found the bridged
PCIe hosts also found roughly a quarter of SXM machines reporting zero on
boards that are physically always NVLinked. So a bridge-capable card with a
zero reading resolves to `FABRIC_SOURCE_UNKNOWN`, not to `NONE`, and the
deployment record says so rather than claiming a certainty the data does not
support. If the control arm silently *did* have NVLink, the A/B would report no
difference and the write-up would draw the wrong conclusion. Settling it for
real needs an on-box reading (`nvidia-smi nvlink -s`, issue #213). Until that
lands, state the residual uncertainty in the write-up.

**A failed host stays the cheapest offer.** Nothing records that it just
failed, so an immediate retry can rent the identical broken machine (issue
#214). Observed. Route around it by raising the bandwidth floor above the bad
host's advertised figure.

**Tier notes.** The 80 GB A100 NVLink arm was effectively empty at the time of
writing: of four offers with real `bw_nvlink`, one was broken and three had
reliability at or below 0.9749. The healthy inventory is on the 40 GB tier,
which `iplane` cannot currently request because both A100 SKUs are catalogued
at 80 GB (issue #257). Also: A100 is Ampere, so **FP8 is unavailable** and Ch
9's 72B FP8 recipe does not port, and the 80 GB PCIe slices are disk-poor
(147 / 91 / 75 GB), so a 70B FP16 will not fit. A 32B FP16 or a 70B AWQ int4
fits either tier.

## Knobs

| var | default | meaning |
| --- | --- | --- |
| `DEMO_REPEAT` | 3 | runs per arm; below 2 nothing can be established |
| `DEMO_DURATION` | 45s | load duration per run |
| `DEMO_RPS` | 4 | offered load |
| `DEMO_MAXTOK` | 64 | max output tokens per request |
| `DEMO_A_LATENCY` / `DEMO_B_LATENCY` | 15ms / 45ms | GPU-free only: the injected delta |
| `DEMO_PAID` | 0 | 1 provisions real GPUs |
| `DEMO_MODEL` / `DEMO_IMAGE` | — | paid only |
| `DEMO_A_SKU` / `DEMO_B_SKU` | — | paid only: fabric arm and control arm |
| `DEMO_GPUS` | 4 | paid only: GPUs per arm |
| `DEMO_DISK_GB` | 150 | paid only: container disk |

## Teardown, and the watchdog

GPU-free mode kills only what it started. Paid mode destroys both deployments
and both instances on exit, including on failure and on `SIGTERM`.

**That is not sufficient on its own.** A shell trap does not run when the
process is killed rather than exited, and it is deferred while a foreground
command is in flight. Anything that kills the run between renting a box and
tearing it down leaves that box billing.

So before a paid run, arm the independent watchdog:

```bash
./watchdog.sh 90 /tmp/wd.log &     # destroy every iplane-* instance after 90m
```

Two rules, both learned the hard way:

- **Arm it before anything can be rented**, not after the first deploy.
- **Stop it when the run ends.** A watchdog left armed from an earlier run
  swept a later run's boxes mid-measurement and destroyed a completed
  experiment. Tie its lifetime to the run.

It exits `0` only after positively confirming the account is clean. If it
cannot read the provider API at the deadline it exits non-zero and says so,
rather than reporting success it did not measure — an earlier version made
exactly that mistake and left a box running for 52 minutes while logging
"nothing to sweep; clean exit".
