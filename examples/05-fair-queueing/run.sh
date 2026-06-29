#!/usr/bin/env bash
# Demo 05 — fair-queueing: interactive cuts ahead of batch.
#
# Spawns two parallel `iplane load` processes against the SAME
# deployment, with different priority classes:
#
#   alice  | interactive | 5 rps
#   bob    | batch       | 20 rps
#
# Under v0.2's strict-priority scheduler (Beat 2.4), the
# interactive lane is preferred -- alice's p95 latency stays
# bounded while bob's batch queue grows. Watch the v0.2 Grafana
# dashboard's "Queue depth" and "Queue wait p95 (per class)"
# panels to see the effect live.
#
# Prerequisites:
#   - `iplane serve` running with the scheduler enabled. Minimum:
#       router.queue.servicers: 2
#       router.queue.capacity: 256
#     (See deploy/config.yaml for the full template.)
#   - A RUNNING deployment exists. Easiest: run examples/04-router-in-path
#     first (it leaves the deployment alive by default).
#   - Local observability stack (`make infra-up`) so the Grafana
#     panels populate.
#
# Usage:
#   bash examples/05-fair-queueing/run.sh <deployment-id>
#
# Optional env knobs:
#   IPLANE_SERVICE_URL   daemon URL (default http://localhost:8080)
#   DEMO_DURATION        traffic duration (default 60s)
#   DEMO_INTERACTIVE_RPS interactive client rate (default 5)
#   DEMO_BATCH_RPS       batch client rate (default 20)

set -euo pipefail

DEPLOY_ID="${1:-}"
if [[ -z "$DEPLOY_ID" ]]; then
  echo "usage: $0 <deployment-id>" >&2
  echo "  hint: run examples/04-router-in-path first to provision a deployment, then pass its id here" >&2
  exit 2
fi

SERVICE_URL="${IPLANE_SERVICE_URL:-http://localhost:8080}"
DURATION="${DEMO_DURATION:-60s}"
INTERACTIVE_RPS="${DEMO_INTERACTIVE_RPS:-5}"
BATCH_RPS="${DEMO_BATCH_RPS:-20}"

# Locate the iplane binary. Prefer a colocated binary built via
# `go build ./cmd/iplane` at the repo root; fall back to PATH.
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

# Sanity-check the daemon is up.
if ! curl -fsS -o /dev/null "${SERVICE_URL}/healthz" 2>/dev/null; then
  if ! curl -fsS -o /dev/null "${SERVICE_URL}" 2>/dev/null; then
    echo "ERROR: cannot reach ${SERVICE_URL}; is \`iplane serve\` running?" >&2
    exit 1
  fi
fi

# Resolve the deployment's model from the daemon. `iplane load --model`
# is required (no default); the engine validates that the body's `model`
# field matches what the pod is serving, so we query the source of truth
# instead of asking the operator to retype it.
if ! command -v jq >/dev/null 2>&1; then
  echo "ERROR: jq is required to resolve the deployment model. brew install jq (or apt-get install jq)." >&2
  exit 1
fi
DEMO_MODEL="$("${IPLANE}" deployment describe "${DEPLOY_ID}" \
  --service-url "${SERVICE_URL}" --output json 2>/dev/null \
  | jq -r '.model // empty')"
if [[ -z "${DEMO_MODEL}" ]]; then
  echo "ERROR: could not resolve model for deployment ${DEPLOY_ID}; is the id correct?" >&2
  echo "  hint: iplane deployment list --service-url ${SERVICE_URL}" >&2
  exit 1
fi

echo "==============================================================="
echo "Demo 05 — fair-queueing (interactive cuts ahead of batch)"
echo "==============================================================="
echo "  deployment    : ${DEPLOY_ID}"
echo "  model         : ${DEMO_MODEL}"
echo "  service URL   : ${SERVICE_URL}"
echo "  duration      : ${DURATION}"
echo "  alice (int)   : ${INTERACTIVE_RPS} rps, priority=interactive"
echo "  bob   (batch) : ${BATCH_RPS} rps, priority=batch"
echo ""
echo "  Watch Grafana panels (uid=inference-plane-v02):"
echo "    - Queue depth (per deploy_id / tenant_id / class)"
echo "    - Queue wait p95 (per class)"
echo ""
echo "  Walking a trace? Filter Tempo by service.name=iplane,"
echo "  attribute iplane.router.priority=batch. Open one trace and"
echo "  read iplane.queue.wait_ms on the router span -- the queue"
echo "  story is right there."
echo "==============================================================="
echo ""

# Capture exit codes; we want to see both clients' stats even if one
# errors out (the daemon being misconfigured shouldn't hide the other
# half of the demo).
alice_log="$(mktemp)"
bob_log="$(mktemp)"
trap 'rm -f "${alice_log}" "${bob_log}"' EXIT

# Fire both clients in parallel. --target uses --service-url to
# construct the deploy-id URL, so all traffic lands on the same
# deployment and the router lanes it by the priority header.
"${IPLANE}" load \
  --target "${DEPLOY_ID}" \
  --service-url "${SERVICE_URL}" \
  --model "${DEMO_MODEL}" \
  --rps "${INTERACTIVE_RPS}" \
  --duration "${DURATION}" \
  --priority interactive \
  --tenant alice \
  --max-tokens 60 \
  --chat-fraction 1.0 \
  --output json \
  > "${alice_log}" 2>&1 &
alice_pid=$!

"${IPLANE}" load \
  --target "${DEPLOY_ID}" \
  --service-url "${SERVICE_URL}" \
  --model "${DEMO_MODEL}" \
  --rps "${BATCH_RPS}" \
  --duration "${DURATION}" \
  --priority batch \
  --tenant bob \
  --max-tokens 60 \
  --chat-fraction 1.0 \
  --output json \
  > "${bob_log}" 2>&1 &
bob_pid=$!

# Wait for both. -- doesn't fail the script if a client returns
# non-zero (just record it).
set +e
wait "${alice_pid}"; alice_rc=$?
wait "${bob_pid}";   bob_rc=$?
set -e

echo ""
echo "=== alice (interactive) ==="
cat "${alice_log}"
echo ""
echo "=== bob (batch) ==="
cat "${bob_log}"
echo ""

if [[ ${alice_rc} -ne 0 || ${bob_rc} -ne 0 ]]; then
  echo "WARN: at least one client returned non-zero (alice=${alice_rc} bob=${bob_rc}). Check the iplane serve log for errors." >&2
fi

echo "Done. Compare alice's p95 latency to bob's -- the chapter's"
echo "fair-queueing story is the gap between them under sustained"
echo "saturation."
