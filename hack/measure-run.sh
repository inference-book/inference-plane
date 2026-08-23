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

MODEL=""; HEARTBEAT=""; SKU="H100_SXM"; GPUS=8; TP=8
IMAGE="vllm/vllm-openai:v0.27.1"
MIN_DISK_GB=700; MIN_VRAM_GB=80
ENGINE_ARGS="--max-model-len,131072,--max-num-seqs,32"
LADDER="1,2,4,8"; PROMPT_TOKENS="8192"
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
SERVICE_URL="http://localhost:8080"
SKIP_WATCHDOG_CHECK=0; DRY_RUN=0
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
    --ladder)        LADDER="$2"; shift 2 ;;
    --prompt-tokens) PROMPT_TOKENS="$2"; shift 2 ;;
    --min-disk-gb)   MIN_DISK_GB="$2"; shift 2 ;;
    --out)           OUT="$2"; shift 2 ;;
    --no-watchdog-check) SKIP_WATCHDOG_CHECK=1; shift ;;
    --dry-run)       DRY_RUN=1; SKIP_WATCHDOG_CHECK=1; shift ;;
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

lsof -iTCP:8080 -sTCP:LISTEN >/dev/null 2>&1 && fail ":8080 busy (another iplane serve?)"
log "starting serve"
env -u IPLANE_SERVICE_URL "$IPLANE" serve > "${OUT}/serve.log" 2>&1 & SERVE_PID=$!
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

log "deploying $DEP_ID"
DEPLOY_ISSUED=1
"$IPLANE" deployment deploy "$DEP_ID" \
  --provider vast --sku "$SKU" --gpu-count "$GPUS" --min-vram-gb "$MIN_VRAM_GB" \
  --fabric intra-node --min-disk-gb "$MIN_DISK_GB" --tp "$TP" \
  --engine-entrypoint python3 --engine-entrypoint=-m \
  --engine-entrypoint vllm.entrypoints.openai.api_server \
  --image "$IMAGE" --model "$MODEL" --engine-args "$ENGINE_ARGS" \
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
STATE=""; HOPELESS=0; DOWNLOAD_FIRST_SEEN=""; ABORTED=""
while [ "$(date +%s)" -lt "$DEADLINE" ]; do
  STATE=$("$IPLANE" deployment describe "$DEP_ID" --service-url "$SERVICE_URL" 2>/dev/null | awk '/^state:/{print $2}')
  case "$STATE" in RUNNING|FAILED|TERMINATED) break ;; esac

  # The judgement the control plane would make if the agent were running.
  # Reads the newest watcher row and asks whether this download can still
  # finish before DEADLINE.
  VERDICT=$(NOW="$(date +%s)" DEADLINE="$DEADLINE" SLACK="$ABORT_SLACK"     tail -1 "${OUT}/deploy-watch.jsonl" 2>/dev/null | python3 -c '
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
log "=== ENGINE IS SERVING ==="
log "sweeping: ladder=$LADDER prompt-tokens=$PROMPT_TOKENS"
for pt in $(echo "$PROMPT_TOKENS" | tr ',' ' '); do
  log "  context ${pt}"
  "$IPLANE" load --sweep "$LADDER" --prompt-tokens "$pt" --model "$MODEL" \
    --output csv > "${OUT}/sweep-${pt}.csv" 2> "${OUT}/sweep-${pt}.log" \
    || log "  sweep at ${pt} failed; see sweep-${pt}.log"
done
log "=== done; artifacts in $OUT ==="
