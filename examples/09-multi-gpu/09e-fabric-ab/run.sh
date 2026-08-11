#!/usr/bin/env bash
# Demo 09e -- the fabric A/B: two arms, identical load, one verdict.
#
# Ch 10's weakest number is the tensor-parallel-on-the-wrong-fabric throughput
# loss. The chapter currently quotes a direction and a mechanism rather than a
# figure, and its throughput panel is a synthetic stand-in captioned as
# illustrative. This harness is what replaces that with a measurement.
#
# TWO MODES.
#
#   GPU-FREE (default). Two `iplane mock-engine` processes with DIFFERENT
#   fixed latencies stand in for the two arms. This produces no fabric
#   finding whatsoever -- mock engines have no interconnect. What it does is
#   validate the harness against ground truth: a known delta is injected, and
#   the comparator must recover it. Run this before spending anything, because
#   a measurement rig you have not tested is how you end up publishing noise.
#
#   PAID (DEMO_PAID=1). The same load, the same comparator, pointed at two
#   real multi-GPU deployments. See README.md for host selection, which is
#   the part that actually decides whether the run is worth anything.
#
# WHY REPEATED RUNS. Each arm is measured DEMO_REPEAT times (default 3). A
# single run per arm cannot establish anything: two numbers are always
# different, so a one-shot A/B can only ever report noise with confidence.
# The comparator enforces this and will refuse to call a one-shot delta
# established.
#
# TEARDOWN. GPU-free mode kills only what it started. Paid mode destroys both
# deployments and both instances on exit, including on failure. Verify with
# `iplane instance list` afterwards regardless: a rented GPU that outlives the
# script is the expensive failure mode.
#
# Usage:
#   bash examples/09-multi-gpu/09e-fabric-ab/run.sh
#   DEMO_PAID=1 DEMO_A_SKU=... DEMO_B_SKU=... bash .../run.sh
#
# Env knobs:
#   DEMO_REPEAT     runs per arm (default 3; below 2 nothing can be established)
#   DEMO_DURATION   load duration per run (default 45s)
#   DEMO_RPS        offered load, requests/sec (default 4)
#   DEMO_MAXTOK     max output tokens per request (default 64)
#   DEMO_A_LATENCY  GPU-free only: arm A injected latency (default 15ms)
#   DEMO_B_LATENCY  GPU-free only: arm B injected latency (default 45ms)
#   DEMO_PAID       1 = provision real GPUs (costs money)
#   DEMO_MODEL      paid only: model to serve
#   DEMO_IMAGE      paid only: engine image
#   DEMO_A_SKU      paid only: arm A sku (the fabric arm)
#   DEMO_B_SKU      paid only: arm B sku (the control arm)
#   DEMO_A_FABRIC   paid only: arm A fabric requirement (default intra-node)
#   DEMO_B_FABRIC   paid only: arm B fabric requirement (default none)
#   DEMO_GPUS       paid only: GPUs per arm (default 4)
#   IPLANE_BIN      path to the iplane binary (default <repo>/bin/iplane)
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${HERE}/../../.." && pwd)"

IPLANE="${IPLANE_BIN:-${REPO_ROOT}/bin/iplane}"
if [[ ! -x "${IPLANE}" ]]; then
  echo "iplane binary not found at ${IPLANE}." >&2
  echo "Build it first:  (cd ${REPO_ROOT} && make build)" >&2
  exit 1
fi

REPEAT="${DEMO_REPEAT:-3}"
DURATION="${DEMO_DURATION:-45s}"
RPS="${DEMO_RPS:-4}"
MAXTOK="${DEMO_MAXTOK:-64}"
PAID="${DEMO_PAID:-0}"

A_LABEL="${DEMO_A_LABEL:-nvlink}"
B_LABEL="${DEMO_B_LABEL:-pcie}"

WORK="$(mktemp -d)"
STATE="$(mktemp -d)"
SERVE_PID=""
ENGINE_PIDS=()
DEPLOYED=()
SERVICE_URL="http://127.0.0.1:8080"

cleanup() {
  local rc=$?
  echo ""
  echo "==> teardown (exit ${rc})"
  # Print this on every path. A run that dies partway still produced summaries
  # worth keeping, and on the first failed paid run the directory was only
  # printed on success, so a $6 run's partial data was nearly lost.
  echo "    raw summaries: ${WORK}"
  for id in "${DEPLOYED[@]:-}"; do
    "${IPLANE}" deployment destroy "${id}" --service-url "${SERVICE_URL}" >/dev/null 2>&1 || true
    "${IPLANE}" instance destroy "${id}" --service-url "${SERVICE_URL}" >/dev/null 2>&1 || true
  done
  [[ -n "${SERVE_PID}" ]] && kill "${SERVE_PID}" 2>/dev/null || true
  for pid in "${ENGINE_PIDS[@]:-}"; do kill "${pid}" 2>/dev/null || true; done
  wait 2>/dev/null || true
  if [[ "${PAID}" == "1" ]]; then
    echo "    paid mode: confirm nothing survived with 'iplane instance list'"
  fi
  exit "${rc}"
}
trap cleanup EXIT INT TERM

echo "results will be written to: ${WORK}"
echo "=============================================================="
echo "== 09e fabric A/B   mode=$([[ "${PAID}" == "1" ]] && echo PAID || echo GPU-FREE)"
echo "==   arms      : ${A_LABEL} vs ${B_LABEL}"
echo "==   per arm   : ${REPEAT} runs x ${DURATION} at ${RPS} rps"
echo "=============================================================="

# --- bring up the two arms --------------------------------------------------
if [[ "${PAID}" == "1" ]]; then
  # Default sized for the 40 GB tier, which is where the healthy multi-GPU
  # A100 capacity is. A 32B at FP16 is ~64 GB of weights, so TP=4 across
  # 4x40 GB puts ~16 GB on each card and leaves room for KV. That matters for
  # this experiment specifically: the model has to be big enough that the
  # all-reduce traffic between cards is a real cost, or the A/B measures
  # nothing whichever fabric it lands on. A 70B AWQ int4 (~40 GB) also fits
  # and stresses the interconnect less.
  MODEL="${DEMO_MODEL:-Qwen/Qwen2.5-32B-Instruct}"
  IMAGE="${DEMO_IMAGE:-vllm/vllm-openai:v0.7.0}"
  A_SKU="${DEMO_A_SKU:?DEMO_A_SKU is required in paid mode}"
  B_SKU="${DEMO_B_SKU:?DEMO_B_SKU is required in paid mode}"
  GPUS="${DEMO_GPUS:-4}"
  PROVIDER="${IPLANE_PROVIDER:-vast}"

  # Three timeouts have to agree. The CLI --timeout below bounds the client
  # wait; write_timeout_sec in config.yaml must exceed it or the server severs
  # the response mid-provision with the pods still billing; and the provider's
  # engine-ready budget must not fail a slow-but-fine 4-GPU cold start.
  export IPLANE_ENGINE_READY_TIMEOUT="${DEMO_ENGINE_READY_TIMEOUT:-40m}"
  IPLANE_STATE_DIR="${STATE}" "${IPLANE}" serve --config "${HERE}/config.yaml" \
    --state-dir "${STATE}" >"${WORK}/serve.log" 2>&1 &
  SERVE_PID=$!
  until "${IPLANE}" deployment list --service-url "${SERVICE_URL}" >/dev/null 2>&1; do sleep 1; done

  # Deploy both arms before measuring either. Measuring arm A while arm B is
  # still pulling its image would compare a quiet host against a busy one.
  # --fabric is not decoration, it IS the experiment. The fabric arm must be
  # guaranteed to have an intra-node link; the control arm must be guaranteed
  # NOT to. Vast lists bridge-capable cards under PCIe names, so a control arm
  # chosen by SKU name alone can silently contain NVLink -- and then the A/B
  # compares NVLink against NVLink, reports a small delta, and the
  # contamination is invisible in the output.
  A_FABRIC="${DEMO_A_FABRIC:-intra-node}"
  B_FABRIC="${DEMO_B_FABRIC:-none}"
  for pair in "${A_LABEL}|${A_SKU}|${A_FABRIC}" "${B_LABEL}|${B_SKU}|${B_FABRIC}"; do
    IFS='|' read -r id sku fabric <<<"${pair}"
    echo "==> deploying ${id} on ${sku} x${GPUS} fabric=${fabric} (this is the slow part)"
    "${IPLANE}" deployment deploy "${id}" \
      --provider "${PROVIDER}" --sku "${sku}" --gpu-count "${GPUS}" \
      --fabric "${fabric}" \
      --image "${IMAGE}" --model "${MODEL}" \
      --engine-entrypoint python3 --engine-entrypoint=-m \
      --engine-entrypoint vllm.entrypoints.openai.api_server \
      --engine-args --tensor-parallel-size --engine-args "${GPUS}" \
      --min-disk-gb "${DEMO_DISK_GB:-150}" \
      --service-url "${SERVICE_URL}" --timeout "${DEMO_DEPLOY_TIMEOUT:-30m}"
    DEPLOYED+=("${id}")
  done
else
  A_LATENCY="${DEMO_A_LATENCY:-15ms}"
  B_LATENCY="${DEMO_B_LATENCY:-45ms}"
  MODEL="mock-model"

  echo "==> starting two mock engines (injected delta: ${A_LATENCY} vs ${B_LATENCY})"
  "${IPLANE}" mock-engine --port 9101 --latency "${A_LATENCY}" >"${WORK}/engine-a.log" 2>&1 &
  ENGINE_PIDS+=($!)
  "${IPLANE}" mock-engine --port 9102 --latency "${B_LATENCY}" >"${WORK}/engine-b.log" 2>&1 &
  ENGINE_PIDS+=($!)
  sleep 2

  IPLANE_STATE_DIR="${STATE}" IPLANE_BACKEND_ENGINE=mock OTEL_EXPORTER_OTLP_ENDPOINT="" \
    "${IPLANE}" serve --state-dir "${STATE}" >"${WORK}/serve.log" 2>&1 &
  SERVE_PID=$!
  until "${IPLANE}" deployment list --service-url "${SERVICE_URL}" >/dev/null 2>&1; do sleep 1; done

  # The external provider attaches to an already-running engine, which is what
  # makes the GPU-free path possible at all: no provisioning, no cloud, no key.
  "${IPLANE}" deployment deploy "${A_LABEL}" --provider external \
    --engine-endpoints "http://127.0.0.1:9101" --model "${MODEL}" \
    --image mock --service-url "${SERVICE_URL}" >/dev/null
  DEPLOYED+=("${A_LABEL}")
  "${IPLANE}" deployment deploy "${B_LABEL}" --provider external \
    --engine-endpoints "http://127.0.0.1:9102" --model "${MODEL}" \
    --image mock --service-url "${SERVICE_URL}" >/dev/null
  DEPLOYED+=("${B_LABEL}")
fi

# --- measure ----------------------------------------------------------------
# Interleave the arms (A,B,A,B,...) rather than running all of A then all of B.
# Anything that drifts over the session -- a noisy neighbour, a thermal ramp,
# network weather -- would otherwise land entirely on whichever arm went second
# and be indistinguishable from a fabric effect.
A_FILES=(); B_FILES=()
for ((i = 1; i <= REPEAT; i++)); do
  for arm in "${A_LABEL}" "${B_LABEL}"; do
    out="${WORK}/${arm}-${i}.json"
    echo "==> run ${i}/${REPEAT}  arm=${arm}"
    # --chat-fraction 1.0 because TTFT is only measurable on the chat SSE
    # shape (choices[].delta.content). The legacy completions endpoint
    # streams choices[].text, which the parser does not time, so any
    # non-chat fraction silently shrinks the TTFT sample count.
    "${IPLANE}" load \
      --target "${arm}" --service-url "${SERVICE_URL}" \
      --model "${MODEL}" \
      --duration "${DURATION}" --rps "${RPS}" --max-tokens "${MAXTOK}" \
      --stream --output json \
      --chat-fraction 1.0 \
      --skip-model-validation \
      >"${out}" 2>"${WORK}/${arm}-${i}.err" || true
    # `iplane load` exits non-zero if ANY request errored, but it prints the
    # summary first. Judge on whether a usable summary exists, not on the exit
    # code: a single transient error must not discard a run whose provisioning
    # is already paid for, and the comparator already warns on errors so the
    # operator can decide what the error rate means. Losing a $6 run to one bad
    # request out of fifty is how this was learned.
    if ! grep -q '"successes"' "${out}" 2>/dev/null; then
      echo "    load produced no usable summary for ${arm}; stderr:" >&2
      tail -5 "${WORK}/${arm}-${i}.err" >&2
      exit 1
    fi
    if [[ -s "${WORK}/${arm}-${i}.err" ]] && grep -qi "errored" "${WORK}/${arm}-${i}.err"; then
      echo "    note: $(grep -i errored "${WORK}/${arm}-${i}.err" | head -1) -- continuing; see the comparator's warnings"
    fi
    if [[ "${arm}" == "${A_LABEL}" ]]; then A_FILES+=("${out}"); else B_FILES+=("${out}"); fi
  done
done

# --- compare ----------------------------------------------------------------
echo ""
(cd "${REPO_ROOT}/examples" && go run ./09-multi-gpu/09e-fabric-ab/compare \
  --a-label "${A_LABEL}" --a "$(IFS=,; echo "${A_FILES[*]}")" \
  --b-label "${B_LABEL}" --b "$(IFS=,; echo "${B_FILES[*]}")")

echo ""
echo "raw summaries: ${WORK}"
if [[ "${PAID}" != "1" ]]; then
  echo ""
  echo "NOTE: GPU-free mode. The delta above is the latency injected into the"
  echo "      two mock engines (${DEMO_A_LATENCY:-15ms} vs ${DEMO_B_LATENCY:-45ms}), NOT a fabric result."
  echo "      What it demonstrates is that the harness recovers a known delta."
  echo "      For a real finding: DEMO_PAID=1, and read README.md on host choice."
fi
