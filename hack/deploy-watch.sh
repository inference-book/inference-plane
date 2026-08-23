#!/usr/bin/env bash
# Watch what a deploy is actually doing, over SSH, while it is doing it.
#
# `engine:init` is silent. vLLM hands snapshot_download a disabled progress
# bar, so a container pulling a 474 GB checkpoint prints nothing until the
# fetch completes, and a deploy that times out first prints nothing at all.
# Three GLM-5.2 attempts produced one clean run and two hour-long silences
# that cost about $22 each and taught nothing: the diagnosis "the host was
# slow" was inferred from the elapsed time, never measured.
#
# The agent reports this properly (issue 413), but it only runs when the
# daemon has an externally reachable URL and the box can fetch an agent
# binary, and neither has been true on any run so far. SSH is already there.
# This asks the box directly.
#
# Free: the instance is rented regardless, and every reading is a shell
# command on a machine already being paid for.
#
# Usage:
#   hack/deploy-watch.sh --deployment glm52-run --total-bytes 474227712000
#   hack/deploy-watch.sh --instance <id> --model cyankiwi/GLM-5.2-AWQ-INT4
#
# --model resolves the download size through `iplane model describe`, so the
# rate becomes an ETA. Without a size it still reports bytes and a rate.
set -uo pipefail

IPLANE="${IPLANE_BIN:-./bin/iplane}"
DEPLOYMENT=""; INSTANCE=""; MODEL=""; TOTAL=0
INTERVAL="${DEPLOY_WATCH_INTERVAL:-30}"
OUT="${DEPLOY_WATCH_OUT:-deploy-watch.jsonl}"
# Tracked as "was it given" rather than "is it non-empty". --service-url
# defaults to http://localhost:8080 and an in-process run must pass it
# EXPLICITLY EMPTY to suppress that, so ${VAR:+--flag "$VAR"} drops exactly
# the case that needs it and the CLI quietly talks to whatever daemon is up.
SERVICE_URL=""; SERVICE_URL_SET=0
STATE_DIR=""; STATE_DIR_SET=0

while [ $# -gt 0 ]; do
  case "$1" in
    --deployment)  DEPLOYMENT="$2"; shift 2 ;;
    --instance)    INSTANCE="$2"; shift 2 ;;
    --model)       MODEL="$2"; shift 2 ;;
    --total-bytes) TOTAL="$2"; shift 2 ;;
    --interval)    INTERVAL="$2"; shift 2 ;;
    --out)         OUT="$2"; shift 2 ;;
    --service-url) SERVICE_URL="$2"; SERVICE_URL_SET=1; shift 2 ;;
    --state-dir)   STATE_DIR="$2"; STATE_DIR_SET=1; shift 2 ;;
    -h|--help)     sed -n '2,28p' "$0"; exit 0 ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done

[ -x "$IPLANE" ] || { echo "deploy-watch: no iplane binary at $IPLANE (run 'make build' or set IPLANE_BIN)" >&2; exit 1; }
[ -n "$DEPLOYMENT$INSTANCE" ] || { echo "deploy-watch: --deployment or --instance is required" >&2; exit 2; }

IPFLAGS=()
if [ "$SERVICE_URL_SET" = 1 ]; then IPFLAGS+=(--service-url "$SERVICE_URL")
elif [ -n "${IPLANE_SERVICE_URL-}" ]; then IPFLAGS+=(--service-url "$IPLANE_SERVICE_URL"); fi
[ "$STATE_DIR_SET" = 1 ] && IPFLAGS+=(--state-dir "$STATE_DIR")

ip() { "$IPLANE" "$@" "${IPFLAGS[@]}"; }

# The download size, so a rate becomes an answer rather than a number.
# Read from the hub rather than through `iplane model describe`, which has no
# machine-readable output. Same rule the store applies: safetensors only,
# because that is what the engine fetches, and a repository publishing several
# formats would otherwise overstate the download by up to 20x.
if [ "$TOTAL" = 0 ] && [ -n "$MODEL" ]; then
  TOTAL=$(MODEL="$MODEL" python3 -c '
import json, os, urllib.request
req = urllib.request.Request("https://huggingface.co/api/models/%s/tree/main?recursive=true" % os.environ["MODEL"])
if os.environ.get("HF_TOKEN"):
    req.add_header("Authorization", "Bearer " + os.environ["HF_TOKEN"])
try:
    tree = json.load(urllib.request.urlopen(req, timeout=30))
except Exception:
    print(0); raise SystemExit(0)
print(sum((f.get("lfs") or {}).get("size") or f.get("size") or 0
          for f in tree if f.get("type") == "file" and f["path"].endswith(".safetensors")))
' 2>/dev/null || echo 0)
  [ -z "$TOTAL" ] && TOTAL=0
fi
[ "$TOTAL" != 0 ] && echo "deploy-watch: $MODEL downloads $(python3 -c "print('%.1f GB'%($TOTAL/1e9))")"

# Everything the box can tell us in one round trip. Kept to POSIX shell and
# tools the engine image already has, because installing anything would
# change the machine we are trying to observe.
#
# HF_HOME is resolved on the box rather than assumed: a warm-volume deploy
# points it at the mount, and measuring the wrong directory reports a
# download that never starts.
REMOTE='
  H="${HF_HOME:-/root/.cache/huggingface}"
  [ -d "$H" ] || H=/root/.cache/huggingface
  B=$(du -sb "$H" 2>/dev/null | cut -f1); [ -z "$B" ] && B=-1
  D=$(df -B1 --output=avail "$H" 2>/dev/null | tail -1); [ -z "$D" ] && D=-1
  # awk, not bc: bc is absent from most engine images (including
  # pytorch/pytorch and vllm/vllm-openai), and the empty result read as
  # "no GPU reading" on a box that had nvidia-smi all along.
  G=$(nvidia-smi --query-gpu=memory.used --format=csv,noheader,nounits 2>/dev/null | awk "{s+=\$1} END{if(NR)print s; else print -1}")
  [ -z "$G" ] && G=-1
  P=$(ps -eo comm 2>/dev/null | grep -c -E "^(python3?|vllm)$")
  echo "{\"cache_bytes\":$B,\"disk_free_bytes\":$D,\"gpu_mem_used_mb\":$G,\"engine_procs\":$P}"
'

replicas() {
  if [ -n "$INSTANCE" ]; then echo "$INSTANCE"; return; fi
  ip deployment describe "$DEPLOYMENT" --output json 2>/dev/null \
    | python3 -c '
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    sys.exit(0)
# Both spellings. iplane marshals with protojson UseProtoNames, so the wire
# form is snake_case (instance_ids); camelCase is the protojson default and
# what a future marshaller change would emit. Accepting both stops this
# silently reporting a deployment as having no replicas, which is exactly
# what it did when it knew only the camelCase name.
for r in (d.get("replicas") or []):
    for i in (r.get("instance_ids") or r.get("instanceIds") or []):
        print(i)
' 2>/dev/null
}

PREV_BYTES=""; PREV_AT=""; NO_SSH_WARNED=0
echo "deploy-watch: polling every ${INTERVAL}s -> $OUT"
while :; do
  NOW=$(date +%s)
  STATE=$( [ -n "$DEPLOYMENT" ] && ip deployment describe "$DEPLOYMENT" 2>/dev/null | awk '/^state:/{print $2}' )
  PHASE=$( [ -n "$DEPLOYMENT" ] && ip deployment describe "$DEPLOYMENT" 2>/dev/null | awk '/^phase:/{print $2}' )

  for R in $(replicas); do
    ERRFILE=$(mktemp)
    RAW=$("$IPLANE" instance ssh "$R" "${IPFLAGS[@]}" -- \
            -o BatchMode=yes -o ConnectTimeout=15 "$REMOTE" 2>"$ERRFILE" \
          | grep -o '{.*}' | tail -1)
    # A replica with no SSH endpoint will NEVER become reachable, and saying
    # "unreachable" every 30s reads exactly like a box still booting. That
    # cost a $54/hr deploy six blind minutes before anyone noticed the
    # difference, so it is called out once and loudly rather than left to be
    # inferred from a run of identical rows.
    if [ -z "$RAW" ] && grep -q 'no SSH endpoint' "$ERRFILE" 2>/dev/null; then
      if [ "$NO_SSH_WARNED" = 0 ]; then
        NO_SSH_WARNED=1
        echo "deploy-watch: replica $R has NO SSH ENDPOINT, so nothing here can ever read it." >&2
        echo "deploy-watch: deployments are proxy-only by default; redeploy with --debug-shell" >&2
        echo "deploy-watch: (hack/measure-run.sh passes it unless --no-debug-shell)" >&2
      fi
      printf '{"ts":%s,"replica":"%s","state":"%s","phase":"%s","reachable":false,"reason":"no-ssh-endpoint"}\n' \
        "$NOW" "$R" "${STATE:-}" "${PHASE:-}" | tee -a "$OUT"
      rm -f "$ERRFILE"
      continue
    fi
    rm -f "$ERRFILE"
    if [ -z "$RAW" ]; then
      # Unreachable is a reading in its own right: a box mid-boot and a box
      # whose sshd died look the same from here, and recording the gap keeps
      # the series honest rather than leaving a hole that reads as "nobody
      # sampled".
      printf '{"ts":%s,"replica":"%s","state":"%s","phase":"%s","reachable":false}\n' \
        "$NOW" "$R" "${STATE:-}" "${PHASE:-}" | tee -a "$OUT"
      continue
    fi
    printf '%s' "$RAW" | TS="$NOW" R="$R" STATE="${STATE:-}" PHASE="${PHASE:-}" TOTAL="$TOTAL" \
      PREV_BYTES="$PREV_BYTES" PREV_AT="$PREV_AT" python3 -c '
import json, os, sys
r = json.load(sys.stdin)
row = {"ts": int(os.environ["TS"]), "replica": os.environ["R"],
       "state": os.environ["STATE"], "phase": os.environ["PHASE"], "reachable": True}
row.update(r)
b, total = r.get("cache_bytes", -1), int(os.environ["TOTAL"] or 0)
pb, pa = os.environ.get("PREV_BYTES") or "", os.environ.get("PREV_AT") or ""
if b >= 0 and pb and pa:
    dt = row["ts"] - int(pa)
    if dt > 0:
        rate = max(0, (b - int(pb))) / dt
        row["bytes_per_s"] = round(rate, 1)
        row["mb_per_s"] = round(rate / 1e6, 1)
        if total and rate > 0 and b < total:
            row["eta_minutes"] = round((total - b) / rate / 60, 1)
if total and b >= 0:
    row["percent"] = round(100.0 * b / total, 1)
print(json.dumps(row))
' | tee -a "$OUT"
    NB=$(printf '%s' "$RAW" | python3 -c 'import sys,json;print(json.load(sys.stdin).get("cache_bytes",-1))' 2>/dev/null)
    [ "${NB:-'-1'}" != "-1" ] && { PREV_BYTES="$NB"; PREV_AT="$NOW"; }
  done

  case "${STATE:-}" in
    RUNNING|FAILED|TERMINATED) echo "deploy-watch: deployment reached ${STATE}; stopping"; exit 0 ;;
  esac
  sleep "$INTERVAL"
done
