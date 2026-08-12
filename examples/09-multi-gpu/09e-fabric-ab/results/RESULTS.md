# Measured NVLink vs PCIe, 2026-08-11

The raw `iplane load --output json` summaries behind Ch 10's throughput figure,
committed because the number is quoted in the book and a claim in print should
have its evidence somewhere a reader can reach.

Two runs, because one operating point could not answer both questions. Neither
can be reproduced exactly: the hosts below are marketplace rentals and are
already gone.

## Configuration, identical across both arms

| | |
| --- | --- |
| model | `Qwen/Qwen2.5-72B-Instruct`, FP16, ~145 GB of weights |
| parallelism | tensor-parallel, TP=8 |
| engine | `vllm/vllm-openai:v0.7.0`, `--max-model-len 4096` |
| hardware | 8 x A100 40 GB per arm (328 GB total), one physical host each |
| provider | Vast |
| request mix | all chat completions, streaming, `--max-tokens 64` |
| repeats | 3 per arm, **interleaved** A,B,A,B so session drift cannot land on one arm |

The model does not fit on fewer cards: 145 GB against 41 GB each, so eight-way
sharding is a necessity rather than a tuning choice, and the all-reduce traffic
is correspondingly heavy. That is what makes this a test of the interconnect
rather than of a configuration.

## Arms

| | arm A `nvlink` | arm B `pcie` |
| --- | --- | --- |
| machine | 117301 | 42605 |
| requested | `--sku A100_SXM4_40GB --fabric intra-node` | `--sku A100_PCIE_40GB --fabric none` |
| `bw_nvlink` | 300 GB/s | 0 |
| resolved fabric | `FABRIC_SCOPE_INTRA_NODE` / **`FABRIC_SOURCE_MEASURED`** / 2400 Gbps | see caveat 3 |
| price | $6.88/hr | $5.44/hr |

Arm A's interconnect was **measured by the provider**, not inferred from the
card's name. That distinction is the point of `FabricSource`.

## `saturated-4rps/` — the throughput result

Offered 4.0 rps for 45 s. **This is the run the chapter's figure comes from.**

| | nvlink | pcie |
| --- | --- | --- |
| throughput | **157 tok/s** [148.7-163.7] | **43 tok/s** [42.6-47.2] |
| achieved rate | 3.93-3.96 of 4.0 | **1.18-1.31 of 4.0** |
| errors | 0 | 0 |

**3.65x.** Both arms were offered identical load and neither errored, so this is
a fair comparison of capacity.

**Do not quote the TTFT from this run.** The PCIe arm never reached the offered
load, so its latency is largely queueing delay, which grows without bound in
offered load and is a property of the load chosen rather than of the hardware.
Throughput survives saturation; latency does not.

## `unsaturated-0.6rps/` — the latency result

Offered 0.6 rps for 90 s, below the PCIe arm's measured ceiling, so neither arm
queues. Both achieved 0.59.

| | nvlink | pcie |
| --- | --- | --- |
| ttft p50 | 1190 ms [1179-1221] | 2254 ms [1334-3175] |
| ttft p95 | 1504 ms [1503-1510] | 3820 ms [3502-4138] |
| throughput | 23.1 tok/s | 23.2 tok/s |

**Throughput being equal here is expected, not a null result.** At unsaturated
load, tokens per second is set by the offered rate rather than by capacity, so
that row carries no information. Each regime measures exactly one of the two
things, which is why both runs exist.

**Treat the latency gap as suggestive, not established.** `pcie-3` is missing:
the run was cut short, leaving n=2 on that arm with values of 1334 and 3175 ms,
a 2.4x spread. The comparator reports it as established because the ranges do
not overlap, which is a rule passing on a technicality it was not designed for
(issue #273).

## Caveats that belong with any quotation of these numbers

1. **Different physical hosts.** The comparison controls for model, parallelism
   degree, card model, card count, and offered load. It does not control for the
   host, so host differences are confounded with interconnect.
2. **One host per arm.** No within-arm host variation was sampled.
3. **The control arm cannot be proven unbridged.** `--fabric none` pushes a
   `bw_nvlink <= 0` filter, which excludes every host with a *positive* reading.
   It cannot prove absence, because the provider reports 0 both for "no link"
   and "never measured". Arm B therefore resolves to `FABRIC_SOURCE_UNKNOWN`
   rather than `NONE`. Settling this needs an on-box reading; the sensor exists
   (`internal/engineagent/nvlink.go`) but agent delivery on Vast landed after
   this run.
4. **Marketplace inventory moves.** Both machines were gone within hours.

## Reproducing

`make demo` in the parent directory reproduces the *method* for free: two mock
engines with an injected latency delta that the comparator must recover. It
reproduces nothing about fabric.

`make demo-paid` reproduces the *experiment*, not these numbers, and costs
roughly $20 at 2026-08 prices. Read the parent README on host selection first;
two of the hosts touched during this work were broken in ways no pre-rent
attribute predicted.
