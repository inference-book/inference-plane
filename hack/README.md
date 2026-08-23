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

## vast-watchdog.sh

Destroys Vast instances whose creator has died or which have outlived a
deadline. Runs as its own process, because that is the whole point: a
teardown that lives inside the run it protects dies with it.

```sh
date +%s > run.hb
hack/vast-watchdog.sh --heartbeat run.hb --max-stale 300 --max-lifetime 5400 &
# the run touches run.hb on a cadence, and writes DONE into it on clean exit
```

It destroys an instance for one of two reasons: the heartbeat file has
stopped being touched (the creator is gone), or the instance is older than
`--max-lifetime` (the creator is alive and wrong). Flags: `--heartbeat`,
`--registry`, `--label-prefix` (default `iplane-`), `--max-stale`,
`--max-lifetime`, `--interval`, `--max-runtime`, `--dry-run`.

**Ownership is positive-only.** A candidate is an instance whose label
carries the prefix, which is what `internal/provisioners/vast` stamps
(`iplane-<deployment-id>`), or whose id was appended to `--registry`. The
registry is there for scripts that call the Vast API directly and so never
get a label stamped for them. Anything unmatched is left running: the
account also holds boxes this project did not create, and destroying one
of those is worse than leaking one of ours.

**Every uncertainty resolves to "leave it running".** An API read that
fails, a response that will not parse, an instance with no `start_date`,
a heartbeat that has not appeared yet. The watchdog arms on first sight of
the heartbeat file rather than at startup, because before that "not
started yet" and "died" are the same observation and only one of them is
worth a teardown.

### Why this exists

A billing probe was launched as `./run.sh &` from inside an already
backgrounded call. The wrapper returned immediately, the harness recorded
the task as complete, and the script was killed as an orphan. A bash EXIT
trap does not run on that kill, so the teardown never fired and the box
billed until somebody happened to poll `/api/v1/instances/`.

Two things follow, and the second is the one that generalises. Never pair
`&` with an already-backgrounded invocation. And never let the only
teardown live in the process that can be killed.

Note also that the credit balance is a lagging indicator: charges kept
draining for several minutes after both test instances were destroyed and
`charges` read 0 throughout. Watching the balance would not have caught
this. Polling `/api/v1/instances/` did. Use `/api/v1/`, never `/api/v0/instances/`,
which is deprecated and answers with an error object that parses as an
empty list.

## hf-throughput-probe.sh

Measures how fast one rented box actually pulls weights from Hugging Face,
and appends the reading to a JSONL log.

```sh
make build
hack/hf-throughput-probe.sh --instance probe-box --model cyankiwi/GLM-5.2-AWQ-INT4
```

It picks the largest file under `--max-file-gb` (default 6), times a single
`hf_hub_download` of it on the box, and reports MB/s plus what the whole
repo would take at that rate. Flags: `--instance`, `--model`, `--file`,
`--out`, `--max-file-gb`, `--timeout`.

**The probed file is not wasted.** It lands in the same HF cache layout the
engine reads, so the shard counts toward the real download and vLLM's
`snapshot_download` skips it. On a host you keep, the only cost is the
seconds spent knowing.

**It times `hf_hub_download`, not curl**, and refuses rather than falling
back if `huggingface_hub` will not import. The GLM checkpoint is Xet-backed
(every shard redirects to `xet-bridge-us` carrying an `x-xet-hash`), so
plain HTTPS and the Xet chunked path are different code paths, and a curl
number would not predict what the engine sees.

**It neither rents nor destroys**, and the box bills for as long as it
takes. Arm `vast-watchdog.sh` first.

`iplane instance ssh` honours `IPLANE_SERVICE_URL`; unset it (or pass
`--service-url ""` to the CLI directly) when driving a local state dir
rather than a running daemon, or the probe dials `localhost:8080` and
reports a connection refused instead of a reading.

**The remote half has not been run on hardware.** The hub side is exercised
against the real 474.3 GB GLM repo; the on-box fetch is not. The first real
invocation is the test.

### Why measure at all

A host's advertised link speed does not predict what it achieves against
Hugging Face. The GLM-5.2 run landed on a Vast host advertising 5,813 Mbps
(726 MB/s) that delivered about 134 MB/s, turning an 11-minute download
into a 59-minute one that hit the engine-ready timeout and cost $22. An
earlier run on a different host had done the same fetch several times
faster. Three runs is three anecdotes; the point of a log is a
distribution, and the reading arrives before the meter has run two minutes.

## deploy-watch.sh

Watches what a deploy is actually doing, over SSH, while it is doing it.

```sh
make build
hack/deploy-watch.sh --deployment glm52-run --model cyankiwi/GLM-5.2-AWQ-INT4
```

Every 30s it asks each replica for the bytes in its model cache, free disk,
GPU memory in use, and whether an engine process is alive, then derives a
rate, a percentage and an ETA against the model's download size. One JSONL
row per replica per tick.

**Free.** The instance is rented regardless and each reading is a shell
command on a machine already being paid for.

**A replica with no SSH endpoint is called out once, loudly**, and its rows
carry `"reason": "no-ssh-endpoint"`. That state never resolves, so reporting
it identically to a box mid-boot buries the one difference that matters.

**Unreachable is recorded, not skipped.** A box mid-boot and a box whose sshd
died look identical from here, and a hole in the series would read as "nobody
sampled" rather than "nobody answered".

**It measures `$HF_HOME` as resolved on the box**, not an assumed path, because
a warm-volume deploy points it at the mount and measuring the wrong directory
reports a download that never starts.

Flags: `--deployment` / `--instance`, `--model` (or `--total-bytes`),
`--interval`, `--out`, `--service-url`, `--state-dir`.

**Pass `--service-url ""` explicitly for an in-process run.** The CLI defaults
it to `http://localhost:8080`, so a script using `${VAR:+--service-url "$VAR"}`
drops the flag in exactly the case that needs it and silently talks to
whatever daemon is up. That bug produced a full run of `"reachable": false`
against a box that was answering fine.

### Why this rather than the agent

The agent reports the same thing properly (issue 413) and does not need SSH.
But it only starts when the daemon has an externally reachable URL to stamp
(`IPLANE_AGENT_SERVICE_URL`) and the box can fetch an agent binary
(`IPLANE_AGENT_BINARY_URL`, or a released version). Neither has been true on
any run so far, so `agentPrelude` returned empty and no agent has ever run on
a real deploy. SSH is already there.

Verified against a live Vast box: it resolved Qwen2.5-7B-Instruct at 15.2 GB
and tracked a real download at 7-8 MB/s with a 27-minute ETA. That host was
slow enough to be worth knowing about, which is the point.

## measure-run.sh

Drives one paid measurement run end to end, and records what happened
whether or not it works.

```sh
make build
hack/vast-watchdog.sh --heartbeat /tmp/run.hb --max-stale 300 --max-lifetime 21600 &
hack/measure-run.sh --heartbeat /tmp/run.hb --model cyankiwi/GLM-5.2-AWQ-INT4
```

**It refuses to start unless a watchdog is already running.** Not a
suggestion: a teardown living in the process that can be killed is not a
teardown, and a script launched with a stray `&` was reaped as an orphan and
leaked a rental exactly that way. `--no-watchdog-check` overrides and accepts
the consequence.

**It deploys with `--debug-shell` by default**, and that is not a debugging
nicety. A deployment is proxy-only unless asked otherwise, so its replicas
have no SSH endpoint at all, so `deploy-watch` can never read them. Found the
expensive way: a run reached `engine:init` on a $54/hr box and reported
`reachable: false` every 30 seconds, which reads exactly like a machine still
booting rather than one that will never answer. `--no-debug-shell` opts out
and accepts a blind run.

**It runs `deploy-watch.sh` alongside the deploy**, so a run that times out
still says how fast the weights were arriving and how far they got. That is
the property all three GLM-5.2 attempts lacked: two of them cost about $22
each and ended in a guess.

**Teardown verifies against `/api/v1/instances/`.** The previous script asked
`/api/v0/instances/`, which is deprecated and answers with an error object
that parses as an empty list, so it printed "nothing is billing"
unconditionally, including when something was.

`--dry-run` does everything except rent: serve startup, readiness, the
download-size read, and teardown verification. Use it after editing. It has
already earned itself once, catching that an ISO-style timestamp made the
deployment id non-DNS-safe and would have been rejected the moment the real
deploy started.

Flags: `--model` and `--heartbeat` (both required), `--sku`, `--gpus`, `--tp`,
`--image`, `--engine-args`, `--ladder`, `--prompt-tokens`, `--min-disk-gb`, `--min-vram-gb`,
`--out`, `--dry-run`, `--no-watchdog-check`. `DEPLOY_TIMEOUT` defaults to 75m,
deliberately longer than `IPLANE_ENGINE_READY_TIMEOUT` so the engine-ready
wait loses the race and its log-carrying error actually fires.

Artifacts land in `measure-runs/run-<stamp>/` (gitignored): `serve.log`,
`model.txt`, `deploy.log`, `deploy-watch.jsonl`, and one `sweep-<tokens>.csv`
per context. The distilled csv is what gets committed, not the run directory.
