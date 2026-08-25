#!/usr/bin/env bash
# Drive one paid measurement run end to end, and record what happened
# whether or not it works.
#
# Three GLM-5.2 attempts produced one clean run and two hour-long silences
# at about $22 each. Neither failure was diagnosable afterwards, because
# nothing watched them: "the host was too slow" was inferred from the
# elapsed time, and is equally consistent with a stalled download, a crash
# loop, or a box that filled its disk. The point of this script is that a
# failed run is still a run that produced data.
#
# It does three things the previous scripts did not. It refuses to start
# unless a watchdog is already running, because a teardown living in the
# process that can be killed is not a teardown. It runs deploy-watch.sh
# alongside the deploy, so the silent hour becomes a byte count and a rate.
# And it verifies teardown against /api/v1/instances/, because /api/v0/
# is deprecated and answers with an error object that parses as an empty
# list, which made the old check print "nothing is billing" every time
# including when something was.
#
#   hack/vast-watchdog.sh --heartbeat /tmp/run.hb --max-stale 300 \
#       --max-lifetime 21600 &        # in ITS OWN process, first
#   hack/measure-run.sh --heartbeat /tmp/run.hb --model <repo> --sku H100_SXM
#
# Everything is parameterised because the next run is a different model on
# a different card, and a script that hardcodes one is a script that gets
# copied and edited into divergence.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "${HERE}/.." && pwd)"
IPLANE="${IPLANE_BIN:-${REPO}/bin/iplane}"

MODEL=""; HEARTBEAT=""; SKU="H100_SXM"; GPUS=8; TP=8; PROVIDER="vast"
IMAGE="vllm/vllm-openai:v0.27.1"
MIN_DISK_GB=700; MIN_VRAM_GB=80; FABRIC=""
ENGINE_ARGS="--max-model-len,131072,--max-num-seqs,32"

# NCCL_DEBUG=INFO, because a hung collective is otherwise one log line and
# then nothing. A GLM-5.2 deploy stopped after "vLLM is using nccl==2.30.7"
# and sat there for half an hour; with this, NCCL narrates topology detection
# and ring construction, so the silence has a last known position (P2P,
# InfiniBand, socket interface) instead of being undifferentiated.
#
# It is the only NCCL knob here that changes anything. Torch's watchdog is
# already on by default in the version this image ships (torch 2.13.0:
# TORCH_NCCL_ASYNC_ERROR_HANDLING=3, ENABLE_MONITORING=true,
# HEARTBEAT_TIMEOUT_SEC=480) and it did not fire on a 23-minute hang, because
# it watches collectives in flight and there was no work item yet. NCCL
# communicators are created blocking by default, so initialisation itself has
# no timeout to set. Setting those variables would have been cargo.
#
# The avoidance knobs (NCCL_P2P_DISABLE, NCCL_IB_DISABLE) are deliberately
# NOT set. Each costs real bandwidth on a healthy host, and paying that on
# every run to dodge a fault seen once is the wrong trade. Turn the debug
# output on, catch the next one with a diagnosis, then set the specific knob
# the log names.
ENGINE_ENV="${MEASURE_RUN_ENGINE_ENV:-NCCL_DEBUG=INFO}"
LADDER="1,2,4,8"; PROMPT_TOKENS="8192"

# Sweep sizing, against the numbers the first GLM-5.2 run produced rather than
# against the defaults. That run served correctly and the measurement was
# still not publishable:
#
#   - `--sweep-window` 3s while a request took 22s, so a "settled window" held
#     0.13 of one request and the steady-state detector was reading noise
#   - 45s of measurement gave 5 to 28 requests per level, making p95 the
#     second-highest of 28 observations rather than a percentile
#   - `--sweep-warmup-max` 90s expired at concurrency 4, which was then
#     measured anyway and reported 28 tok/s against concurrency 2's 91
#   - streaming off, so ttft_samples and itl_samples were both 0, and TTFT and
#     ITL are the numbers that separate prefill cost from decode cost
#   - 256 max tokens against an 8k prompt is a 40:1 prefill:decode ratio, so
#     the run measured prefill and said almost nothing about decode
#
# A window has to be longer than one request or it cannot detect anything.
SWEEP_WINDOW="${SWEEP_WINDOW:-30s}"
SWEEP_STABLE_WINDOWS="${SWEEP_STABLE_WINDOWS:-3}"
# Per context, comma-matched to --prompt-tokens (a single value applies to
# all). Long context needs longer, because a slower request yields fewer
# samples per second and percentile grade follows sample count. At 120k a
# request can take 45s, so even ten minutes buys tens of requests rather than
# hundreds: those rows are p50-grade and the validity check says so instead of
# pretending otherwise.
SWEEP_DURATION="${SWEEP_DURATION:-180s}"
SWEEP_WARMUP_MAX="${SWEEP_WARMUP_MAX:-8m}"
SWEEP_MAX_TOKENS="${SWEEP_MAX_TOKENS:-512}"
# Below this many successes a percentile is an extremum. Rows under it are
# reported as unpublishable rather than quietly charted.
SWEEP_MIN_SAMPLES="${SWEEP_MIN_SAMPLES:-100}"
DEPLOY_TIMEOUT="${DEPLOY_TIMEOUT:-75m}"

# Abort thresholds, MIRRORING internal/provisioners/staging.go. The control
# plane makes the same judgement from the agent's reading; this makes it from
# deploy-watch's, because the agent does not run on any deploy we have driven
# (agentPrelude needs both a stamped service URL and a fetchable binary, and
# no run has had either). Two implementations of one policy is a drift risk,
# so tests/constraints asserts these three numbers still match the Go ones.
#
# The zero-rate rule carries across unchanged and is the important one: a
# stalled download projects to infinity, so only a MEASURED POSITIVE rate may
# end a run. A download that has died is left to the deadline.
ABORT_MIN_WINDOW=90       # seconds of download observed before judging
ABORT_CONSECUTIVE=3       # readings in a row that must agree
ABORT_SLACK=1.5           # projection must overshoot the remaining time by this

# A hung collective looks nothing like a slow download and must be told apart.
# vLLM initialises NCCL across the ranks, then loads weights; if the collective
# never completes, the cards sit spinning in a busy-wait while the disk stays
# flat and the engine prints nothing further. A GLM-5.2 deploy did exactly
# that for half an hour: 188 MB on a 900 GB disk, 5.7 GB inbound (the image,
# not 474 GB of weights) and eight GPUs pinned at 86%.
#
# Deliberately stricter than the slow-download abort. A download that pauses
# between shards also shows a flat disk, so the GPU floor is what separates
# "waiting on the network" from "spinning on a collective", and the window is
# long enough that weight loading (busy cards, flat disk, legitimately) does
# not trip it.
STALL_GPU_BUSY_PCT=50     # cards this busy...
STALL_DISK_GROWTH_BYTES=$((512 * 1024 * 1024))   # ...while the disk grows less than this...
STALL_WINDOW=600          # ...for this long, is a hang rather than a fetch
# A port of its own, not 8080. A paid run must not share a port with every
# other iplane on the machine: one `freeport 8080` reached in and killed the
# daemon mid-download, and because the script itself stayed alive the
# heartbeat kept beating and the watchdog stayed passive while a $54/hr box
# went on billing with nothing able to drive it.
PORT="${MEASURE_RUN_PORT:-18080}"
SERVICE_URL=""
SKIP_WATCHDOG_CHECK=0; DRY_RUN=0; DEBUG_SHELL=1
OUT=""

while [ $# -gt 0 ]; do
  case "$1" in
    --model)         MODEL="$2"; shift 2 ;;
    --heartbeat)     HEARTBEAT="$2"; shift 2 ;;
    --sku)           SKU="$2"; shift 2 ;;
    --gpus)          GPUS="$2"; shift 2 ;;
    --tp)            TP="$2"; shift 2 ;;
    --image)         IMAGE="$2"; shift 2 ;;
    --engine-args)   ENGINE_ARGS="$2"; shift 2 ;;
    --engine-env)    ENGINE_ENV="$2"; shift 2 ;;
    --ladder)        LADDER="$2"; shift 2 ;;
    --prompt-tokens) PROMPT_TOKENS="$2"; shift 2 ;;
    --min-disk-gb)   MIN_DISK_GB="$2"; shift 2 ;;
    --min-vram-gb)   MIN_VRAM_GB="$2"; shift 2 ;;
    --fabric)        FABRIC="$2"; shift 2 ;;
    --out)           OUT="$2"; shift 2 ;;
    --port)          PORT="$2"; shift 2 ;;
    --provider)      PROVIDER="$2"; shift 2 ;;
    --no-watchdog-check) SKIP_WATCHDOG_CHECK=1; shift ;;
    --dry-run)       DRY_RUN=1; SKIP_WATCHDOG_CHECK=1; shift ;;
    --no-debug-shell) DEBUG_SHELL=0; shift ;;
    -h|--help)       sed -n '2,30p' "$0"; exit 0 ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done

log(){ echo "[$(date +%H:%M:%S)] $*"; }
fail(){ echo "measure-run: $*" >&2; exit 2; }

[ -n "$MODEL" ]     || fail "--model is required"
[ -n "$HEARTBEAT" ] || fail "--heartbeat is required (the watchdog reads it; see hack/vast-watchdog.sh)"
[ -x "$IPLANE" ]    || fail "no iplane binary at $IPLANE (run 'make build')"
[ -n "${VAST_API_KEY:-}" ] || fail "VAST_API_KEY unset"

# The discipline that keeps being forgotten, enforced instead of documented.
# A run whose only teardown is its own EXIT trap leaks the moment the process
# is killed rather than exited, which is what happened: a script launched
# with a stray `&` was reaped as an orphan and its trap never fired.
if [ "$SKIP_WATCHDOG_CHECK" = 0 ] && ! pgrep -f 'vast-watchdog\.sh' >/dev/null 2>&1; then
  fail "no watchdog running. start one FIRST, in its own process:
    hack/vast-watchdog.sh --heartbeat $HEARTBEAT --max-stale 300 --max-lifetime 21600 &
  (--no-watchdog-check overrides, and accepts the leak that follows)"
fi

# Lowercase and hyphenated, because the run id becomes the deployment id and
# that must be DNS-safe: an ISO-style stamp carries an uppercase T and the
# deploy is rejected before anything is rented. Caught by --dry-run, which is
# what --dry-run is for.
RUN="$(date +%Y%m%d-%H%M%S)"
OUT="${OUT:-${REPO}/measure-runs/run-${RUN}}"; mkdir -p "$OUT"
DEP_ID="run-${RUN}"
log "run dir: $OUT"
log "model=$MODEL sku=${GPUS}x${SKU} tp=$TP image=$IMAGE"

SERVE_PID=""; WATCH_PID=""; BEAT_PID=""
# Set the instant the deploy is ISSUED, not when it succeeds. A deploy that
# failed halfway may still have rented a machine, so teardown must run for
# an error exactly as it does for success.
DEPLOY_ISSUED=0
cleanup() {
  rc=$?
  set +e
  log "=== TEARDOWN ==="
  [ -n "$WATCH_PID" ] && kill "$WATCH_PID" 2>/dev/null
  [ -n "$BEAT_PID" ]  && kill "$BEAT_PID" 2>/dev/null
  if [ -n "$SERVE_PID" ] && kill -0 "$SERVE_PID" 2>/dev/null; then
    [ "$DEPLOY_ISSUED" = 1 ] && "$IPLANE" deployment destroy "$DEP_ID" --service-url "$SERVICE_URL" 2>&1 | tail -2
    kill "$SERVE_PID" 2>/dev/null; wait "$SERVE_PID" 2>/dev/null
  fi
  # The provider is the authority on what is billing, never our own state
  # file, and it has to be asked on v1: /api/v0/instances/ is deprecated and
  # returns {"success":false,...}, which parses as no instances and printed
  # an all-clear unconditionally.
  log "--- what Vast still has rented ---"
  curl -s -H "Authorization: Bearer ${VAST_API_KEY}" "https://console.vast.ai/api/v1/instances/" \
    | python3 -c "
import json,sys
try:
    d=json.load(sys.stdin); ins=d.get('instances',d) if isinstance(d,dict) else d
except Exception as e:
    print('  COULD NOT CHECK (%s) -- CHECK THE CONSOLE BY HAND' % e); raise SystemExit
if not isinstance(ins,list):
    print('  UNEXPECTED RESPONSE -- CHECK THE CONSOLE BY HAND'); raise SystemExit
if not ins: print('  none. nothing is billing.')
for i in ins:
    print('  STILL RUNNING id=%s %s x%s \$%s/hr -- the watchdog should take it' %
          (i.get('id'), i.get('gpu_name'), i.get('num_gpus'), i.get('dph_total')))
"
  log "=== teardown done (rc=$rc); artifacts in $OUT ==="
  exit "$rc"
}
trap cleanup EXIT INT TERM

# Keep the watchdog's heartbeat fresh for as long as this script lives, and
# let it go stale the moment it does not. That is the signal the watchdog
# acts on, and it is deliberately not something the teardown path writes.
( while :; do date +%s > "$HEARTBEAT"; sleep 20; done ) & BEAT_PID=$!

SERVICE_URL="http://localhost:${PORT}"
lsof -iTCP:"$PORT" -sTCP:LISTEN >/dev/null 2>&1 && fail ":${PORT} busy (another iplane serve? pass --port)"

# A state dir of its own, for the same reason capacity-sample.sh takes one.
# Sharing ~/.iplane means a measurement run loads whatever deployments happen
# to be on the machine: the last one adopted a stale "demo" deployment and
# spent the run quarantining its replicas, which is noise at best and a
# teardown aimed at the wrong record at worst.
RUN_STATE="${OUT}/state"; mkdir -p "$RUN_STATE"
sed "s|addr: \":8080\"|addr: \":${PORT}\"|" "${REPO}/deploy/config.yaml" > "${OUT}/config.yaml"

log "starting serve on :${PORT} with its own state dir"
env -u IPLANE_SERVICE_URL "$IPLANE" serve --config "${OUT}/config.yaml" --state-dir "$RUN_STATE" \
  > "${OUT}/serve.log" 2>&1 & SERVE_PID=$!
# Readiness is "the RPC we are about to use answers", not an HTTP health
# path: `serve` exposes no /healthz, and a probe against a route that does
# not exist waits forever on a daemon that came up fine.
SERVE_DEADLINE=$(( $(date +%s) + 90 ))
until "$IPLANE" deployment list --service-url "$SERVICE_URL" >/dev/null 2>&1; do
  kill -0 "$SERVE_PID" 2>/dev/null || { tail -20 "${OUT}/serve.log"; fail "serve died on startup"; }
  [ "$(date +%s)" -lt "$SERVE_DEADLINE" ] || { tail -20 "${OUT}/serve.log"; fail "serve did not answer within 90s"; }
  sleep 2
done
log "serve is answering"

log "download size, before renting anything:"
"$IPLANE" model describe "$MODEL" --service-url "$SERVICE_URL" 2>&1 | tee "${OUT}/model.txt" | sed -n '/download/p'

# --wait=false so no single foreground call spans the deploy. The daemon
# provisions in its own goroutine and this polls, which is what lets a long
# cold start survive a caller that cannot block for an hour.
if [ "$DRY_RUN" = 1 ]; then
  # Everything except the part that costs money. Exercises serve startup,
  # readiness, the model read and the teardown verification, which is the
  # plumbing that has broken before, without renting anything.
  log "DRY RUN: would deploy $DEP_ID as ${GPUS}x${SKU} tp=$TP, engine-args '$ENGINE_ARGS'"
  log "DRY RUN: would then sweep ladder=$LADDER at prompt-tokens=$PROMPT_TOKENS"
  log "DRY RUN: stopping before anything is rented"
  exit 0
fi

# --debug-shell by default, and this is not a debugging nicety. A deployment
# is proxy-only unless it is asked for, which means its replicas have no SSH
# endpoint, which means deploy-watch can never read them and the run is blind
# for exactly the phase it exists to observe. It narrows placement and costs
# a routable IP; being unable to see an hour of engine:init costs more.
# A fabric describes how cards reach each other, so it is meaningless on one
# card and iplane refuses it: "fabric_scope needs gpu_count >= 2". Defaulting
# it unconditionally made every single-GPU rehearsal fail at provision, which
# is a safe failure but a useless one. Default it by width instead.
if [ -z "$FABRIC" ] && [ "$GPUS" -ge 2 ] && [ "$PROVIDER" != "local" ]; then FABRIC="intra-node"; fi
FABRIC_FLAG=()
[ -n "$FABRIC" ] && FABRIC_FLAG=(--fabric "$FABRIC")

ENGINE_ENV_FLAG=()
[ -n "$ENGINE_ENV" ] && ENGINE_ENV_FLAG=(--env "$ENGINE_ENV")

DEBUG_FLAG=()
if [ "$DEBUG_SHELL" = 1 ] && [ "$PROVIDER" != "local" ]; then
  DEBUG_FLAG=(--debug-shell)
  log "deploying with --debug-shell (deploy-watch needs SSH; --no-debug-shell to opt out)"
else
  log "NO --debug-shell: replicas will have no SSH, so deploy-watch can see nothing"
fi
log "deploying $DEP_ID"
DEPLOY_ISSUED=1
"$IPLANE" deployment deploy "$DEP_ID" \
  --provider "$PROVIDER" --sku "$SKU" --gpu-count "$GPUS" --min-vram-gb "$MIN_VRAM_GB" \
  ${FABRIC_FLAG[@]+"${FABRIC_FLAG[@]}"} --min-disk-gb "$MIN_DISK_GB" --tp "$TP" \
  ${DEBUG_FLAG[@]+"${DEBUG_FLAG[@]}"} \
  --engine-entrypoint python3 --engine-entrypoint=-m \
  --engine-entrypoint vllm.entrypoints.openai.api_server \
  --image "$IMAGE" --model "$MODEL" --engine-args "$ENGINE_ARGS" \
  ${ENGINE_ENV_FLAG[@]+"${ENGINE_ENV_FLAG[@]}"} \
  --wait=false --service-url "$SERVICE_URL" 2>&1 | tee "${OUT}/deploy.log"

# The whole reason this script exists. Runs for the life of the deploy and
# writes a row per replica per tick, so a run that times out still says how
# fast the weights were arriving and how far they got.
log "watching the deploy (bytes, rate, ETA) -> ${OUT}/deploy-watch.jsonl"
"${HERE}/deploy-watch.sh" --deployment "$DEP_ID" --model "$MODEL" \
  --service-url "$SERVICE_URL" --interval 30 --out "${OUT}/deploy-watch.jsonl" \
  > "${OUT}/deploy-watch.log" 2>&1 & WATCH_PID=$!

DEADLINE=$(( $(date +%s) + $(python3 -c "
import re,sys
s='$DEPLOY_TIMEOUT'; m=re.match(r'^(\d+)([smh])$', s)
print({'s':1,'m':60,'h':3600}[m.group(2)]*int(m.group(1)) if m else 4500)") ))
STATE=""; HOPELESS=0; DOWNLOAD_FIRST_SEEN=""; ABORTED=""; SERVE_DIED=0
while [ "$(date +%s)" -lt "$DEADLINE" ]; do
  # A dead daemon is a dead run, and must stop it rather than be polled
  # through. Nothing can drive the deployment without serve: no sweep, no
  # destroy, no way to notice. Exiting is also what re-arms the watchdog,
  # since the heartbeat stops with this script; a run that keeps looping
  # keeps beating and tells the watchdog all is well while the box bills.
  if ! kill -0 "$SERVE_PID" 2>/dev/null; then
    log "=== serve died; the deployment cannot be driven. tearing down ==="
    tail -5 "${OUT}/serve.log" 2>/dev/null | sed 's/^/  /'
    SERVE_DIED=1
    break
  fi
  STATE=$("$IPLANE" deployment describe "$DEP_ID" --service-url "$SERVICE_URL" 2>/dev/null | awk '/^state:/{print $2}')
  case "$STATE" in RUNNING|FAILED|TERMINATED) break ;; esac

  # The judgement the control plane would make if the agent were running.
  # Reads the newest watcher row and asks whether this download can still
  # finish before DEADLINE.
  # Exported, not prefixed. `NOW=x DEADLINE=y tail ... | python3` applies the
  # assignments to `tail`, and python3 on the far side of the pipe never sees
  # them: every tick threw KeyError and the abort was dead for a whole paid
  # run while looking armed.
  export NOW DEADLINE SLACK
  NOW="$(date +%s)"; SLACK="$ABORT_SLACK"
  VERDICT=$(tail -1 "${OUT}/deploy-watch.jsonl" 2>/dev/null | python3 -c '
import json, os, sys
line = sys.stdin.read().strip()
if not line:
    print("SKIP no reading"); raise SystemExit
try:
    r = json.loads(line)
except Exception:
    print("SKIP unparseable"); raise SystemExit
if not r.get("reachable"):
    print("SKIP unreachable"); raise SystemExit
rate = r.get("bytes_per_s") or 0
# THE guard. A stall projects to infinity and would abort every download
# that paused between shards, hardest on the biggest checkpoints.
if rate <= 0:
    print("SKIP no positive rate"); raise SystemExit
eta = r.get("eta_minutes")
if eta is None:
    print("SKIP no eta"); raise SystemExit
left = (int(os.environ["DEADLINE"]) - int(os.environ["NOW"])) / 60.0
if left <= 0:
    print("SKIP past deadline"); raise SystemExit
if eta > left * float(os.environ["SLACK"]):
    print("HOPELESS needs %.0f min, %.0f min left, %.1f%% done at %.1f MB/s"
          % (eta, left, r.get("percent") or 0, r.get("mb_per_s") or 0))
else:
    print("OK eta %.0f min within %.0f min left" % (eta, left))
')
  # The provider's view, which needs no key and works where SSH does not.
  STALL=$(NOW="$(date +%s)" GPU_MIN="$STALL_GPU_BUSY_PCT" GROWTH="$STALL_DISK_GROWTH_BYTES" \
    WINDOW="$STALL_WINDOW" python3 - "${OUT}/deploy-watch.jsonl" <<'PYSTALL'
import json, os, sys
try:
    rows = [json.loads(l) for l in open(sys.argv[1]) if l.strip().startswith("{")]
except Exception:
    print("SKIP"); raise SystemExit
rows = [r for r in rows if r.get("provider_gpu_util") is not None]
if len(rows) < 2:
    print("SKIP no provider readings"); raise SystemExit
now, window = int(os.environ["NOW"]), int(os.environ["WINDOW"])
recent = [r for r in rows if now - int(r.get("ts", 0)) <= window]
if len(recent) < 2 or (int(recent[-1]["ts"]) - int(recent[0]["ts"])) < window * 0.8:
    print("SKIP window not covered"); raise SystemExit
gpus = [float(r.get("provider_gpu_util") or 0) for r in recent]
disks = [int(r.get("provider_disk_bytes") or 0) for r in recent]
growth = max(disks) - min(disks)
if min(gpus) < float(os.environ["GPU_MIN"]):
    print("SKIP cards not consistently busy"); raise SystemExit
if growth >= int(os.environ["GROWTH"]):
    print("SKIP disk still growing"); raise SystemExit
print("STALLED cards at %.0f%% for %ds with the disk flat (%.2f GB growth): a collective that never completed, not a download"
      % (min(gpus), int(recent[-1]["ts"]) - int(recent[0]["ts"]), growth / 1e9))
PYSTALL
)
  case "$STALL" in
    STALLED*)
      ABORTED="${STALL#STALLED }"
      log "=== ABANDONING: $ABORTED ==="
      log "    retry elsewhere; this host will not finish"
      break
      ;;
  esac

  case "$VERDICT" in
    HOPELESS*)
      [ -z "$DOWNLOAD_FIRST_SEEN" ] && DOWNLOAD_FIRST_SEEN=$(date +%s)
      OBSERVED=$(( $(date +%s) - DOWNLOAD_FIRST_SEEN ))
      if [ "$OBSERVED" -lt "$ABORT_MIN_WINDOW" ]; then
        log "slow, but only ${OBSERVED}s observed (need ${ABORT_MIN_WINDOW}s): ${VERDICT#HOPELESS }"
      else
        HOPELESS=$(( HOPELESS + 1 ))
        log "hopeless ${HOPELESS}/${ABORT_CONSECUTIVE}: ${VERDICT#HOPELESS }"
        if [ "$HOPELESS" -ge "$ABORT_CONSECUTIVE" ]; then
          ABORTED="${VERDICT#HOPELESS }"
          log "=== ABANDONING: $ABORTED ==="
          log "    retry on a faster host, or raise DEPLOY_TIMEOUT"
          break
        fi
      fi
      ;;
    OK*)
      # Any reading that is not hopeless clears the count, so "slow three
      # times ever" is never mistaken for "slow three times running".
      [ "$HOPELESS" != 0 ] && log "recovered: ${VERDICT#OK }"
      HOPELESS=0; DOWNLOAD_FIRST_SEEN=""
      ;;
  esac
  sleep 20
done
log "deployment state: ${STATE:-unknown}"

if [ "$SERVE_DIED" = 1 ]; then
  tail -5 "${OUT}/deploy-watch.jsonl" 2>/dev/null | sed 's/^/  /'
  exit 1
fi

if [ -n "$ABORTED" ]; then
  log "run abandoned before the deadline; teardown follows"
  echo "$ABORTED" > "${OUT}/aborted.txt"
  tail -5 "${OUT}/deploy-watch.jsonl" 2>/dev/null | sed 's/^/  /'
  exit 1
fi

if [ "$STATE" != "RUNNING" ]; then
  log "did not reach RUNNING. what the watcher saw, last 5 readings:"
  tail -5 "${OUT}/deploy-watch.jsonl" 2>/dev/null | sed 's/^/  /'
  "$IPLANE" deployment describe "$DEP_ID" --service-url "$SERVICE_URL" > "${OUT}/failed-describe.txt" 2>&1
  exit 1
fi

kill "$WATCH_PID" 2>/dev/null; WATCH_PID=""
# A weak measurement has to announce itself. The first GLM-5.2 run charted a
# level that never settled and percentiles drawn from 28 samples, and neither
# was visible without reading the CSV by hand.
sweep_validity() {
  [ -s "$1" ] || { log "  no sweep artifact at $1"; return; }
  MIN="$SWEEP_MIN_SAMPLES" CTX="$2" python3 - "$1" <<'PYV'
import csv, os, sys
rows = [r for r in csv.DictReader(l for l in open(sys.argv[1]) if not l.startswith("#"))]
if not rows:
    print("  VALIDITY: no rows"); raise SystemExit
minimum = int(os.environ["MIN"])
# Grades, not a pass mark. A percentile needs samples behind it, and at long
# context enough samples for a p95 costs more than the answer is worth. Saying
# which rows support which statistic beats calling most of a sweep a failure.
def grade(succ, settled):
    if not settled:
        return "UNUSABLE", "never settled"
    if succ >= minimum:
        return "p95", "%d samples" % succ
    if succ >= 30:
        return "p50", "%d samples: p50 holds, p95 is the %.0fth of %d" % (succ, 0.95 * succ, succ)
    return "UNUSABLE", "%d samples: too few for any percentile" % succ

counts = {}
lines = []
for r in rows:
    succ = int(r["successes"])
    g, why = grade(succ, r["steady_state"].lower() == "true")
    # TTFT percentiles are drawn from ttft_samples, not from successes, so a
    # row can be p95 on throughput and much weaker on TTFT. They were far
    # apart once already: the loadgen parsed only chat-shaped frames while
    # sending most traffic to /v1/completions, so ttft_samples sat at exactly
    # --chat-fraction of successes and the grader called those rows p95
    # anyway (#437). Grade the column on its own evidence.
    ttft = int(r["ttft_samples"])
    if ttft == 0:
        g, why = "UNUSABLE", why + "; no TTFT (streaming off?)"
    elif ttft < succ * 0.9:
        g, why = "UNUSABLE", why + "; TTFT on only %d of %d requests" % (ttft, succ)
    else:
        tg, _ = grade(ttft, True)
        if tg != g and g != "UNUSABLE":
            why += "; TTFT is %s-grade on %d samples" % (tg, ttft)
    counts[g] = counts.get(g, 0) + 1
    lines.append("    N=%-3s %-8s %s" % (r["concurrency"], g, why))
print("  VALIDITY at %s tokens: %s" % (
    os.environ["CTX"], ", ".join("%d %s" % (v, k) for k, v in sorted(counts.items()))))
for l in lines:
    print(l)
if counts.get("UNUSABLE"):
    print("    ^ UNUSABLE rows must not be charted: lengthen this context's --sweep-duration or drop the level")
PYV
}

log "=== ENGINE IS SERVING ==="

# One real request through the exact URL the sweep will use, before spending
# an hour of rental on it. "The engine is serving" and "the sweep can reach
# the engine" are different claims, and the gap between them is where a whole
# measurement was lost: the deploy was healthy, the daemon was healthy, and
# every request went to a closed port because the sweep defaulted its --url.
#
# Deliberately checks the path rather than the port. A listener on the right
# port proves nothing about whether a completion round-trips, and a 200 here
# is the same call the sweep makes.
log "pre-flight: one completion through ${SERVICE_URL}"
PREFLIGHT=$(curl -s -o /dev/null -w '%{http_code}' --max-time 120 \
  -H 'Content-Type: application/json' \
  -d "{\"model\":\"${MODEL}\",\"prompt\":\"hello\",\"max_tokens\":1}" \
  "${SERVICE_URL}/v1/completions" 2>/dev/null)
if [ "$PREFLIGHT" != "200" ]; then
  fail "pre-flight got HTTP '${PREFLIGHT}' from ${SERVICE_URL}/v1/completions.
  The engine is serving but the sweep cannot reach it, so every level would
  measure nothing while the box bills. Teardown follows."
fi
log "pre-flight ok"
log "sweeping: ladder=$LADDER prompt-tokens=$PROMPT_TOKENS"
CTX_INDEX=0
for pt in $(echo "$PROMPT_TOKENS" | tr ',' ' '); do
  # Nth duration for the Nth context, or the last one given, so a single
  # value still applies everywhere.
  THIS_DURATION=$(echo "$SWEEP_DURATION" | tr ',' ' ' | awk -v i=$((CTX_INDEX + 1)) '{print (i <= NF) ? $i : $NF}')
  CTX_INDEX=$((CTX_INDEX + 1))
  log "  context ${pt} (measuring ${THIS_DURATION} per level)"
  # --target AND --service-url, both explicitly, and neither is optional.
  #
  # Routing: `iplane load` defaults its URL to localhost:8080 and this script
  # deliberately serves on $PORT (18080), so passing neither fires the whole
  # sweep at a closed port: an empty csv, no traffic, and a rented box billing
  # through all of it. Cost 13 minutes at $32.88/hr to find, because the
  # failure is invisible from the log -- the sweep header prints the URL it is
  # about to use and then simply never reports a level.
  #
  # Provenance: sweepFleetProvenance() returns an EMPTY struct unless BOTH
  # --target and --service-url are set, and it returns it silently, before the
  # warning that names the problem. So the flat --url form routes correctly
  # and still writes an artifact with no provider, no gpu_sku, no gpu_count
  # and no plan, which is precisely the hardware #347 says a figure's data
  # file has to carry. A sweep that measures the right box and cannot say
  # which box it was costs the same money and is worth less.
  #
  # stderr is teed rather than redirected, so the sweep's own progress lines
  # reach this runner's output as well as the log. Every other phase here
  # reports continuously and the sweep is the longest and dearest, so a file
  # somebody reads afterwards is the wrong place for the one signal that
  # says whether the level is working (#438). Process substitution rather
  # than a pipe, so the sweep's exit status still reaches the || below.
  "$IPLANE" load --sweep "$LADDER" --prompt-tokens "$pt" --model "$MODEL" \
    --target "$DEP_ID" --service-url "$SERVICE_URL" \
    --stream --max-tokens "$SWEEP_MAX_TOKENS" \
    --sweep-window "$SWEEP_WINDOW" --sweep-stable-windows "$SWEEP_STABLE_WINDOWS" \
    --sweep-duration "$THIS_DURATION" --sweep-warmup-max "$SWEEP_WARMUP_MAX" \
    --output csv > "${OUT}/sweep-${pt}.csv" 2> >(tee "${OUT}/sweep-${pt}.log" >&2) \
    || log "  sweep at ${pt} failed; see sweep-${pt}.log"
  sweep_validity "${OUT}/sweep-${pt}.csv" "$pt"
done
log "=== done; artifacts in $OUT ==="
