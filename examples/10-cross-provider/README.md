# 10 — Capacity Across Providers

Six acts on shopping several vendors for the same requirement, deciding
between them, and moving what you already placed.

Nothing here rents anything.

## What it shows

| Act | |
| --- | --- |
| 10a | One catalog, then several. The widened search and the footer that says which vendors answered |
| 10b | The unvouched row that vanishes under `--fabric`, and why it was dropped for staying silent rather than for lacking the hardware |
| 10c | Reclaimable capacity, and the discovery that one vendor of three actually sells it |
| 10d | The ranking becoming a decision, with its runners-up and the facts it could not weigh |
| 10e | A migration planned and priced before anything is spent |
| 10f | Why iplane refuses to drain an engine it did not rent |

## Prerequisites

- `make build` at the repo root, for `bin/iplane`.
- **At least one provider API key.** A free Vast.ai account is enough and no
  balance is needed, because acts 10a through 10d are read-only catalog
  queries.

```sh
export VAST_API_KEY=...
```

The script refuses to start without one rather than skipping acts. A run with
no vendor in it would print the shape of the argument with none of the
content, which is worse than not running.

Vast is the key worth having. It is the only one of the three that publishes a
measured interconnect reading or a reclaimable price, so with only RunPod and
Lambda, acts 10b and 10c run and show the absence rather than the effect.

## Running

```sh
make demo
# or
bash examples/10-cross-provider/run.sh
```

Env knobs: `DEMO_CLASS` (default `large`), `DEMO_GPUS` (default `2`),
`DEMO_LIMIT` (default `4`).

Two cards rather than one, throughout. A fabric describes how cards reach each
other, so the service refuses a fabric requirement below two, and asking for a
different shape in 10b than in 10a would make that act's side-by-side a
comparison of two different questions.

## What to look for

**10a, the footer rather than the table.** Three outcomes that an empty result
would have collapsed into one. `cannot answer` is a fact about a vendor's API.
`answered, no capacity` is a fact about the market and the only one of the
three that tells you anything about supply. The comparability lines name what
the ranking could not weigh, so a blank column reads as unmeasured rather than
as zero.

**10b, which rows leave.** On a live marketplace one of the disappearing rows
is usually cheaper than the cheapest survivor. Those hosts were not rejected
for lacking an interconnect. They were rejected for not saying, because the
alternative is renting a pool to find out and finding out costs the whole bill
either way.

**10c, the footer before the prices.** Of three vendors one sells reclaimable
capacity, one has no such tier anywhere in its API, and one exposes a bid price
that reads exactly like a spot rate and was equal to its on-demand rate on
every available shape when this was written. iplane drops that third case
rather than reporting a discount that does not exist. Shopping several vendors
turns out to be about which commercial arrangements exist at all, not only
about who has a card free this afternoon.

**10d, everything after the winning line.** A placement you cannot interrogate
is one you take on trust. Cheapest is not best, and the notes say which facts
the ranking could not compare, because a card can be cheapest by sitting where
your weights are not staged.

**10e, the warning.** Weights are staged per vendor and per region. The
difference between a warm move and a cold one is minutes, and it is knowable
for free before committing.

**10f, the refusal.** The engine is visible because it registered itself. It
will not be drained because iplane did not rent it, and releasing hardware you
did not acquire is not something a control plane should be willing to do.

## What is real here and what is not

Real: every price, every host, every interconnect reading, and every
per-provider outcome comes from a live vendor API at the moment you run it. The
numbers in this README's sibling chapter were captured this way.

Not real: nothing is rented, so nothing is measured under load. The engine in
10e and 10f is `iplane mock-engine` on localhost, attached through the
`external` provider, which is why 10f's refusal is the honest outcome rather
than a staged one.

Absent on purpose: a preemption. Nothing listens for a vendor signalling an
impending reclaim, and nothing infers one from a member that stopped answering.
The drain is the mechanism and the trigger does not exist. A demo that
destroyed its own instance on a timer would record a capability iplane does not
have, and a reader would discover the gap when a real vendor reclaimed
something and nothing happened.

## Capturing the book figures

The chapter's capacity listings are captures of 10a, 10b and 10c, and the
migration listing is a capture of 10e. Each carries a header recording the
trims made to fit the page, since a full-width table does not.

Re-capturing means running the acts and re-doing those trims by hand. Prices
and offer ids change between runs, so a captured listing is a snapshot of one
moment on one marketplace rather than a reproducible fixture, and the trims are
judgement calls about which columns carry the argument.

## See also

- `09-multi-gpu` for the control channel these fleet verbs read from
- `08-scaling-30b` for the cold-start cost that 10e's warning is about
- `docs/design/0004-cross-provider-warm-cache.md` for why the warm cache does
  not follow a move
