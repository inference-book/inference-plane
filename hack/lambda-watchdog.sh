#!/usr/bin/env bash
#
# Terminates Lambda Labs instances whose creator has died or which have
# outlived a deadline. Runs as its own process so that it survives whatever
# it is guarding.
#
# The failure it exists for: a paid run drives the CLI from a script, the
# script is killed, and the VM keeps billing. A teardown living inside that
# script cannot help, because a killed process runs no EXIT trap. So the
# guard has to be somewhere the kill does not reach.
#
# This is hack/vast-watchdog.sh for the other VM provider, and it differs in
# two places, both forced by the API.
#
# Ownership reads the `name` field rather than a label. Lambda has a real
# tags array now, but internal/provisioners/lambdalabs stamps ownership into
# `name` as "iplane-<deployment-id>", so that is what a guard has to match.
#
# Age is measured from first sight, not from a launch timestamp. Lambda's
# instance record carries no created_at, launched_at or start_date at all
# (checked against the published OpenAPI document, 2026-08-24), so there is
# nothing to subtract. The watchdog writes the first time it saw each
# instance to a small state file and ages it from there. An instance that
# was already running when the guard started therefore looks younger than it
# is, which is the safe direction: --max-lifetime fires late rather than
# early, and firing early would destroy somebody else's healthy run.
#
# Ownership is positive-only. An instance is a candidate when its name
# carries the prefix, or when its id was written to the registry file.
# Anything else is left alone however suspicious it looks, because the
# account also holds boxes this project did not create and destroying one of
# those is worse than leaking ours.
#
# Every uncertainty resolves to "leave it running". An unreadable API, a
# response that will not parse, a heartbeat that has not appeared yet: all
# mean keep going. The one thing that makes this safe to run unattended is
# that it never terminates on an inference.
#
#   hack/lambda-watchdog.sh --heartbeat run.hb --max-stale 300 --max-lifetime 5400
#
# The guarded run touches the heartbeat on a cadence and writes DONE into it
# on clean exit. See hack/README.md.
set -uo pipefail

# LAMBDA_API_BASE is a test seam. tests/watchdog points it at a local server
# so the guard's ownership rules and its terminate call can be exercised
# without renting anything. Nothing in normal operation sets it.
API="${LAMBDA_API_BASE:-https://cloud.lambdalabs.com/api/v1}"

HEARTBEAT=""; REGISTRY=""; NAME_PREFIX="iplane-"; STATE=""
MAX_STALE=300; MAX_LIFETIME=0; INTERVAL=30; MAX_RUNTIME=0; DRY_RUN=0

while [ $# -gt 0 ]; do
  case "$1" in
    --heartbeat)    HEARTBEAT="$2"; shift 2 ;;
    --registry)     REGISTRY="$2"; shift 2 ;;
    --name-prefix)  NAME_PREFIX="$2"; shift 2 ;;
    --state)        STATE="$2"; shift 2 ;;
    --max-stale)    MAX_STALE="$2"; shift 2 ;;
    --max-lifetime) MAX_LIFETIME="$2"; shift 2 ;;
    --interval)     INTERVAL="$2"; shift 2 ;;
    --max-runtime)  MAX_RUNTIME="$2"; shift 2 ;;
    --dry-run)      DRY_RUN=1; shift ;;
    -h|--help)      sed -n '2,42p' "$0"; exit 0 ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done

: "${LAMBDA_API_KEY:?LAMBDA_API_KEY must be set}"

# Lambda authenticates with HTTP Basic, the API key as the username and an
# empty password. Not a Bearer token: it is the outlier among the three
# providers and a Bearer header here returns 401 on every call.
AUTH=(-u "${LAMBDA_API_KEY}:")

# First-sight ages live next to the heartbeat by default, so a run that
# names its own scratch directory keeps everything in one place.
if [ -z "$STATE" ]; then
  if [ -n "$HEARTBEAT" ]; then STATE="${HEARTBEAT}.seen"; else STATE="/tmp/lambda-watchdog.seen"; fi
fi
: >>"$STATE" || { echo "cannot write state file $STATE" >&2; exit 2; }

log() { echo "$(date -u +%Y-%m-%dT%H:%M:%SZ) lambda-watchdog: $*"; }

# mtime in epoch seconds, BSD and GNU stat.
mtime() { stat -f %m "$1" 2>/dev/null || stat -c %Y "$1" 2>/dev/null; }

# Owned instances as "id<TAB>name<TAB>status", one per line. Empty output
# means either "none owned" or "could not tell"; the caller distinguishes
# them by the exit status, because acting on the two the same way is how a
# transient API error turns into a teardown.
owned() {
  local body
  body=$(curl -sS --max-time 30 "${AUTH[@]}" "$API/instances" 2>/dev/null) || return 1
  REGISTRY="$REGISTRY" NAME_PREFIX="$NAME_PREFIX" python3 -c '
import sys, json, os
try:
    d = json.loads(sys.stdin.read())
except Exception:
    sys.exit(3)
ins = d.get("data") if isinstance(d, dict) else None
if not isinstance(ins, list):
    sys.exit(3)
prefix = os.environ["NAME_PREFIX"]
reg = set()
path = os.environ.get("REGISTRY") or ""
if path and os.path.exists(path):
    with open(path) as fh:
        reg = {ln.strip() for ln in fh if ln.strip()}
for i in ins:
    iid = str(i.get("id") or "")
    name = i.get("name") or ""
    if not iid:
        continue
    if not (name.startswith(prefix) or iid in reg):
        continue
    print("\t".join([iid, name, str(i.get("status") or "")]))
' <<<"$body"
}

# first_seen echoes the epoch second this instance id was first observed,
# recording it on the first call. An unwritable state file degrades to "seen
# just now", which keeps --max-lifetime from firing rather than making it
# fire on a bad reading.
first_seen() {
  local id="$1" now="$2" line
  line=$(grep -F "${id} " "$STATE" 2>/dev/null | head -1)
  if [ -n "$line" ]; then
    echo "${line##* }"
    return
  fi
  echo "${id} ${now}" >>"$STATE" 2>/dev/null
  echo "$now"
}

terminate() {
  local id="$1" why="$2"
  if [ "$DRY_RUN" = 1 ]; then
    log "DRY-RUN would terminate $id ($why)"
    return 0
  fi
  local i r
  for i in 1 2 3 4 5; do
    r=$(curl -sS --max-time 30 -X POST "${AUTH[@]}" \
          -H 'Content-Type: application/json' \
          -d "{\"instance_ids\":[\"$id\"]}" \
          "$API/instance-operations/terminate" 2>/dev/null)
    # A terminate that says the instance does not exist has reached the end
    # state this call exists for, so it counts as done.
    case "$r" in
      *'"terminated_instances"'*) log "terminated $id ($why)"; return 0 ;;
      *'object-does-not-exist'*)  log "already gone $id ($why)"; return 0 ;;
    esac
    log "terminate $id attempt $i failed: ${r:-<no response>}"
    sleep 5
  done
  log "!!! COULD NOT TERMINATE $id ($why) - TERMINATE MANUALLY AT https://cloud.lambdalabs.com/instances !!!"
  return 1
}

STARTED=$(date +%s)
ARMED=0
log "watching name-prefix='$NAME_PREFIX' registry='${REGISTRY:-none}' heartbeat='${HEARTBEAT:-none}'"
log "state='$STATE' max-stale=${MAX_STALE}s max-lifetime=${MAX_LIFETIME:-off}s interval=${INTERVAL}s dry-run=$DRY_RUN"

while :; do
  NOW=$(date +%s)

  # Heartbeat state. Until the file first appears the watchdog is unarmed,
  # so starting it before the run it guards is safe and is the intended
  # order: armed-on-first-sight cannot mistake "not started yet" for "died",
  # and those are the same observation.
  STALE=0; FINISHED=0
  if [ -n "$HEARTBEAT" ]; then
    if [ -f "$HEARTBEAT" ]; then
      [ "$ARMED" = 0 ] && { ARMED=1; log "armed (heartbeat appeared)"; }
      HB=$(mtime "$HEARTBEAT")
      if [ -n "$HB" ] && [ $(( NOW - HB )) -gt "$MAX_STALE" ]; then
        STALE=1
      fi
      grep -qx DONE "$HEARTBEAT" 2>/dev/null && FINISHED=1
    elif [ "$ARMED" = 1 ]; then
      STALE=1
    fi
  fi

  LIST=$(owned); RC=$?
  if [ $RC -ne 0 ]; then
    log "could not read the instance list (rc=$RC); leaving everything running"
  else
    COUNT=0
    while IFS=$'\t' read -r ID NAME STATUS; do
      [ -z "${ID:-}" ] && continue
      COUNT=$(( COUNT + 1 ))
      SEEN=$(first_seen "$ID" "$NOW")
      AGE=$(( NOW - SEEN ))
      WHY=""
      if [ "$STALE" = 1 ]; then
        WHY="creator heartbeat stale (>${MAX_STALE}s)"
      elif [ "$MAX_LIFETIME" -gt 0 ] && [ "$AGE" -gt "$MAX_LIFETIME" ]; then
        WHY="seen ${AGE}s ago, over max-lifetime ${MAX_LIFETIME}s"
      fi
      if [ -n "$WHY" ]; then
        terminate "$ID" "$WHY"
      else
        log "ok $ID name=$NAME status=$STATUS age=${AGE}s"
      fi
    done <<<"$LIST"
    [ "$COUNT" = 0 ] && log "no owned instances"
    if [ "$FINISHED" = 1 ] && [ "$COUNT" = 0 ]; then
      log "run reported DONE and nothing is owned; exiting"
      exit 0
    fi
  fi

  if [ "$MAX_RUNTIME" -gt 0 ] && [ $(( NOW - STARTED )) -gt "$MAX_RUNTIME" ]; then
    log "max-runtime reached; exiting"
    exit 0
  fi
  sleep "$INTERVAL"
done
