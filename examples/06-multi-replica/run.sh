#!/usr/bin/env bash
# Demo 06 -- multi-replica throughput curve.
#
# Takes three throughput snapshots against the SAME deployment as
# the replica count grows: 1, 2, 3. Between snapshots, scales the
# deployment up via `iplane deployment scale` and polls the
# describe RPC until the new replicas are healthy.
#
# Under v0.2's router fan-out (Beat 3.3) + scale verb (Beat 3.4),
# the chapter's throughput-curve picture is:
#
#   replicas=1 : actual_rps capped by single-engine service time
#   replicas=2 : roughly 2x
#   replicas=3 : approaches target_rps if the engine can serve it
#
# Prerequisites:
#   - `iplane serve` running (router in the data path).
#   - A running deployment at replicas=1 (run
#     examples/04-router-in-path first to leave one alive).
#   - For the clean throughput picture: engine: vllm. With mock,
#     the per-replica ceiling is tens of thousands of RPS and the
#     scale-up doesn't bind -- the table will look flat.
#
# Usage:
#   bash examples/06-multi-replica/run.sh <deployment-id>
#
# Env knobs:
#   IPLANE_SERVICE_URL    daemon URL (default http://localhost:8080)
#   DEMO_RPS              target rate per snapshot (default 30)
#   DEMO_DURATION         traffic duration per snapshot (default 30s)
#   DEMO_HEALTH_TIMEOUT   seconds to wait for new replicas healthy
#                         after a scale-up (default 300)

set -euo pipefail

DEPLOY_ID="${1:-}"
if [[ -z "${DEPLOY_ID}" ]]; then
  echo "usage: $0 <deployment-id>" >&2
  echo "  hint: run examples/04-router-in-path first to provision a deployment, then pass its id here" >&2
  exit 2
fi

SERVICE_URL="${IPLANE_SERVICE_URL:-http://localhost:8080}"
RPS="${DEMO_RPS:-30}"
DURATION="${DEMO_DURATION:-30s}"
HEALTH_TIMEOUT="${DEMO_HEALTH_TIMEOUT:-300}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
if [[ -x "${REPO_ROOT}/iplane" ]]; then
  IPLANE="${REPO_ROOT}/iplane"
elif command -v iplane >/dev/null 2>&1; then
  IPLANE="iplane"
else
  echo "ERROR: iplane binary not found. Build it first: (cd ${REPO_ROOT} && go build -o iplane ./cmd/iplane)" >&2
  exit 1
fi

if ! command -v jq >/dev/null 2>&1; then
  echo "ERROR: jq is required to read iplane load output. brew install jq (or apt-get install jq)." >&2
  exit 1
fi

if ! curl -fsS -o /dev/null "${SERVICE_URL}/healthz" 2>/dev/null \
   && ! curl -fsS -o /dev/null "${SERVICE_URL}" 2>/dev/null; then
  echo "ERROR: cannot reach ${SERVICE_URL}; is \`iplane serve\` running?" >&2
  exit 1
fi

echo "==============================================================="
echo "Demo 06 -- multi-replica throughput curve"
echo "==============================================================="
echo "  deployment      : ${DEPLOY_ID}"
echo "  service URL     : ${SERVICE_URL}"
echo "  target RPS      : ${RPS}"
echo "  per-snapshot    : ${DURATION}"
echo "  health timeout  : ${HEALTH_TIMEOUT}s"
echo ""
echo "  Watch Grafana panels (uid=inference-plane-v02):"
echo "    - Per-replica utilization (#88) -- should be roughly equal"
echo "    - Router routing-decision metric -- round-robin across replicas"
echo "==============================================================="
echo ""

# Holding-pen for the three snapshots so we can print a comparison
# table at the end.
snapshot_log_1="$(mktemp)"
snapshot_log_2="$(mktemp)"
snapshot_log_3="$(mktemp)"
trap 'rm -f "${snapshot_log_1}" "${snapshot_log_2}" "${snapshot_log_3}"' EXIT

# Poll `iplane deployment describe` until len(instance_ids) -
# len(unhealthy_instance_ids) == expected. `scale --wait` already
# blocks until a terminal aggregate state, but we ALSO confirm the
# healthy count explicitly so the demo is self-documenting (and so a
# partially-failed scale-up is caught before the next snapshot).
wait_healthy() {
  local expected="$1"
  local deadline=$(( $(date +%s) + HEALTH_TIMEOUT ))
  while :; do
    local desc
    desc="$("${IPLANE}" deployment describe "${DEPLOY_ID}" \
      --service-url "${SERVICE_URL}" --output json 2>/dev/null || echo '{}')"
    local total unhealthy healthy
    total=$(echo "${desc}" | jq -r '(.instance_ids // []) | length')
    unhealthy=$(echo "${desc}" | jq -r '(.unhealthy_instance_ids // []) | length')
    healthy=$(( total - unhealthy ))
    if [[ "${healthy}" -ge "${expected}" ]]; then
      echo "  healthy=${healthy}/${expected} (unhealthy=${unhealthy})"
      return 0
    fi
    if [[ $(date +%s) -ge ${deadline} ]]; then
      echo "  ERROR: timed out waiting for healthy=${expected} (last: healthy=${healthy}/${expected}, unhealthy=${unhealthy})" >&2
      return 1
    fi
    sleep 3
  done
}

# Run one `iplane load` snapshot against the deployment. Writes
# the JSON summary to the provided log file. Returns load's exit
# code so the caller can decide whether to continue.
snapshot() {
  local replicas="$1"
  local out="$2"
  echo "--- replicas=${replicas}: ${DURATION} @ ${RPS} rps ---"
  set +e
  "${IPLANE}" load \
    --target "${DEPLOY_ID}" \
    --service-url "${SERVICE_URL}" \
    --rps "${RPS}" \
    --duration "${DURATION}" \
    --max-tokens 60 \
    --chat-fraction 1.0 \
    --output json \
    > "${out}" 2>&1
  local rc=$?
  set -e
  if [[ ${rc} -ne 0 ]]; then
    echo "  WARN: load exited ${rc} (deployment unreachable? mid-scale?)." >&2
    cat "${out}" >&2
    return ${rc}
  fi
  # Inline read of the headline numbers; full JSON kept in the log
  # for the end-of-run table.
  local actual target ratio p95
  actual=$(jq -r '.actual_rps' < "${out}")
  target=$(jq -r '.target_rps' < "${out}")
  p95=$(jq -r '.latency_p95_ms' < "${out}")
  ratio=$(awk -v a="${actual}" -v t="${target}" 'BEGIN { if (t > 0) printf "%.2f", a/t; else print "n/a" }')
  printf "  actual_rps=%.1f  target_rps=%.1f  ratio=%s  p95=%sms\n" "${actual}" "${target}" "${ratio}" "${p95}"
}

# === Baseline (replicas=1) ===
echo "[1/3] snapshot @ replicas=1"
snapshot 1 "${snapshot_log_1}"
echo ""

# === Scale to 2, poll healthy, snapshot ===
echo "[2/3] scaling to 2 replicas ..."
"${IPLANE}" deployment scale "${DEPLOY_ID}" 2 \
  --service-url "${SERVICE_URL}" --wait
wait_healthy 2
echo "[2/3] snapshot @ replicas=2"
snapshot 2 "${snapshot_log_2}"
echo ""

# === Scale to 3, poll healthy, snapshot ===
echo "[3/3] scaling to 3 replicas ..."
"${IPLANE}" deployment scale "${DEPLOY_ID}" 3 \
  --service-url "${SERVICE_URL}" --wait
wait_healthy 3
echo "[3/3] snapshot @ replicas=3"
snapshot 3 "${snapshot_log_3}"
echo ""

# === Summary table + saturation hint ===
echo "==============================================================="
echo "Throughput summary"
echo "==============================================================="
printf "%-10s %12s %12s %10s %14s\n" "Replicas" "actual_rps" "target_rps" "ratio" "latency_p95ms"
for n in 1 2 3; do
  log_var="snapshot_log_${n}"
  log="${!log_var}"
  actual=$(jq -r '.actual_rps // 0'    < "${log}")
  target=$(jq -r '.target_rps // 0'    < "${log}")
  p95=$(jq -r    '.latency_p95_ms // 0' < "${log}")
  ratio=$(awk -v a="${actual}" -v t="${target}" 'BEGIN { if (t > 0) printf "%.2f", a/t; else print "n/a" }')
  printf "%-10s %12.1f %12.1f %10s %14s\n" "${n}" "${actual}" "${target}" "${ratio}" "${p95}"
done

# Saturation hint: at replicas=1, ratio close to 1.0 means the
# baseline isn't pressing the engine -- the scale-up won't reveal
# headroom because there's no headroom to reveal. Threshold 0.85
# is a soft signal; the chapter narrative wants the baseline at
# 0.4-0.7.
base_actual=$(jq -r '.actual_rps' < "${snapshot_log_1}")
base_target=$(jq -r '.target_rps' < "${snapshot_log_1}")
base_ratio=$(awk -v a="${base_actual}" -v t="${base_target}" 'BEGIN { if (t > 0) print a/t; else print 0 }')
above_threshold=$(awk -v r="${base_ratio}" 'BEGIN { print (r >= 0.95) ? "yes" : "no" }')
echo ""
if [[ "${above_threshold}" == "yes" ]]; then
  echo "NOTE: replicas=1 ratio = $(printf '%.2f' "${base_ratio}") -- one replica is NOT saturated at DEMO_RPS=${RPS}."
  echo "      The scale-up won't reveal much headroom because the engine has spare capacity at the baseline."
  echo "      Re-run with a higher rate to see the chapter's plateau-vs-linear shape:"
  echo "          DEMO_RPS=$(( RPS * 3 )) bash $0 ${DEPLOY_ID}"
fi

echo ""
echo "Done. The deployment is left at replicas=3 -- scale back down"
echo "via 'iplane deployment scale ${DEPLOY_ID} 1 --wait' if you're"
echo "running on a paid provider."
