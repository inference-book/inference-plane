# Demo 07 — Stateful Routing and Prefix-Cache Reuse (Ch 8)

A multi-turn **chat session** keeps coming back, turn after turn, with a
prefix that grows every turn. Under the Ch 7 round-robin router each turn
lands on a different replica and re-prefills the whole conversation from
cold. Switch the router to **prefix-cache affinity** and a conversation's
turns stick to the replica that already holds its prefix.

`run.sh` shows this GPU-free in three acts, on the mock engine:

| Act | Router | What you see |
| --- | ------ | ------------ |
| 07a | `round_robin` | sessions scatter across all engines (cache-defeating) |
| 07b | `prefix_affinity` | each session pins to one engine (scatter 0) |
| 07c | `prefix_affinity` + overload cap | replicas that hit the cap shed load — affinity yields to load under saturation |

It measures **routing** — which engine each session lands on. The
engine-side prefix-cache hit-rate and the latency win are a real
multi-replica run (see "Capturing the book figures" below).

## Run it (GPU-free)

```bash
make build                              # once, from the repo root
bash examples/07-prefix-affinity/run.sh # or: cd examples/07-prefix-affinity && make demo
```

No GPU, no provider keys, no cloud. `run.sh` starts three `iplane
mock-engine` processes, and for each act starts `iplane serve` with a
different routing policy, registers the engines as one deployment via the
**external** provider (no provisioning), fires the session driver, and
prints the per-engine session distribution plus a **scatter score** (how
many sessions touched more than one engine).

Env knobs: `DEMO_SESSIONS` (default 9), `DEMO_TURNS` (default 4).

### What a run looks like

```
== 07a  round_robin ...
  scatter: 9 of 9 sessions touched more than one engine
== 07b  prefix_affinity ...
    engine 9001: s-0006 s-0007 s-0008
    engine 9002: s-0000 s-0002 s-0005
    engine 9003: s-0001 s-0003 s-0004
  scatter: 0 of 9 sessions touched more than one engine
== 07c  prefix_affinity + overload cap 2 ...
  scatter: 8 of 9 sessions touched more than one engine
```

Round-robin scatters every session; affinity pins each to exactly one
engine (scatter 0); the overload cap makes saturated replicas shed load,
so scatter climbs back — the "affinity is not free" trade-off.

## The moving parts (real commands)

```bash
# a mock engine (dev/CI; --latency keeps the demo fast)
iplane mock-engine --port 9001 --latency 3ms

# register those engines as one deployment, no provisioning
iplane deployment deploy demo --provider external \
    --engine-endpoints http://127.0.0.1:9001,http://127.0.0.1:9002,http://127.0.0.1:9003 \
    --model mock/mock

# the router policy is set on `iplane serve` (config or env):
#   router.routing_policy: round_robin | prefix_affinity
#   router.affinity_overload_threshold: 0 (off) | N (spill when a pin has >= N in-flight)

# the load: closed-loop multi-turn sessions, each stamping X-IPlane-Session
iplane load session --target demo --model mock/mock --sessions 9 --turns 4
```

Header-less clients (a plain OpenAI SDK on the flat `/v1/chat/completions`)
also get affinity: the router derives the session key from the request's
opening. `X-IPlane-Session` still wins when present.

## Try it yourself

- **Scale the mock fleet to 5 (`PORTS` in `run.sh`) and re-run 07a** — round-robin's scatter stays high; more replicas means *worse* cache locality, not better.
- **Bump `router.affinity_overload_threshold` up** until 07c stops spilling — that value is roughly your per-replica concurrency ceiling before affinity yields to load.
- **Point a plain OpenAI SDK at the flat URL twice (no `X-IPlane-Session`)** under `prefix_affinity` and watch both turns land on the same engine — the derived key at work.

## Capturing the book figures (real cluster)

The mock walkthrough shows routing. The book's figures — prefix-cache
hit-rate and time-to-first-token diverging between round-robin and
affinity — need real engines with a real KV cache. This is the one paid
step; run it when you're ready:

1. Provision **3 replicas of a small model on cheap GPUs** (an 8B on
   ~16–24 GB cards, e.g. `iplane deployment deploy ... --replicas 3`).
   The effect is driven by *conversation length*, not model size, so keep
   the model small — long multi-turn sessions do the work.
2. Bring up the obs stack (`make infra-up`) so the v0.2 Grafana dashboard
   (`uid=inference-plane-v02`) populates.
3. Fire `iplane load session` under each policy (restart `serve` with
   `routing_policy: round_robin`, then `prefix_affinity`).
4. **Screenshot these panels** for the chapter:
   - **Session affinity hit-rate** — ~1/N under round-robin, ~1.0 under affinity.
   - **Request latency (p50/p95)** — TTFT flat under affinity, climbing with conversation length under round-robin.
   - **Per-replica in-flight** — even under round-robin, session-pinned clusters under affinity; bounded under the overload cap.
5. Tear down. Rough cost: a few dollars for a few minutes of 3 cheap GPUs.

The affinity hit-rate the mock run shows is a routing-locality proxy; the
real engine's `gpu_prefix_cache_hit_rate` (scraping is a follow-up, issue
51) is the ground truth, and their divergence under KV eviction is the
memory-pressure story.

## Deferred / follow-ups

- **Engine `gpu_prefix_cache_hit_rate` scraping** (real cache-hit ground truth) → issue 51.
- **KV eviction under memory pressure** — real-engine only; the mock has no cache to evict.
- **Front-pruning invalidates the prefix** and **asymmetric whale load** — both need a driver feature (context pruning / per-session load weighting) that doesn't exist yet → tracked as a follow-up.
