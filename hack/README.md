# hack/

Orchestration scripts. Behaviour is tested in Go (see `CLAUDE.md`); shell
here does setup, sampling and glue only, and nothing in this directory
asserts anything.

## capacity-sample.sh

Appends one JSONL row per GPU width per run, recording what every
configured provider says it would rent. Free and read-only: `iplane
capacity` asks and rents nothing.

```sh
make build
hack/capacity-sample.sh capacity-samples.jsonl
```

Each row carries the timestamp, the requested width, a per-provider count
and cheapest price, and the full candidate list. A width nobody can supply
is recorded as zero rather than skipped, because a gap in the series would
read as "nobody sampled" where the truth is "nobody had anything", and
telling those apart is the point.

**It needs the provider API keys in the environment.** `iplane` skips a
provider whose key is missing, and a skipped provider looks exactly like
one with no capacity, so a run of zeroes is worth checking before it is
believed.

Tunables, all via environment: `CAPACITY_SAMPLE_PROVIDERS`,
`CAPACITY_SAMPLE_WIDTHS`, `CAPACITY_SAMPLE_MIN_VRAM_GB`,
`CAPACITY_SAMPLE_TIMEOUT`, `CAPACITY_SAMPLE_STATE_DIR`, `IPLANE_BIN`.

### Why sample at all

Frontier capacity is perishable and one observation cannot show it. While
planning the GLM run, eight-card 80GB-plus supply on RunPod went from one
offer at $28.72/hr to nothing across twenty-three minutes, and a different
eight-card offer had been there twenty minutes before that. That is an
anecdote. A week of samples is a distribution, and the difference matters
because Part IV wants to say something in print about what renting at this
size is actually like.

The raw log is gitignored. A distilled artifact is what gets committed,
the same way `iplane load --sweep --output csv` carries provenance rather
than a terminal paste (#347).
