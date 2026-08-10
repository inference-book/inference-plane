#!/usr/bin/env bash
# Demo 09 walkthrough: tracking a distributed data plane, GPU-free, four acts.
#
#   09a  the assembling window  -- a group exists before it serves
#   09b  the span column        -- 4c/1n and 1c/1n side by side
#   09c  degraded, not dead     -- correct tokens, link down
#   09d  the lease expires      -- silence is how the control plane learns
#
# Everything runs on `iplane mock-engine --register`, so it needs no GPU, no
# provider keys, and no cloud. What it shows is the CONTROL CHANNEL: what an
# engine reports about itself and what the fleet view can therefore say. The
# NVLink-vs-PCIe throughput figures for the book come from a real paid run --
# see the README "Capturing the book figures" section.
#
# The span here is fabricated (mock-engine invents four cards on one node)
# because a real four-card group costs money and this demo does not. The
# STATES are not fabricated: they are produced by the same agent code that
# runs on a rented box, through the same registration path.
#
# Prereqs: `make build` (bin/iplane).
#
# Usage:
#   bash examples/09-multi-gpu/run.sh
#
# Env knobs:
#   DEMO_ASSEMBLE   how long the group takes to form (default 8s)
#   DEMO_DEGRADE    how long until its link drops (default 24s)
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
IPLANE="${REPO_ROOT}/bin/iplane"
CONFIG="${REPO_ROOT}/examples/09-multi-gpu/config.yaml"
SERVICE="http://localhost:8080"
if [[ ! -x "${IPLANE}" ]]; then
  echo "ERROR: ${IPLANE} not found; run 'make build' first." >&2
  exit 1
fi

ASSEMBLE="${DEMO_ASSEMBLE:-8s}"
DEGRADE="${DEMO_DEGRADE:-24s}"
GROUP_PORT=9101
SINGLE_PORT=9102
LOG_DIR="$(mktemp -d)"
STATE_DIR="$(mktemp -d)"
SERVE_PID=""
GROUP_PID=""
SINGLE_PID=""

cleanup() {
  for pid in "${GROUP_PID}" "${SINGLE_PID}" "${SERVE_PID}"; do
    [[ -n "${pid}" ]] && kill "${pid}" 2>/dev/null || true
  done
  wait 2>/dev/null || true
}
trap cleanup EXIT

fleet() { "${IPLANE}" fleet status --service-url "${SERVICE}" "$@"; }

# wait_for <member> <state-regex> <timeout-seconds>
#
# Polls rather than sleeping a fixed amount, so the demo still reads
# correctly on a slow machine. Matches the whole row because the degraded
# label contains a space and a comma, which defeats column splitting.
wait_for() {
  local member="$1" want="$2" limit="$3" n=0
  while (( n < limit )); do
    if fleet --show-lost 2>/dev/null | grep -qE "^${member}[[:space:]].*${want}"; then
      return 0
    fi
    sleep 1
    n=$((n + 1))
  done
  echo "ERROR: ${member} never reached '${want}' (waited ${limit}s)" >&2
  fleet --show-lost >&2 || true
  return 1
}

banner() {
  echo
  echo "=============================================================="
  echo "== $1"
  echo "=============================================================="
}

echo "== starting the control plane =="
IPLANE_STATE_DIR="${STATE_DIR}" \
  IPLANE_BACKEND_ENGINE=mock \
  OTEL_EXPORTER_OTLP_ENDPOINT="" \
  "${IPLANE}" serve --config "${CONFIG}" >"${LOG_DIR}/serve.log" 2>&1 &
SERVE_PID=$!
for _ in $(seq 1 30); do
  curl -sf -m 2 "${SERVICE}/health" >/dev/null 2>&1 && break
  sleep 0.5
done

echo "== starting two engines: a four-card group and a single card =="
# The group takes time to form and later loses a link. The single card is
# the control: it is up immediately and stays healthy, so every difference
# you see below belongs to the group rather than to the demo.
"${IPLANE}" mock-engine --port "${GROUP_PORT}" --register "${SERVICE}" \
  --engine-id tp4-group --model mock/mock --latency 3ms \
  --span-nodes 1 --span-cards 4 \
  --assemble-delay "${ASSEMBLE}" --degrade-after "${DEGRADE}" \
  >"${LOG_DIR}/group.log" 2>&1 &
GROUP_PID=$!

"${IPLANE}" mock-engine --port "${SINGLE_PORT}" --register "${SERVICE}" \
  --engine-id single-card --model mock/mock --latency 3ms --span-cards 1 \
  >"${LOG_DIR}/single.log" 2>&1 &
SINGLE_PID=$!

banner "09a  the assembling window -- a group exists before it serves"
wait_for tp4-group assembling 30
fleet
cat <<'NOTE'

  tp4-group is ASSEMBLING: its processes are alive and the group has not
  formed. No health poller can see this state, because during assembly
  there is no endpoint for anything outside the box to connect to. The
  agent is inside, so it can watch the engine come up and say so.
NOTE

banner "09b  the span column -- 4c/1n and 1c/1n, one list"
wait_for tp4-group serving 60
fleet
cat <<'NOTE'

  Both members are serving, and they are the same KIND of thing: one
  endpoint, one model. The span column is the only difference. That is
  the promise Chapter 8 made and this chapter keeps -- a distributed
  engine did not become a new object type with its own verbs.
NOTE

banner "09c  degraded, not dead -- correct tokens, link down"
wait_for tp4-group "serving, link down" 60
fleet
echo
echo "  ...and it is still answering:"
curl -s -m 10 "http://127.0.0.1:${GROUP_PORT}/v1/chat/completions" \
  -H 'Content-Type: application/json' \
  -d '{"model":"mock/mock","messages":[{"role":"user","content":"still there?"}]}' \
  | grep -o '"completion_tokens":[0-9]*' \
  | sed 's/^/    /' || echo "    (no reply)"
cat <<'NOTE'

  Read that pair together. The engine returns correct tokens and the
  fleet view says its link is down. Both are true. /health would say
  "serving", which is also true and useless, because a degraded group
  IS serving -- just at a fraction of the hardware you are paying for.
  This state has no single-card equivalent, which is why the vocabulary
  had to grow rather than being reused.
NOTE

banner "09d  the lease expires -- silence is the signal"
echo "  killing tp4-group's process (no goodbye, the way a real crash goes)"
# disown first so bash does not print its own "Killed: 9" job notice over
# the demo output. SIGKILL rather than SIGTERM on purpose: a crashed engine
# does not get to run a shutdown handler, and the whole point of the act is
# that the control plane learns from silence rather than from a farewell.
disown "${GROUP_PID}" 2>/dev/null || true
kill -9 "${GROUP_PID}" 2>/dev/null || true
GROUP_PID=""
echo "  the member keeps reading as serving until its lease runs out..."
sleep 5
fleet
echo
echo "  ...waiting out the lease (30s) plus a sweep (10s):"
wait_for tp4-group lost 75
fleet --show-lost
cat <<'NOTE'

  Killed at the start of this act, the member went on reading as
  degraded-but-serving until its lease ran out. That is correct rather
  than sloppy: the fleet view is not a probe of what is alive right
  now, it is a record of what last announced itself plus a deadline.
  The delay is bounded by a number the control plane handed out, not
  by an unknown network timeout -- which is the whole argument for a
  lease over a held-open connection.

  Note the default view hides it. `fleet status` shows the living;
  `--show-lost` shows what went away, because a deleted row and one
  that never registered look identical.
NOTE

banner "what this demo did not show"
cat <<'NOTE'
  The span above is fabricated: mock-engine invents four cards on one
  node so the column has something to render. The STATES are real --
  same agent code, same registration path as a rented box.

  Two things need real hardware and real money:
    * the NVLink-vs-PCIe throughput delta (the chapter's A/B)
    * link health read from actual NVLink counters rather than a timer
  See the README, and issues 215 and 213.
NOTE
