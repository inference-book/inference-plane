#!/usr/bin/env bash
# Demo 08a -- cold-start distance: a warm deploy vs a cold deploy.
#
# The chapter's Figure 9.7 on one time axis. Same model, same region,
# two deploys:
#
#   COLD  no pinned volume -- the engine downloads the weights from
#         Hugging Face inside the pod. At 32B the download dominates the
#         cold start. The deploy is tagged storage_tier=cold.
#   WARM  the weights are pre-staged onto a network volume with
#         `iplane model pin`, so the deploy mounts them instead of
#         downloading. The engine:init phase collapses. storage_tier=warm.
#
# The arc mirrors what an operator actually does: deploy cold, feel the
# download, pin the model, redeploy warm. This script owns the full
# lifecycle because `iplane model pin` runs in-process and needs the
# state-dir lock that `iplane serve` holds for its lifetime -- so the pin
# happens between two serve sessions, not against a running daemon.
#
# What you observe afterward (control-plane telemetry, not the engine):
#   - Tempo: two `deployment.provision` traces. The cold one has a fat
#     engine:init child span; the warm one's engine:init is a sliver.
#   - Grafana "Inference Plane Deployment & Lifecycle" dashboard, the
#     "Engine-init: warm vs cold" panel -- the storage_tier split.
#
# TEARDOWN: every GPU pod this script creates is destroyed on exit (the
# two deploys; the CPU staging pod from `pin` self-terminates). The
# pinned network *volume* persists on purpose -- it is the reusable cache,
# and a re-run is warm and cheap. Set DEMO_UNPIN=1 to destroy it too.
#
# Prerequisites:
#   - RUNPOD_API_KEY (a full-access rpa_... key). Real money: two GPU
#     deploys + one CPU staging pod, typically well under a dollar.
#   - The observability stack up (for the panels): from the repo root
#       make infra-up
#     and OTEL_EXPORTER_OTLP_ENDPOINT pointing at its collector
#     (default localhost:4317). The A/B numbers print to stdout even
#     without it; only the Grafana/Tempo views need it.
#   - A prebuilt binary: `make build` from the repo root (or set IPLANE_BIN).
#
# Usage:
#   bash examples/08-scaling-30b/08a-cold-start-distance/run.sh
#
# Env knobs:
#   DEMO_MODEL         model to A/B (default Qwen/Qwen2.5-32B-Instruct-AWQ)
#   DEMO_IMAGE         engine image (default vllm/vllm-openai:v0.7.0)
#   DEMO_MIN_VRAM_GB   min VRAM per GPU (default 24 -- fits the AWQ anchor)
#   DEMO_REGION        provider region for BOTH deploys + the pin (default EU-RO-1)
#   DEMO_PROVIDER      provider (default runpod; must support volume pinning)
#   DEMO_UNPIN         1 = destroy the pinned volume on exit (default: keep)
#   IPLANE_BIN         path to the iplane binary (default <repo>/bin/iplane)

set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${HERE}/../../.." && pwd)"
CONFIG="${HERE}/config.yaml"

MODEL="${DEMO_MODEL:-Qwen/Qwen2.5-32B-Instruct-AWQ}"
IMAGE="${DEMO_IMAGE:-vllm/vllm-openai:v0.7.0}"
MIN_VRAM="${DEMO_MIN_VRAM_GB:-24}"
# Container disk holds the engine image + (cold) the downloaded weights.
# The default (20 GB) is far too small for a large model: a 73 GB FP8 model
# fills it mid-download and the cold deploy fails. Size it to weights +
# image + headroom. 0 = leave the deploy default.
MIN_DISK="${DEMO_MIN_DISK_GB:-0}"
REGION="${DEMO_REGION:-EU-RO-1}"
PROVIDER="${DEMO_PROVIDER:-runpod}"
# `iplane deployment deploy --wait` defaults to an 8-minute deadline, which
# is too short for a real cold deploy: ~10 GB engine-image pull + the model
# download + vLLM startup routinely exceed it. Give it real room.
DEPLOY_TIMEOUT="${DEMO_DEPLOY_TIMEOUT:-20m}"
# The daemon's engine-ready wait (how long the provider polls the engine's
# /health) defaults to 10m, which a cold image pull on community capacity
# can blow past. Extend it to match the deploy budget so a slow-but-fine
# cold start isn't failed as a timeout. Read by iplane serve at startup.
export IPLANE_RUNPOD_ENGINE_READY_TIMEOUT="${DEMO_ENGINE_READY_TIMEOUT:-20m}"
SERVICE_URL="http://localhost:8080"
# NOTE: `iplane model pin/serve` both default to ~/.iplane for state.json +
# the .lock, and there is no --state-dir flag under `iplane model`, so this
# demo uses that shared default. Pin and serve must agree on the state dir
# or auto-resolve can't see the pinned volume.

COLD_ID="coldstart-cold-$$"
WARM_ID="coldstart-warm-$$"
SERVE_PID=""
VOLUME_ID=""

# ---- binary resolution: IPLANE_BIN -> repo bin/iplane -> PATH ----------
IPLANE="${IPLANE_BIN:-}"
if [[ -z "${IPLANE}" ]]; then
  if [[ -x "${REPO_ROOT}/bin/iplane" ]]; then
    IPLANE="${REPO_ROOT}/bin/iplane"
  elif command -v iplane >/dev/null 2>&1; then
    IPLANE="$(command -v iplane)"
  else
    echo "error: no iplane binary found. Run 'make build' from the repo root, or set IPLANE_BIN." >&2
    exit 1
  fi
fi

# ---- preconditions ----------------------------------------------------
if [[ -z "${RUNPOD_API_KEY:-}" && "${PROVIDER}" == "runpod" ]]; then
  echo "error: RUNPOD_API_KEY is required for --provider runpod." >&2
  exit 1
fi
if [[ -z "${OTEL_EXPORTER_OTLP_ENDPOINT:-}" ]]; then
  export OTEL_EXPORTER_OTLP_ENDPOINT="localhost:4317"
  echo "warn: OTEL_EXPORTER_OTLP_ENDPOINT unset; defaulting to ${OTEL_EXPORTER_OTLP_ENDPOINT}." >&2
  echo "      The A/B numbers still print, but the Grafana/Tempo panels need the" >&2
  echo "      obs stack up ('make infra-up' from the repo root)." >&2
fi
if lsof -iTCP:8080 -sTCP:LISTEN >/dev/null 2>&1; then
  echo "error: something is already listening on :8080. Stop any running 'iplane serve'" >&2
  echo "       first -- this demo starts and stops its own serve (pin needs the state lock)." >&2
  exit 1
fi

# ---- serve lifecycle helpers -----------------------------------------
start_serve() {
  # OTLP env propagates to the engine pod; OTEL_EXPORTER_OTLP_ENDPOINT
  # (already in the environment) drives the control-plane exporter that
  # emits the storage_tier phase/provision metrics.
  IPLANE_OTEL_ENDPOINT="${IPLANE_OTEL_ENDPOINT:-${OTEL_EXPORTER_OTLP_ENDPOINT}}" \
    "${IPLANE}" serve --config "${CONFIG}" &
  SERVE_PID=$!
  echo "==> iplane serve started (pid ${SERVE_PID}); waiting for :8080 ..."
  for _ in $(seq 1 60); do
    if curl -fsS "${SERVICE_URL}/health" >/dev/null 2>&1; then
      echo "==> serve is up."
      return 0
    fi
    if ! kill -0 "${SERVE_PID}" 2>/dev/null; then
      echo "error: iplane serve exited during startup." >&2
      SERVE_PID=""
      return 1
    fi
    sleep 1
  done
  echo "error: iplane serve did not become ready on :8080 within 60s." >&2
  return 1
}

stop_serve() {
  [[ -z "${SERVE_PID}" ]] && return 0
  kill "${SERVE_PID}" 2>/dev/null || true
  wait "${SERVE_PID}" 2>/dev/null || true
  SERVE_PID=""
  # Wait for the lock/port to actually free before the next pin/serve.
  for _ in $(seq 1 30); do
    lsof -iTCP:8080 -sTCP:LISTEN >/dev/null 2>&1 || return 0
    sleep 1
  done
}

# ---- teardown: destroy every GPU pod, then serve, then (opt) the volume
cleanup() {
  local rc=$?
  set +e
  echo ""
  echo "=== teardown ==="
  # Deploys are destroyed through the daemon, so serve must still be up.
  if [[ -z "${SERVE_PID}" ]]; then
    start_serve >/dev/null 2>&1
  fi
  for id in "${WARM_ID}" "${COLD_ID}"; do
    if "${IPLANE}" deployment describe "${id}" --service-url "${SERVICE_URL}" >/dev/null 2>&1; then
      "${IPLANE}" deployment destroy "${id}" --service-url "${SERVICE_URL}" >/dev/null 2>&1 \
        && echo "destroyed deployment ${id}" \
        || echo "WARN: could not destroy ${id} -- check the ${PROVIDER} console for orphaned pods."
    fi
  done
  stop_serve
  if [[ "${DEMO_UNPIN:-0}" == "1" && -n "${VOLUME_ID}" ]]; then
    # Volume unpin is in-process and needs the lock -- serve is down now.
    "${IPLANE}" model unpin "${VOLUME_ID}" >/dev/null 2>&1 \
      && echo "destroyed pinned volume ${VOLUME_ID}" \
      || echo "WARN: could not unpin ${VOLUME_ID}; run 'iplane model unpin ${VOLUME_ID}' by hand."
  elif [[ -n "${VOLUME_ID}" ]]; then
    echo "kept pinned volume ${VOLUME_ID} (the reusable cache; re-runs are warm)."
    echo "  destroy it with: iplane model unpin ${VOLUME_ID}   (or re-run with DEMO_UNPIN=1)"
  fi
  echo "=== teardown done ==="
  exit "${rc}"
}
trap cleanup EXIT INT TERM

# ---- deploy timer -----------------------------------------------------
# Deploys with --wait (the default) block until the engine is RUNNING, so
# the wall-clock around the call is the end-to-end provision time. The
# result lands in the global LAST_ELAPSED (command substitution would
# swallow the live deploy output).
LAST_ELAPSED=0
timed_deploy() {
  local id="$1" label="$2"
  echo ""
  echo "=== ${label} deploy (${id}) -- model ${MODEL} in ${REGION} ==="
  local t0=${SECONDS}
  # tee: keep the live progress stream AND capture it, because
  # `iplane deployment deploy` exits 0 even when the deploy lands in
  # FAILED (e.g. no-capacity / bad SKU) -- so we must read the state
  # back rather than trust the exit code.
  local disk_flag=()
  [[ "${MIN_DISK}" -gt 0 ]] && disk_flag=(--min-disk-gb "${MIN_DISK}")
  local tmp; tmp="$(mktemp)"
  "${IPLANE}" deployment deploy "${id}" \
    --provider "${PROVIDER}" \
    --region "${REGION}" \
    --min-vram-gb "${MIN_VRAM}" \
    "${disk_flag[@]}" \
    --image "${IMAGE}" \
    --model "${MODEL}" \
    --timeout "${DEPLOY_TIMEOUT}" \
    --service-url "${SERVICE_URL}" 2>&1 | tee "${tmp}"
  LAST_ELAPSED=$(( SECONDS - t0 ))
  local out; out="$(cat "${tmp}")"; rm -f "${tmp}"
  if ! grep -qE '^state:[[:space:]]+RUNNING' <<<"${out}"; then
    echo "error: ${label} deploy did not reach RUNNING -- see the state / failure line above." >&2
    return 1
  fi
  echo "=== ${label} deploy reached RUNNING in ${LAST_ELAPSED}s ==="
}

# =======================================================================
# Phase 1 -- COLD. No volume pinned yet; the engine downloads from HF.
# =======================================================================
start_serve
timed_deploy "${COLD_ID}" COLD
COLD_SECS="${LAST_ELAPSED}"
# Free the GPU before the warm run so we never hold two pods at once.
echo "==> destroying the cold deploy to free the GPU before pinning ..."
"${IPLANE}" deployment destroy "${COLD_ID}" --service-url "${SERVICE_URL}" >/dev/null 2>&1 || true
stop_serve

# =======================================================================
# Phase 2 -- PIN. Stage the weights onto a per-region network volume.
# Runs with serve DOWN (in-process, needs the state lock).
# =======================================================================
echo ""
echo "=== pin ${MODEL} onto a ${PROVIDER}/${REGION} volume ==="
echo "    (stages the weights via a throwaway CPU pod; can take a few minutes)"
PIN_OUT="$("${IPLANE}" model pin "${MODEL}" \
  --provider "${PROVIDER}" \
  --region "${REGION}")"
echo "${PIN_OUT}"
# The pin prints "... onto volume <id> (...)"; capture the id for teardown.
VOLUME_ID="$(printf '%s\n' "${PIN_OUT}" | sed -n 's/.*onto volume \([^ ]*\).*/\1/p' | head -n1)"

# =======================================================================
# Phase 3 -- WARM. Same model, same region. Deploy auto-resolves the
# pinned volume and mounts it; engine:init collapses.
# =======================================================================
start_serve
timed_deploy "${WARM_ID}" WARM
WARM_SECS="${LAST_ELAPSED}"

# ---- the A/B summary --------------------------------------------------
echo ""
echo "########################################################################"
echo "# cold-start distance"
echo "#   COLD  (download from HF) : ${COLD_SECS}s   storage_tier=cold"
echo "#   WARM  (mount pinned vol) : ${WARM_SECS}s   storage_tier=warm"
echo "#"
echo "# The headline is the wall-clock delta; the *why* is the engine:init"
echo "# phase, which the warm deploy skips. See it attributed:"
echo "#   - Tempo: search 'deployment.provision' -- compare the engine:init"
echo "#     child span on ${COLD_ID} (fat) vs ${WARM_ID} (sliver)."
echo "#   - Grafana 'Inference Plane Deployment & Lifecycle' -> 'Engine-init:"
echo "#     warm vs cold' (the storage_tier split)."
echo "########################################################################"
echo ""
echo "The warm deploy ${WARM_ID} is left RUNNING so you can inspect it."
echo "Press Enter to tear everything down (destroys ${WARM_ID}, stops serve)."
read -r _ || true
