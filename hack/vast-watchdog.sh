#!/usr/bin/env bash
#
# Destroys Vast instances whose creator has died or which have outlived a
# deadline. Runs as its own process so that it survives whatever it is
# guarding.
#
# The failure it exists for: a paid run drives the CLI from a script, the
# script is killed, and the box keeps billing. A teardown living inside
# that script cannot help, because a killed process runs no EXIT trap. So
# the guard has to be somewhere the kill does not reach.
#
# Ownership is positive-only. An instance is a candidate when its label
# carries the prefix (iplane stamps "iplane-<deployment-id>"), or when its
# id was written to the registry file. Anything else is left alone however
# suspicious it looks, because the account also holds boxes this project
# did not create and destroying one of those is worse than leaking ours.
#
# Every uncertainty resolves to "leave it running". An unreadable API, an
# instance with no start_date, a heartbeat that has not appeared yet: all
# mean keep going. The one thing that makes this safe to run unattended is
# that it never destroys on an inference.
#
#   hack/vast-watchdog.sh --heartbeat run.hb --max-stale 300 --max-lifetime 5400
#
# The guarded run touches the heartbeat on a cadence and writes DONE into
# it on clean exit. See hack/README.md.
set -uo pipefail

API=https://console.vast.ai/api/v0
V1=https://console.vast.ai/api/v1

HEARTBEAT=""; REGISTRY=""; LABEL_PREFIX="iplane-"
MAX_STALE=300; MAX_LIFETIME=0; INTERVAL=30; MAX_RUNTIME=0; DRY_RUN=0

while [ $# -gt 0 ]; do
  case "$1" in
    --heartbeat)    HEARTBEAT="$2"; shift 2 ;;
    --registry)     REGISTRY="$2"; shift 2 ;;
    --label-prefix) LABEL_PREFIX="$2"; shift 2 ;;
    --max-stale)    MAX_STALE="$2"; shift 2 ;;
    --max-lifetime) MAX_LIFETIME="$2"; shift 2 ;;
    --interval)     INTERVAL="$2"; shift 2 ;;
    --max-runtime)  MAX_RUNTIME="$2"; shift 2 ;;
    --dry-run)      DRY_RUN=1; shift ;;
    -h|--help)      sed -n '2,30p' "$0"; exit 0 ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done

: "${VAST_API_KEY:?VAST_API_KEY must be set}"
AUTH="Authorization: Bearer ${VAST_API_KEY}"

log() { echo "$(date -u +%Y-%m-%dT%H:%M:%SZ) watchdog: $*"; }

# mtime in epoch seconds, BSD and GNU stat.
mtime() { stat -f %m "$1" 2>/dev/null || stat -c %Y "$1" 2>/dev/null; }

# Owned instances as "id<TAB>label<TAB>start_date<TAB>rate", one per line.
# Empty output means either "none owned" or "could not tell"; the caller
# distinguishes them by the exit status, because acting on the two the
# same way is how a transient API error turns into a teardown.
owned() {
  local body
  body=$(curl -sS --max-time 30 -H "$AUTH" "$V1/instances/" 2>/dev/null) || return 1
  REGISTRY="$REGISTRY" LABEL_PREFIX="$LABEL_PREFIX" python3 -c '
import sys, json, os
raw = sys.stdin.read()
try:
    d = json.loads(raw)
except Exception:
    sys.exit(3)
ins = d.get("instances", d) if isinstance(d, dict) else d
if not isinstance(ins, list):
    sys.exit(3)
prefix = os.environ["LABEL_PREFIX"]
reg = set()
path = os.environ.get("REGISTRY") or ""
if path and os.path.exists(path):
    with open(path) as fh:
        reg = {ln.strip() for ln in fh if ln.strip()}
for i in ins:
    iid = str(i.get("id"))
    label = i.get("label") or ""
    if not (label.startswith(prefix) or iid in reg):
        continue
    print("\t".join([iid, label, str(i.get("start_date") or ""), str(i.get("dph_total") or "")]))
' <<<"$body"
}

destroy() {
  local id="$1" why="$2"
  if [ "$DRY_RUN" = 1 ]; then
    log "DRY-RUN would destroy $id ($why)"
    return 0
  fi
  local i r
  for i in 1 2 3 4 5; do
    r=$(curl -sS --max-time 30 -X DELETE -H "$AUTH" "$API/instances/$id/" 2>/dev/null)
    case "$r" in
      *'"success": true'*|*'"success":true'*) log "destroyed $id ($why)"; return 0 ;;
    esac
    log "destroy $id attempt $i failed: ${r:-<no response>}"
    sleep 5
  done
  log "!!! COULD NOT DESTROY $id ($why) - DESTROY MANUALLY !!!"
  return 1
}

STARTED=$(date +%s)
ARMED=0
log "watching label-prefix='$LABEL_PREFIX' registry='${REGISTRY:-none}' heartbeat='${HEARTBEAT:-none}'"
log "max-stale=${MAX_STALE}s max-lifetime=${MAX_LIFETIME:-off}s interval=${INTERVAL}s dry-run=$DRY_RUN"

while :; do
  NOW=$(date +%s)

  # Heartbeat state. Until the file first appears the watchdog is unarmed,
  # so starting it before the run it guards is safe and is the intended
  # order: armed-on-first-sight cannot mistake "not started yet" for
  # "died", and those are the same observation.
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
    while IFS=$'\t' read -r ID LABEL START RATE; do
      [ -z "${ID:-}" ] && continue
      COUNT=$(( COUNT + 1 ))
      WHY=""
      if [ "$STALE" = 1 ]; then
        WHY="creator heartbeat stale (>${MAX_STALE}s)"
      elif [ "$MAX_LIFETIME" -gt 0 ] && [ -n "$START" ]; then
        AGE=$(python3 -c "print(int($NOW - float('$START')))" 2>/dev/null || echo "")
        if [ -n "$AGE" ] && [ "$AGE" -gt "$MAX_LIFETIME" ]; then
          WHY="age ${AGE}s exceeds max-lifetime ${MAX_LIFETIME}s"
        fi
      fi
      if [ -n "$WHY" ]; then
        destroy "$ID" "$WHY"
      else
        log "ok $ID label=$LABEL rate=\$$RATE/hr"
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
