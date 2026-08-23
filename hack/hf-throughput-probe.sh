#!/usr/bin/env bash
# Measure how fast one rented box actually pulls weights from Hugging Face,
# and append the reading to a JSONL log.
#
# The reason this exists: a host's advertised link speed does not predict
# what it achieves against Hugging Face's CDN. A Vast host advertising
# 5,813 Mbps (726 MB/s) delivered 134 MB/s on the GLM-5.2 run, turning an
# 11-minute download into a 59-minute one that timed out and cost $22.
# Three runs produced three anecdotes and no distribution.
#
# The probe fetches ONE file and times it. On a fast host that is about
# ten seconds; on a slow one about forty. Either way the answer arrives
# before the meter has run two minutes, so a bad host can be handed back
# instead of ridden to a timeout.
#
# The fetched file is not wasted. It is written into the same HF cache
# layout the engine reads, so the shard counts toward the real download
# and vLLM's snapshot_download skips it.
#
# STATUS: the hub-side half (file selection, repo sizing, JSONL row) is
# exercised against the real GLM-5.2 repo. The REMOTE half has never run on
# hardware -- the rental it needed was not available when this landed. Treat
# the first real invocation as the test, and expect the remote shell quoting
# and the huggingface_hub import to be where it breaks.
#
# Usage:
#   hack/hf-throughput-probe.sh --instance <id> --model <repo> [--out probe.jsonl]
#
# Needs a rented instance reachable by `iplane instance ssh`, so run
# hack/vast-watchdog.sh first: this script does not rent and does not
# destroy, and the box it measures is billing the whole time.
set -uo pipefail

IPLANE="${IPLANE_BIN:-./bin/iplane}"
OUT="${HF_PROBE_OUT:-hf-throughput-samples.jsonl}"
INSTANCE=""; MODEL=""; FILE=""; MAX_FILE_GB="${HF_PROBE_MAX_FILE_GB:-6}"; TIMEOUT="${HF_PROBE_TIMEOUT:-600}"

while [ $# -gt 0 ]; do
  case "$1" in
    --instance) INSTANCE="$2"; shift 2 ;;
    --model)    MODEL="$2"; shift 2 ;;
    --file)     FILE="$2"; shift 2 ;;
    --out)      OUT="$2"; shift 2 ;;
    --max-file-gb) MAX_FILE_GB="$2"; shift 2 ;;
    --timeout)  TIMEOUT="$2"; shift 2 ;;
    -h|--help)  sed -n '2,30p' "$0"; exit 0 ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done

[ -n "$INSTANCE" ] || { echo "hf-throughput-probe: --instance is required" >&2; exit 2; }
[ -n "$MODEL" ]    || { echo "hf-throughput-probe: --model is required" >&2; exit 2; }
[ -x "$IPLANE" ]   || { echo "hf-throughput-probe: no iplane binary at $IPLANE (run 'make build' or set IPLANE_BIN)" >&2; exit 1; }

# Pick the file to time and learn the repo's full size, from the hub rather
# than from the box. Doing it here keeps the remote side to one timed
# fetch, and the repo total is what turns a rate into an ETA.
read -r FILE FILE_BYTES REPO_BYTES REPO_FILES <<EOF
$(MODEL="$MODEL" FILE="$FILE" MAX_FILE_GB="$MAX_FILE_GB" python3 -c '
import json, os, sys, urllib.request
model = os.environ["MODEL"]
want  = os.environ.get("FILE") or ""
cap   = float(os.environ["MAX_FILE_GB"]) * 1e9
req = urllib.request.Request("https://huggingface.co/api/models/%s/tree/main?recursive=true" % model)
tok = os.environ.get("HF_TOKEN")
if tok:
    req.add_header("Authorization", "Bearer " + tok)
try:
    tree = json.load(urllib.request.urlopen(req, timeout=30))
except Exception as e:
    print("ERROR 0 0 0", file=sys.stderr); sys.exit(1)
files = [(f["path"], (f.get("lfs") or {}).get("size") or f.get("size") or 0)
         for f in tree if f.get("type") == "file"]
total = sum(s for _, s in files)
if want:
    pick = next(((p, s) for p, s in files if p == want), None)
    if pick is None:
        sys.exit(1)
else:
    # Largest file that still fits the cap. Big enough that TCP ramp-up and
    # per-request overhead do not dominate the reading, small enough that a
    # slow host is diagnosed in under a minute rather than measured exactly.
    under = [(p, s) for p, s in files if s <= cap]
    pick = max(under or files, key=lambda x: x[1])
print(pick[0], pick[1], total, len(files))
')
EOF
[ -n "${FILE:-}" ] && [ "${FILE:-}" != "ERROR" ] || { echo "hf-throughput-probe: could not read $MODEL from the hub" >&2; exit 1; }
echo "probe: $MODEL -> $FILE ($(python3 -c "print('%.2f GB'%($FILE_BYTES/1e9))")) of $(python3 -c "print('%.1f GB'%($REPO_BYTES/1e9))") across $REPO_FILES files"

# The remote side times the SAME downloader the engine uses. Measuring with
# curl instead would time plain HTTPS against the CDN, and this repo is
# Xet-backed, so the two take different paths and the number would not
# predict what vLLM sees. If huggingface_hub is missing the probe says so
# rather than quietly measuring the wrong thing.
REMOTE=$(cat <<'REMOTE_EOF'
set -e
python3 - <<'PY'
import json, os, time
try:
    from huggingface_hub import hf_hub_download
except Exception as e:
    print(json.dumps({"error": "huggingface_hub not importable: %s" % e})); raise SystemExit(0)
import huggingface_hub.constants as C
repo, fn = os.environ["PROBE_REPO"], os.environ["PROBE_FILE"]
t0 = time.perf_counter()
try:
    p = hf_hub_download(repo_id=repo, filename=fn)
except Exception as e:
    print(json.dumps({"error": "download failed: %s" % e})); raise SystemExit(0)
dt = time.perf_counter() - t0
print(json.dumps({
    "seconds": dt,
    "bytes": os.path.getsize(p),
    "xet_high_performance": bool(getattr(C, "HF_XET_HIGH_PERFORMANCE", False)),
    "hf_transfer": bool(getattr(C, "HF_HUB_ENABLE_HF_TRANSFER", False)),
}))
PY
REMOTE_EOF
)
B64=$(printf '%s' "PROBE_REPO='$MODEL' PROBE_FILE='$FILE' $REMOTE" | base64 | tr -d '\n')

RAW=$("$IPLANE" instance ssh "$INSTANCE" -- "echo $B64 | base64 -d | sh" 2>&1) || true
JSON=$(printf '%s' "$RAW" | grep -o '{.*}' | tail -1)
[ -n "$JSON" ] || { echo "hf-throughput-probe: no reading from the box. Raw output:" >&2; printf '%s\n' "$RAW" >&2; exit 1; }

ts="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
printf '%s' "$JSON" | TS="$ts" INSTANCE="$INSTANCE" MODEL="$MODEL" FILE="$FILE" \
  REPO_BYTES="$REPO_BYTES" REPO_FILES="$REPO_FILES" python3 -c '
import json, os, sys
r = json.load(sys.stdin)
row = {"ts": os.environ["TS"], "instance": os.environ["INSTANCE"],
       "model": os.environ["MODEL"], "file": os.environ["FILE"],
       "repo_bytes": int(os.environ["REPO_BYTES"]), "repo_files": int(os.environ["REPO_FILES"])}
if r.get("error"):
    row["error"] = r["error"]
    print(json.dumps(row)); sys.exit(0)
row.update(r)
mbps = r["bytes"] / r["seconds"] / 1e6
row["mb_per_s"] = round(mbps, 1)
row["repo_eta_minutes"] = round(row["repo_bytes"] / (mbps * 1e6) / 60, 1)
print(json.dumps(row))
' > /tmp/hf-probe-row.$$ || { echo "hf-throughput-probe: could not parse the reading: $JSON" >&2; exit 1; }

cat /tmp/hf-probe-row.$$ >> "$OUT"
python3 -c '
import json, sys
r = json.load(open(sys.argv[1]))
if r.get("error"):
    print("probe FAILED: %s" % r["error"]); sys.exit(1)
print("  measured : %.1f MB/s (%.2f GB in %.1fs)" % (r["mb_per_s"], r["bytes"]/1e9, r["seconds"]))
print("  xet high-perf=%s hf_transfer=%s" % (r["xet_high_performance"], r["hf_transfer"]))
print("  whole repo would take ~%.1f min at this rate" % r["repo_eta_minutes"])
' /tmp/hf-probe-row.$$
rc=$?
rm -f /tmp/hf-probe-row.$$
echo "appended to $OUT"
exit $rc
