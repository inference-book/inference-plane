#!/usr/bin/env bash
# Sample what every configured provider would rent, and append it to a
# JSONL log. Free and read-only by contract: `iplane capacity` asks
# providers what they have and rents nothing.
#
# The reason this exists is that frontier capacity is perishable and a
# single observation cannot show that. Eight-card availability was seen
# going from one offer to none across twenty-three minutes while planning
# the GLM run, which is an anecdote until it is sampled repeatedly. The
# output is meant to become a figure showing what eight-card supply
# actually looks like over days rather than at one instant.
#
# Usage:
#   hack/capacity-sample.sh [output.jsonl]
#
# Needs the provider API keys in the environment (VAST_API_KEY,
# RUNPOD_API_KEY, LAMBDA_API_KEY). A provider whose key is missing is
# skipped by iplane itself and recorded as having answered nothing, which
# is indistinguishable from real absence, so check your keys before
# trusting a run of zeroes.
set -uo pipefail

OUT="${1:-${CAPACITY_SAMPLE_OUT:-capacity-samples.jsonl}}"
IPLANE="${IPLANE_BIN:-./bin/iplane}"
PROVIDERS="${CAPACITY_SAMPLE_PROVIDERS:-runpod,vast,lambdalabs}"
WIDTHS="${CAPACITY_SAMPLE_WIDTHS:-1 2 4 8}"

# Floors, plural, and that is the whole point of this variable.
#
# `iplane capacity` returns the cheapest few SKUs clearing the floor, capped
# at skucatalog.MaxResults, so that an operator asking for "small" does not
# land on a frontier card because the cheap tiers were busy. The cap is right
# and it makes a single floor a narrow window rather than a floor: at 80 the
# five cheapest 80 GB-plus cards are A100s and H100s, and H200, B200 and B300
# never appear however much of them the market is holding.
#
# Sampling one floor therefore produced six days of eight-card observations
# that recorded no Blackwell at all while 8x B200 was live on Vast at $47/hr,
# and this log is what the epic's "blocked on capacity" judgements are made
# from. 140 clears every Hopper part and reaches the Blackwell tier; 80 keeps
# the series that already exists comparable.
MIN_VRAMS="${CAPACITY_SAMPLE_MIN_VRAM_GB:-80 140}"
TIMEOUT="${CAPACITY_SAMPLE_TIMEOUT:-60}"

if [ ! -x "$IPLANE" ]; then
  echo "capacity-sample: no iplane binary at $IPLANE (run 'make build' or set IPLANE_BIN)" >&2
  exit 1
fi

# A state dir of its own. capacity takes no state lock, but pointing it at
# a scratch dir keeps a sampling run from touching a real fleet's records.
STATE_DIR="${CAPACITY_SAMPLE_STATE_DIR:-$(mktemp -d)}"

ts="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
rows=0
for n in $WIDTHS; do
 for MIN_VRAM in $MIN_VRAMS; do
  # --service-url "" forces the in-process path. Without it the flag
  # defaults to http://localhost:8080 and the sample silently describes
  # whatever daemon happens to be up.
  json="$(IPLANE_STATE_DIR="$STATE_DIR" "$IPLANE" capacity \
    --service-url "" \
    --provider "$PROVIDERS" \
    --gpu-count "$n" \
    --min-vram-gb "$MIN_VRAM" \
    --output json \
    --timeout "$TIMEOUT" 2>/dev/null)"

  # A failed or empty answer is recorded rather than dropped. A gap in the
  # series would read as "nobody sampled" where the truth is "nobody had
  # anything", and those are the two cases this is built to tell apart.
  if [ -z "$json" ]; then
    json='{"candidates":[],"providers":[],"sample_error":"no output"}'
  fi

  printf '%s' "$json" | python3 -c '
import json, sys, os
ts, width, vram = sys.argv[1], int(sys.argv[2]), int(sys.argv[3])
try:
    d = json.load(sys.stdin)
except Exception as e:
    d = {"candidates": [], "providers": [], "sample_error": str(e)}
cands = d.get("candidates") or []
by = {}
for c in cands:
    p = c.get("provider", "?")
    b = by.setdefault(p, {"n": 0, "min_price": None})
    b["n"] += 1
    pr = c.get("price_usd_per_hour")
    if pr is not None and (b["min_price"] is None or pr < b["min_price"]):
        b["min_price"] = pr
print(json.dumps({
    "ts": ts,
    "gpu_count": width,
    "min_vram_gb": vram,
    "total_candidates": len(cands),
    "by_provider": by,
    "providers": d.get("providers"),
    "candidates": cands,
    "sample_error": d.get("sample_error"),
}, separators=(",", ":")))
' "$ts" "$n" "$MIN_VRAM" >> "$OUT"
  rows=$(( rows + 1 ))
 done
done

echo "capacity-sample: appended $rows rows to $OUT at $ts" >&2
