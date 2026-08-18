# backends notes

Lore for `internal/backends`. Mostly about the mock, which is load-
bearing for every GPU-free demo and is easy to over-trust.

## The RNG is shared and must stay locked

`MockBackend.rng` is a `math/rand/v2.Rand`, which is **not safe for
concurrent use**, and the mock engine serves every request on its own
goroutine. Every latency and output-token sample raced from the day the
mock existed. It survived because the mock's own tests were sequential
and the binary is never run under the race detector; the first concurrent
in-process `Generate` found it in one run.

The three sampling helpers (`randFloat`, `randIntN`, `randInt64N`) exist
so every read goes through the lock. **Wrap the source, do not lock at
call sites** — a call site added later would otherwise be a race that
compiles and passes.

A shared source under a lock rather than one per goroutine, because the
seed is fixed on purpose: two runs of a demo sample the same latencies,
and per-goroutine sources would make that depend on scheduling.

## What the mock models, and what it does not

Models:

- A bimodal-with-tail latency mixture (`DefaultLatencyClusters`), tuned so
  the duration histogram on the dashboard is not a rectangle.
- Output token counts, clamped to the caller's `max_tokens`.
- Prompt tokens, at `len/4`. **Prompt length changes the reported count
  and nothing else** unless a KV budget is configured.
- Token-denominated admission, when `WithKVBudget` is set.

Does **not** model:

- **Decode slowing as the batch grows.** A real engine's inter-token
  latency rises with batch size; this one's stays flat. The mock
  therefore understates the cost of a large batch, and a sweep against it
  shows all of the queueing in time-to-first-token and none in the gaps
  after it.
- Prefill as distinct from decode. `Generate` sleeps once for the whole
  completion and the streaming layer then emits frames, so
  time-to-first-token is the configured latency regardless of prompt size.
- Prefix caching of any kind.

## The KV budget is weighted, and that is the whole point

`WithKVBudget(tokens)` caps the tokens held across all in-flight
sequences. A sequence occupies `prompt + permitted output` for the whole
generation.

- **Weighted, not counted.** A gate admitting a fixed number of requests
  caps concurrency at a number that does not move with context length,
  which is the behaviour it exists to produce.
- **The reservation is the permitted output, not the sampled one.** A
  real engine cannot know how long a reply runs when it schedules it.
- **Held for the whole generation.** Releasing before the sleep would
  model a pool that costs nothing to occupy.
- **A sequence larger than the pool is refused, not parked.** No amount
  of waiting frees space that does not exist; the error names both
  figures because the operator's next move is to change one of them.
- `sync.Cond` has no context-aware wait, so cancellation is broadcast
  explicitly. Without it a waiter ignores its deadline and the load
  driver's shutdown hangs behind the slowest occupant.

## Testing a gate without a flake

Do not use a timeout to detect "this would block". Under a loaded runner
a *successful* acquire can simply be slow to schedule, and the test reads
that as a rejection. Acquire the admissions that must succeed against an
uncancellable context, and let only the one that must block wait on a
clock. The first version of these tests failed roughly once in twenty and
only when the full suite and a `-race` run were competing.

## How this is driven

`iplane mock-engine --kv-budget-tokens N --token-latency D`. The
measurement side is in [../../docs/load-measurement.md](../../docs/load-measurement.md).
