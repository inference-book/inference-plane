#!/usr/bin/env bash
# Demo 07 walkthrough: prefix-cache affinity, GPU-free, three acts.
#
#   07a  round_robin      -- a session's turns scatter across engines
#   07b  prefix_affinity  -- each session pins to ONE engine
#   07c  + overload cap    -- a whale session spills instead of monopolizing
#
# Everything runs on the mock engine (iplane mock-engine + the external
# provider), so it needs no GPU, no provider keys, and no cloud. It shows
# the ROUTING behavior (which engine each session lands on). The latency /
# cache-eviction figures for the book come from a real multi-replica run
# -- see the README "Capturing the book figures" section.
#
# Prereqs: `make build` (bin/iplane).
#
# Usage:
#   bash examples/07-prefix-affinity/run.sh
#
# Env knobs:
#   DEMO_SESSIONS   concurrent conversations (default 9)
#   DEMO_TURNS      turns per conversation (default 4)
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
IPLANE="${REPO_ROOT}/bin/iplane"
CONFIG="${REPO_ROOT}/examples/07-prefix-affinity/config.yaml"
if [[ ! -x "${IPLANE}" ]]; then
  echo "ERROR: ${IPLANE} not found; run 'make build' first." >&2
  exit 1
fi

SESSIONS="${DEMO_SESSIONS:-9}"
TURNS="${DEMO_TURNS:-4}"
PORTS=(9001 9002 9003)
LOG_DIR="$(mktemp -d)"
ENGINE_PIDS=()
SERVE_PID=""

cleanup() {
  [[ -n "${SERVE_PID}" ]] && kill "${SERVE_PID}" 2>/dev/null || true
  for pid in "${ENGINE_PIDS[@]:-}"; do kill "${pid}" 2>/dev/null || true; done
  wait 2>/dev/null || true
}
trap cleanup EXIT

echo "== starting ${#PORTS[@]} mock engines =="
ENDPOINTS=""
for p in "${PORTS[@]}"; do
  "${IPLANE}" mock-engine --port "${p}" --latency 3ms >"${LOG_DIR}/engine-${p}.log" 2>&1 &
  ENGINE_PIDS+=($!)
  ENDPOINTS="${ENDPOINTS:+${ENDPOINTS},}http://127.0.0.1:${p}"
done

# run_act <label> <routing_policy> <overload_threshold> <sessions>
run_act() {
  local label="$1" policy="$2" threshold="$3" sessions="$4"
  echo
  echo "=============================================================="
  echo "== ${label}"
  echo "=============================================================="

  # Each act needs its own serve (routing_policy is fixed at startup) on
  # the same :8080 / :9090 (gRPC port is hardcoded), so fully tear the
  # previous one down and let the ports release before rebinding.
  if [[ -n "${SERVE_PID}" ]]; then
    kill -9 "${SERVE_PID}" 2>/dev/null || true
    wait "${SERVE_PID}" 2>/dev/null || true
    sleep 2
  fi
  local state; state="$(mktemp -d)"
  IPLANE_STATE_DIR="${state}" \
    IPLANE_ROUTER_ROUTING_POLICY="${policy}" \
    IPLANE_ROUTER_AFFINITY_OVERLOAD_THRESHOLD="${threshold}" \
    IPLANE_BACKEND_ENGINE=mock \
    OTEL_EXPORTER_OTLP_ENDPOINT="" \
    "${IPLANE}" serve --config "${CONFIG}" >"${LOG_DIR}/serve.log" 2>&1 &
  SERVE_PID=$!

  local ok=""
  for _ in $(seq 1 30); do
    if curl -sf -m 2 http://localhost:8080/health >/dev/null 2>&1; then ok=1; break; fi
    sleep 0.5
  done
  [[ -z "${ok}" ]] && { echo "ERROR: serve didn't come up; see ${LOG_DIR}/serve.log" >&2; exit 1; }

  IPLANE_SERVICE_URL=http://localhost:8080 "${IPLANE}" deployment deploy affinity-demo \
    --provider external --engine-endpoints "${ENDPOINTS}" --model mock/mock >/dev/null

  # clear engine logs so this act's routing is measured cleanly
  for p in "${PORTS[@]}"; do : > "${LOG_DIR}/engine-${p}.log"; done

  IPLANE_SERVICE_URL=http://localhost:8080 "${IPLANE}" load session --target affinity-demo \
    --model mock/mock --sessions "${sessions}" --turns "${TURNS}" --think-time 0 \
    2>&1 | grep -iE "successes|errors" || true

  # (engine, session) pairs this act, then the scatter score.
  local pairs
  pairs="$(for p in "${PORTS[@]}"; do
    grep -ao "session=s-[0-9]*" "${LOG_DIR}/engine-${p}.log" | sort -u | sed "s/^/${p} /"
  done)"
  echo "  per-engine sessions:"
  for p in "${PORTS[@]}"; do
    ids="$(echo "${pairs}" | awk -v e="${p}" '$1==e {printf "%s ", $2}')"
    echo "    engine ${p}: ${ids}"
  done
  echo "${pairs}" | awk '
    { engines[$2]++ }
    END {
      total=0; scattered=0
      for (s in engines) { total++; if (engines[s] > 1) scattered++ }
      printf "  scatter: %d of %d sessions touched more than one engine\n", scattered, total
    }'
}

run_act "07a  round_robin -- sessions scatter (cache-defeating)" round_robin 0 "${SESSIONS}"
run_act "07b  prefix_affinity -- each session pins to one engine" prefix_affinity 0 "${SESSIONS}"
run_act "07c  prefix_affinity + overload cap 2 -- affinity yields to load when replicas saturate" prefix_affinity 2 "${SESSIONS}"

cat <<'NOTE'

==============================================================
Read the scatter lines: round_robin scatters most sessions across
engines (scatter high -- prefix cache misses); prefix_affinity pins
each session to one engine (scatter 0); adding the overload cap makes
replicas that hit the cap shed load, so scatter climbs back up. That
is affinity yielding to load-balancing under saturation -- the
"affinity is not free" trade-off, and the safety valve that stops one
replica from melting for the sake of cache locality. The same override
protects against a single whale session; showing that distinctly needs
asymmetric per-session load the driver doesn't generate yet (tracked as
a follow-up).

This shows ROUTING only. The engine-side prefix-cache hit-rate and
the latency win need a real multi-replica run -- see the README
"Capturing the book figures" section.
NOTE
