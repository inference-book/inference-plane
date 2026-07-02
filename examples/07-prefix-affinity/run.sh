#!/usr/bin/env bash
# Demo 07 smoke: prefix-cache affinity on a GPU-free local harness.
#
# Stands up 3 mock engines, registers them as one deployment via the
# external provider (no provisioning), fires multi-turn chat sessions,
# and prints which engine each session pinned to. Under prefix_affinity
# every session sticks to ONE engine; flip the config to round_robin and
# the same sessions scatter across all three.
#
# This is the smoke version (proves the harness + affinity end to end,
# GPU-free). The polished 07a/07b/07c walkthrough + book figures are
# tracked separately (see README "Work needed").
#
# Prereqs: `make build` (bin/iplane). No GPU, no provider keys, no cloud.
#
# Usage:
#   bash examples/07-prefix-affinity/run.sh
#
# Env knobs:
#   IPLANE_ROUTER_ROUTING_POLICY  prefix_affinity (default) | round_robin
#   DEMO_SESSIONS                 concurrent conversations (default 6)
#   DEMO_TURNS                    turns per conversation (default 3)
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
IPLANE="${REPO_ROOT}/bin/iplane"
if [[ ! -x "${IPLANE}" ]]; then
  echo "ERROR: ${IPLANE} not found; run 'make build' first." >&2
  exit 1
fi

POLICY="${IPLANE_ROUTER_ROUTING_POLICY:-prefix_affinity}"
SESSIONS="${DEMO_SESSIONS:-6}"
TURNS="${DEMO_TURNS:-3}"
PORTS=(9001 9002 9003)
STATE_DIR="$(mktemp -d)"
LOG_DIR="$(mktemp -d)"
PIDS=()

cleanup() {
  for pid in "${PIDS[@]:-}"; do kill "${pid}" 2>/dev/null || true; done
  wait 2>/dev/null || true
}
trap cleanup EXIT

echo "== starting ${#PORTS[@]} mock engines =="
ENDPOINTS=""
for p in "${PORTS[@]}"; do
  "${IPLANE}" mock-engine --port "${p}" >"${LOG_DIR}/engine-${p}.log" 2>&1 &
  PIDS+=($!)
  ENDPOINTS="${ENDPOINTS:+${ENDPOINTS},}http://127.0.0.1:${p}"
done

echo "== starting iplane serve (routing_policy=${POLICY}) =="
IPLANE_STATE_DIR="${STATE_DIR}" \
  IPLANE_ROUTER_ROUTING_POLICY="${POLICY}" \
  IPLANE_BACKEND_ENGINE=mock \
  OTEL_EXPORTER_OTLP_ENDPOINT="" \
  "${IPLANE}" serve --config "${REPO_ROOT}/examples/07-prefix-affinity/config.yaml" \
  >"${LOG_DIR}/serve.log" 2>&1 &
PIDS+=($!)

export IPLANE_SERVICE_URL="http://localhost:8080"
for _ in $(seq 1 30); do
  curl -sf -m 2 http://localhost:8080/health >/dev/null 2>&1 && break
  sleep 0.5
done

echo "== registering external deployment over the mock engines =="
"${IPLANE}" deployment deploy affinity-demo \
  --provider external --engine-endpoints "${ENDPOINTS}" --model mock/mock >/dev/null

echo "== firing ${SESSIONS} sessions x ${TURNS} turns =="
"${IPLANE}" load session --target affinity-demo --model mock/mock \
  --sessions "${SESSIONS}" --turns "${TURNS}" --think-time 0 2>&1 | grep -iE "successes|errors" || true

echo
echo "== which engine each session pinned to (policy=${POLICY}) =="
for p in "${PORTS[@]}"; do
  n="$(grep -aoc "session=s-" "${LOG_DIR}/engine-${p}.log" 2>/dev/null || echo 0)"
  ids="$(grep -ao "session=s-[0-9]*" "${LOG_DIR}/engine-${p}.log" 2>/dev/null | sort -u | tr '\n' ' ')"
  echo "  engine ${p}: ${n} requests  [${ids}]"
done
echo
echo "prefix_affinity: each session's requests land on ONE engine."
echo "Re-run with IPLANE_ROUTER_ROUTING_POLICY=round_robin to watch them scatter."
