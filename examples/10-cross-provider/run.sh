#!/usr/bin/env bash
# Demo 10 walkthrough: capacity across providers, six acts.
#
#   10a  one catalog, then several  -- the widened search
#   10b  silence is not evidence    -- the unvouched row that vanishes
#   10c  the cheap ticket           -- who actually sells reclaimable capacity
#   10d  ranking becomes a decision -- cheapest-fit, with its reasoning
#   10e  moving without dropping    -- migrate, priced before committing
#   10f  draining on purpose        -- the release half that already worked
#
# Nothing here rents anything. Acts 10a-10d are read-only vendor catalog
# queries, which is what makes this chapter reproducible where Chapter 10's
# fabric A/B was not: that one needed two paid pools, this one needs an API
# key and no balance. Acts 10e-10f run against `iplane mock-engine` through
# the external provider, so they need no GPU either.
#
# What it will NOT do is stage a preemption. There is no reclaim trigger to
# fire (see the closing note), and a demo that destroys its own instance on a
# timer would teach the drain while implying a signal that does not exist.
#
# Prereqs:
#   - `make build` at the repo root (bin/iplane)
#   - at least one provider API key; a free Vast.ai account is enough and is
#     the vendor that makes 10b and 10c interesting
#
# Usage:
#   bash examples/10-cross-provider/run.sh
#
# Env knobs:
#   DEMO_CLASS   gpu class to shop for  (default large)
#   DEMO_GPUS    cards per instance     (default 2)
#   DEMO_LIMIT   rows per table         (default 4)
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
IPLANE="${REPO_ROOT}/bin/iplane"
CONFIG="${REPO_ROOT}/examples/10-cross-provider/config.yaml"
SERVICE="http://localhost:18110"
CLASS="${DEMO_CLASS:-large}"
# Two cards, not one, and every act uses the same figure. A fabric describes
# how cards reach each other, so the service refuses a fabric requirement
# below two; asking for one card in 10a and two in 10b would also make that
# act's "compare the two tables" instruction a comparison of two questions.
GPUS="${DEMO_GPUS:-2}"
LIMIT="${DEMO_LIMIT:-4}"
ENGINE_PORT=9110

if [[ ! -x "${IPLANE}" ]]; then
  echo "ERROR: ${IPLANE} not found; run 'make build' first." >&2
  exit 1
fi

# Fail fast rather than skipping acts. This demo is about what real vendors
# publish and how they disagree, so a run with no vendor in it would print a
# shape without any of the content, which is worse than not running.
if [[ -z "${VAST_API_KEY:-}${RUNPOD_API_KEY:-}${LAMBDA_API_KEY:-}" ]]; then
  cat >&2 <<'SETUP'
ERROR: this walkthrough needs at least one provider API key.

  export VAST_API_KEY=...       # free account, no balance needed for 10a-10d
  export RUNPOD_API_KEY=...
  export LAMBDA_API_KEY=...

Any one of them runs the whole script. Vast is the one worth having: it is
the only vendor of the three that publishes a measured interconnect reading
(act 10b) or a reclaimable price (act 10c), so with only the other two those
acts run and show the absence rather than the effect.

Nothing here rents anything. Acts 10a-10d are read-only catalog queries.
SETUP
  exit 1
fi

LOG_DIR="$(mktemp -d)"
STATE_DIR="$(mktemp -d)"
SERVE_PID=""
ENGINE_PID=""

cleanup() {
  for pid in "${ENGINE_PID}" "${SERVE_PID}"; do
    [[ -n "${pid}" ]] && kill "${pid}" 2>/dev/null || true
  done
  wait 2>/dev/null || true
  rm -rf "${LOG_DIR}" "${STATE_DIR}"
}
trap cleanup EXIT

capacity() { "${IPLANE}" capacity "$@"; }

banner() {
  echo
  echo "=============================================================="
  echo "== $1"
  echo "=============================================================="
}

# ---------------------------------------------------------------------------
banner "10a  one catalog, then several"

echo "\$ iplane capacity --provider vast --class ${CLASS} --gpu-count ${GPUS} --limit ${LIMIT}"
capacity --provider vast --class "${CLASS}" --gpu-count "${GPUS}" --limit "${LIMIT}" || true

cat <<'NOTE'

  One vendor, ranked by price. This is what the resolver could always do.

NOTE

echo "\$ iplane capacity --all --class ${CLASS} --gpu-count ${GPUS} --limit ${LIMIT}"
capacity --all --class "${CLASS}" --gpu-count "${GPUS}" --limit "${LIMIT}"

cat <<'NOTE'

  The same requirement against every configured vendor. Two things in that
  footer are doing work that the table cannot.

  The per-provider lines separate three outcomes an empty result would have
  collapsed. "cannot answer" is a fact about a vendor's API and sends you to
  another source. "answered, no capacity" is a fact about the market and is
  the only one of the three that says anything about supply.

  The comparability lines name what the ranking could NOT weigh. A blank in
  those columns is unmeasured rather than zero, and the ordering above did
  not use them.
NOTE

# ---------------------------------------------------------------------------
banner "10b  silence is not evidence"

echo "\$ iplane capacity --all --class ${CLASS} --gpu-count ${GPUS} --limit ${LIMIT}                  # (above)"
echo "\$ iplane capacity --all --class ${CLASS} --gpu-count ${GPUS} --limit ${LIMIT} --fabric intra-node"
capacity --all --class "${CLASS}" --gpu-count "${GPUS}" --limit "${LIMIT}" --fabric intra-node

cat <<'NOTE'

  Compare the two tables. Rows reading "unknown (host reports no reading)"
  are gone, and on a live marketplace one of them is usually cheaper than
  the cheapest survivor.

  Those hosts were not rejected for lacking the interconnect. They were
  rejected for not saying. A vendor that publishes nothing must not win a
  requirement by default, because the alternative is renting a pool to find
  out, and finding out costs the whole bill either way.

  Note which vendor keeps rows here. Only one of the three measures the
  fabric per host; the others report the tier their catalog declares.
NOTE

# ---------------------------------------------------------------------------
banner "10c  the cheap ticket, and who actually sells it"

echo "\$ iplane capacity --all --class ${CLASS} --gpu-count ${GPUS} --limit ${LIMIT} --reclaim yes"
capacity --all --class "${CLASS}" --gpu-count "${GPUS}" --limit "${LIMIT}" --reclaim yes

cat <<'NOTE'

  Reclaimable capacity is the same hardware at a discount, in exchange for
  the vendor being allowed to take it back.

  Read the footer before the prices. Of three vendors, one sells this. One
  has no such tier anywhere in its API. One exposes a bid price that reads
  exactly like a spot rate and was equal to its on-demand rate on every
  available shape when this was written, so iplane drops those rather than
  reporting a discount that does not exist.

  That is the chapter's opening pattern one level down. Shopping several
  vendors is about availability before it is about price, and what varies
  between them is not only which cards are free this afternoon but which
  commercial arrangements exist at all.
NOTE

# ---------------------------------------------------------------------------
banner "10d  ranking becomes a decision"

echo "\$ iplane instance create auto demo-10 --class ${CLASS} --gpu-count ${GPUS} --dry-run"
"${IPLANE}" instance create auto demo-10 --class "${CLASS}" --gpu-count "${GPUS}" --dry-run || true

cat <<'NOTE'

  Same search, one step further. The resolver picks instead of printing.

  Everything after the winning line exists so the choice can be argued
  with: what came second and by how much, how many candidates there were,
  any vendor that was unreachable and therefore did not compete, and the
  facts the ranking could not compare and so did not weigh.

  Cheapest is not best, and the gap is the point. A card can be cheapest
  because it sits where your weights are not staged, and then the cold
  start costs more than the hourly saving ever returns. Nothing here
  weighs that, so it says so rather than implying it did.
NOTE

# ---------------------------------------------------------------------------
banner "10e  moving without dropping anything"

echo "== starting a control plane and one mock engine =="
IPLANE_STATE_DIR="${STATE_DIR}" IPLANE_BACKEND_ENGINE=mock OTEL_EXPORTER_OTLP_ENDPOINT="" \
  "${IPLANE}" serve --config "${CONFIG}" --state-dir "${STATE_DIR}" \
  --server-addr :18110 >"${LOG_DIR}/serve.log" 2>&1 &
SERVE_PID=$!
for _ in $(seq 1 40); do
  curl -sf -m 2 "${SERVICE}/healthz" >/dev/null 2>&1 && break
  sleep 0.5
done

# Registered as well as reachable. The deployment below routes to it; the
# registration is what makes it a fleet member, which is what act 10f drains.
# Same engine, two views of it.
"${IPLANE}" mock-engine --port "${ENGINE_PORT}" --register "${SERVICE}" \
  --engine-id demo-10-engine --model mock/mock --latency 3ms \
  >"${LOG_DIR}/engine.log" 2>&1 &
ENGINE_PID=$!
sleep 2

"${IPLANE}" deployment deploy demo-10-dep \
  --provider external --engine-endpoints "http://127.0.0.1:${ENGINE_PORT}" \
  --model mock/mock --service-url "${SERVICE}" >"${LOG_DIR}/deploy.log" 2>&1 || true

echo
echo "\$ iplane deployment migrate demo-10-dep --to runpod --region US-TX-3 --dry-run"
"${IPLANE}" deployment migrate demo-10-dep --to runpod --region US-TX-3 \
  --dry-run --service-url "${SERVICE}" || true

cat <<'NOTE'

  A planned move, priced before anything is spent.

  The warning is the useful part. Weights are staged per vendor and per
  region, so moving somewhere they are not pinned means downloading them
  again, which on a large model is minutes rather than seconds. An operator
  who was not told reads a stalled deploy as a hang.

  The ordering matters more than the warning. A migration grows onto the
  destination, waits for it to serve, and only then drains the source. That
  sequence is available because nothing is being taken away: a migration has
  no deadline. A reclaim does not get it, which is why the reclaim path in
  this chapter is still a drain with no trigger.
NOTE

# ---------------------------------------------------------------------------
banner "10f  the ownership boundary"

echo "\$ iplane fleet status --service-url ${SERVICE}"
"${IPLANE}" fleet status --service-url "${SERVICE}" || true

echo
echo "\$ iplane fleet drain demo-10-engine --timeout 5s --service-url ${SERVICE}"
"${IPLANE}" fleet drain demo-10-engine --timeout 5s --service-url "${SERVICE}" || true

cat <<'NOTE'

  The engine is visible and it will not be drained, and both halves of that
  are correct.

  It is visible because it registered itself over the control channel, which
  is all a fleet member is: one engine, one endpoint, one model, reporting
  what it can see about its own hardware.

  It will not be drained because iplane did not rent it. Draining releases
  hardware, and this engine is attached rather than owned, so there is no
  rental to release and no provider to tell. The same rule makes the external
  provider's Terminate detach instead of destroy. A control plane that
  released things it did not acquire would be a worse tool than one that
  refuses and says why.

  So the drain machinery is real and this demo cannot show it working. It
  needs a member iplane provisioned, which means renting something. What you
  can see here is the boundary that decides who is allowed to release what.
NOTE

# ---------------------------------------------------------------------------
banner "what this demo did not show"

cat <<'NOTE'

  A preemption. Nothing listens for a vendor signalling that it is about to
  take hardware back, and nothing infers one from a member that stopped
  answering. The drain above is the mechanism; the trigger does not exist.

  Staging one would be easy and dishonest. Destroying our own instance on a
  timer would produce a convincing recording of a capability iplane does not
  have, and the reader would only discover it when a real vendor reclaimed
  something and nothing happened.

  A completed migration. Act 10e plans one and stops. Actually moving a
  deployment between vendors needs two rentals and about twenty minutes, so
  it is a paid exercise rather than a walkthrough step. The planning, the
  warm-versus-cold check and the failure case are all verified; that a
  successful grow-then-drain ends with exactly the destination serving is
  not.

  A per-token price. iplane can route to a hosted API and cannot yet price
  one, so an endpoint billed per token still cannot be compared against a
  pool billed per hour. That comparison is the hard part and it is open.

  A completed drain. Act 10f shows the refusal rather than the release,
  because releasing hardware needs hardware that iplane rented. Every other
  act here is free, and that one is not, so the walkthrough stops at the
  boundary rather than spending to cross it.
NOTE

echo
echo "done. nothing was rented."
