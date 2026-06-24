#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# loadgen.sh — Drive OpenAI-style chat-completion traffic at a target QPS
# against the modeldeployment(s) in a workload namespace provisioned by
# charts/modelharness + charts/modeldeployment.
#
# The request path exercised is the full production path:
#   localhost (port-forward) → Istio Gateway "<ns>-gw" → BBR ext_proc
#   → EPP (InferencePool ext_proc) → vLLM (mock) inference pod.
#
# Two load engines are supported:
#   * curl     (default) — a self-contained QPS-paced loop that records the
#                          HTTP status code and end-to-end latency of every
#                          request and prints a success/error + latency
#                          summary (avg / p50 / p90 / p99). No extra deps.
#   * guidellm           — delegates to the `guidellm` benchmark CLI for a
#                          full TTFT / TPOT / throughput report (same tool
#                          the .github/workflows/benchmark.yaml uses).
#
# The script auto-discovers the model name, the per-namespace Gateway, and
# (when the namespace has API-key auth enabled) the bearer token from the
# operator-minted `llm-api-key` Secret. It starts (and cleans up) its own
# `kubectl port-forward` to the Gateway Service.
#
# ---------------------------------------------------------------------------
# Usage:
#   hack/loadgen.sh -n <namespace> [options]
#
# Common options:
#   -n, --namespace NS     Workload namespace (required).
#   -m, --model NAME       modeldeployment name == OpenAI `model` field.
#                          Auto-discovered from the namespace when omitted.
#   -q, --qps N            Target requests/sec for the curl engine (default 2).
#   -d, --duration SEC     How long to send traffic, seconds (default 30).
#   -c, --concurrency N    Max in-flight requests for the curl engine
#                          (default: max(qps, 4)).
#       --requests N       Stop after N requests instead of after --duration.
#   -p, --prompt TEXT      Prompt content (default: a short fixed sentence).
#       --max-tokens N     max_tokens in the request body (default 64).
#   -e, --engine ENGINE    curl (default) | guidellm.
#   -g, --gateway NAME     Gateway resource name (default "<ns>-gw").
#       --api-key KEY      Bearer token override (else auto-read when auth on).
#       --no-auth          Skip API-key auto-detection (send unauthenticated).
#       --local-port PORT  Local port for the port-forward (default: random).
#
# guidellm-only options:
#       --processor ID     HF tokenizer/processor id (default
#                          microsoft/Phi-4-mini-instruct).
#       --profile P        guidellm profile (default constant).
#       --rate R           guidellm rate (default = --qps).
#       --data SPEC        guidellm synthetic-data spec
#                          (default prompt_tokens=256,output_tokens=64).
#       --max-seconds SEC  guidellm per-benchmark cap (default = --duration).
#       --out-dir DIR      Directory for guidellm JSON/CSV result files
#                          (default: a fresh mktemp dir, printed on exit).
#
# Examples:
#   # 5 QPS for 60s against the auto-discovered model in namespace `demo-a`:
#   hack/loadgen.sh -n demo-a -q 5 -d 60
#
#   # Target a specific model and run a guidellm sweep:
#   hack/loadgen.sh -n demo-a -m chat-phi -e guidellm \
#       --profile sweep --max-seconds 30
# ---------------------------------------------------------------------------
set -euo pipefail

# ── Defaults ───────────────────────────────────────────────────────────────
NAMESPACE=""
MODEL=""
QPS="2"
DURATION="30"
CONCURRENCY=""
MAX_REQUESTS=""
PROMPT="Write a one-sentence hello to the production stack."
MAX_TOKENS="64"
ENGINE="curl"
GATEWAY=""
API_KEY=""
AUTH_MODE="auto"   # auto | off
LOCAL_PORT=""

# guidellm-only
PROCESSOR="microsoft/Phi-4-mini-instruct"
GUIDELLM_PROFILE="constant"
GUIDELLM_RATE=""
GUIDELLM_DATA="prompt_tokens=256,output_tokens=64"
GUIDELLM_MAX_SECONDS=""
GUIDELLM_OUT_DIR=""

APIKEY_SECRET_NAME="llm-api-key"
APIKEY_SECRET_KEY="apiKey"

die() { echo "❌ $*" >&2; exit 1; }
info() { echo "▶︎ $*"; }

usage() { sed -n '2,75p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit "${1:-0}"; }

# ── Parse args ──────────────────────────────────────────────────────────────
while [[ $# -gt 0 ]]; do
  case "$1" in
    -n|--namespace)   NAMESPACE="$2"; shift 2 ;;
    -m|--model)       MODEL="$2"; shift 2 ;;
    -q|--qps)         QPS="$2"; shift 2 ;;
    -d|--duration)    DURATION="$2"; shift 2 ;;
    -c|--concurrency) CONCURRENCY="$2"; shift 2 ;;
    --requests)       MAX_REQUESTS="$2"; shift 2 ;;
    -p|--prompt)      PROMPT="$2"; shift 2 ;;
    --max-tokens)     MAX_TOKENS="$2"; shift 2 ;;
    -e|--engine)      ENGINE="$2"; shift 2 ;;
    -g|--gateway)     GATEWAY="$2"; shift 2 ;;
    --api-key)        API_KEY="$2"; shift 2 ;;
    --no-auth)        AUTH_MODE="off"; shift ;;
    --local-port)     LOCAL_PORT="$2"; shift 2 ;;
    --processor)      PROCESSOR="$2"; shift 2 ;;
    --profile)        GUIDELLM_PROFILE="$2"; shift 2 ;;
    --rate)           GUIDELLM_RATE="$2"; shift 2 ;;
    --data)           GUIDELLM_DATA="$2"; shift 2 ;;
    --max-seconds)    GUIDELLM_MAX_SECONDS="$2"; shift 2 ;;
    --out-dir)        GUIDELLM_OUT_DIR="$2"; shift 2 ;;
    -h|--help)        usage 0 ;;
    *) die "Unknown argument: $1 (use --help)" ;;
  esac
done

[[ -n "${NAMESPACE}" ]] || { echo "Missing required --namespace" >&2; usage 1; }
case "${ENGINE}" in curl|guidellm) ;; *) die "Invalid --engine '${ENGINE}' (curl|guidellm)";; esac
command -v kubectl >/dev/null || die "kubectl not found in PATH"

# ── Verify namespace + discover model / gateway ─────────────────────────────
kubectl get namespace "${NAMESPACE}" >/dev/null 2>&1 || die "namespace '${NAMESPACE}' not found"

GATEWAY="${GATEWAY:-${NAMESPACE}-gw}"
GATEWAY_SVC="${GATEWAY}-istio"

if [[ -z "${MODEL}" ]]; then
  info "Discovering model name from modeldeployment HTTPRoutes in ${NAMESPACE}..."
  # Each modeldeployment HTTPRoute is stamped kaito.sh/owned-by=modeldeployment
  # and kaito.sh/inferenceset=<model-name>. The model name is also the value
  # sent in the OpenAI `model` field (matched as X-Gateway-Model-Name).
  # (bash 3.2 has no mapfile — collect into a space-separated string.)
  MODELS_RAW="$(kubectl get httproute -n "${NAMESPACE}" \
    -l kaito.sh/owned-by=modeldeployment \
    -o jsonpath='{range .items[*]}{.metadata.labels.kaito\.sh/inferenceset}{"\n"}{end}' 2>/dev/null \
    | sed '/^$/d' | sort -u)"
  if [[ -z "${MODELS_RAW}" ]]; then
    # Fall back to InferenceSet names.
    MODELS_RAW="$(kubectl get inferenceset -n "${NAMESPACE}" \
      -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null | sed '/^$/d' | sort -u)"
  fi
  [[ -n "${MODELS_RAW}" ]] || die "no modeldeployment found in '${NAMESPACE}' — pass --model"
  MODEL="$(echo "${MODELS_RAW}" | head -1)"
  MODEL_COUNT="$(echo "${MODELS_RAW}" | wc -l | tr -d ' ')"
  if [[ "${MODEL_COUNT}" -gt 1 ]]; then
    info "Found ${MODEL_COUNT} models ($(echo ${MODELS_RAW} | tr '\n' ' ')); defaulting to '${MODEL}'. Use --model to pick another."
  fi
fi
info "Target model: ${MODEL}  (namespace=${NAMESPACE}, gateway=${GATEWAY})"

kubectl get svc -n "${NAMESPACE}" "${GATEWAY_SVC}" >/dev/null 2>&1 \
  || die "gateway service ${NAMESPACE}/${GATEWAY_SVC} not found (is modelharness installed?)"

# ── Auth detection ──────────────────────────────────────────────────────────
HOST_HEADER=""
if [[ "${AUTH_MODE}" == "auto" && -z "${API_KEY}" ]]; then
  if kubectl get secret -n "${NAMESPACE}" "${APIKEY_SECRET_NAME}" >/dev/null 2>&1; then
    API_KEY="$(kubectl get secret -n "${NAMESPACE}" "${APIKEY_SECRET_NAME}" \
      -o jsonpath="{.data.${APIKEY_SECRET_KEY}}" | base64 -d)"
    info "API-key auth detected — using bearer token from Secret ${APIKEY_SECRET_NAME}"
  fi
fi
if [[ -n "${API_KEY}" ]]; then
  # The llm-gateway-apikey ext_authz filter resolves the APIKey CR namespace
  # from the request Host subdomain "<ns>.gw.kaito.sh".
  HOST_HEADER="${NAMESPACE}.gw.kaito.sh"
fi

# ── Port-forward to the gateway ─────────────────────────────────────────────
if [[ -z "${LOCAL_PORT}" ]]; then
  LOCAL_PORT="$(python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()' 2>/dev/null || echo 18080)"
fi
PF_LOG="$(mktemp -t loadgen-pf.XXXXXX)"
info "Starting port-forward svc/${GATEWAY_SVC} → localhost:${LOCAL_PORT}"
kubectl -n "${NAMESPACE}" port-forward "svc/${GATEWAY_SVC}" "${LOCAL_PORT}:80" >"${PF_LOG}" 2>&1 &
PF_PID=$!

cleanup() {
  [[ -n "${PF_PID:-}" ]] && kill "${PF_PID}" >/dev/null 2>&1 || true
  [[ -n "${PF_LOG:-}" && -f "${PF_LOG}" ]] && rm -f "${PF_LOG}" || true
  [[ -n "${REQ_LOG:-}" && -f "${REQ_LOG}" ]] && rm -f "${REQ_LOG}" || true
}
trap cleanup EXIT INT TERM

# Wait for the local port to accept connections.
for _ in $(seq 1 30); do
  if (exec 3<>"/dev/tcp/127.0.0.1/${LOCAL_PORT}") 2>/dev/null; then exec 3>&- 3<&-; break; fi
  if ! kill -0 "${PF_PID}" 2>/dev/null; then cat "${PF_LOG}" >&2; die "port-forward exited early"; fi
  sleep 1
done
(exec 3<>"/dev/tcp/127.0.0.1/${LOCAL_PORT}") 2>/dev/null || { cat "${PF_LOG}" >&2; die "port-forward not ready"; }
exec 3>&- 3<&- 2>/dev/null || true

ENDPOINT="http://127.0.0.1:${LOCAL_PORT}"
if [[ -n "${HOST_HEADER}" ]]; then
  ENDPOINT="http://${HOST_HEADER}:${LOCAL_PORT}"   # resolves to 127.0.0.1 below
fi

# ── Smoke test ──────────────────────────────────────────────────────────────
smoke_curl=(curl -sS -o /dev/null -w '%{http_code}' --max-time 60
  --resolve "${HOST_HEADER:-127.0.0.1}:${LOCAL_PORT}:127.0.0.1"
  -H 'Content-Type: application/json')
[[ -n "${API_KEY}" ]] && smoke_curl+=(-H "Authorization: Bearer ${API_KEY}")
smoke_body="{\"model\":\"${MODEL}\",\"messages\":[{\"role\":\"user\",\"content\":\"ping\"}],\"max_tokens\":8}"
info "Smoke-testing ${ENDPOINT}/v1/chat/completions ..."
smoke_code="$("${smoke_curl[@]}" -d "${smoke_body}" "${ENDPOINT}/v1/chat/completions" || echo 000)"
if [[ "${smoke_code}" =~ ^2 ]]; then
  echo "  ✅ smoke test OK (HTTP ${smoke_code})"
else
  echo "  ⚠️  smoke test returned HTTP ${smoke_code} — continuing anyway"
fi

# ─────────────────────────────────────────────────────────────────────────
# Engine: guidellm
# ─────────────────────────────────────────────────────────────────────────
if [[ "${ENGINE}" == "guidellm" ]]; then
  # Resolve a guidellm binary. Prefer one already on PATH; otherwise install it
  # into a dedicated virtualenv so we don't trip over PEP 668 ("externally
  # managed") on Homebrew/Debian Python, and don't pollute the system env.
  if command -v guidellm >/dev/null 2>&1; then
    GUIDELLM_BIN="$(command -v guidellm)"
  else
    GUIDELLM_VENV="${GUIDELLM_VENV:-${HOME}/.cache/kaito-loadgen/guidellm-venv}"
    GUIDELLM_BIN="${GUIDELLM_VENV}/bin/guidellm"
    if [[ ! -x "${GUIDELLM_BIN}" ]]; then
      info "guidellm not found — creating venv at ${GUIDELLM_VENV} and installing 'guidellm[recommended]'..."
      python3 -m venv "${GUIDELLM_VENV}"
      "${GUIDELLM_VENV}/bin/python" -m pip install --quiet --upgrade pip
      "${GUIDELLM_VENV}/bin/python" -m pip install --quiet 'guidellm[recommended]'
    fi
    [[ -x "${GUIDELLM_BIN}" ]] || die "guidellm install failed (no executable at ${GUIDELLM_BIN})"
  fi
  GUIDELLM_RATE="${GUIDELLM_RATE:-${QPS}}"
  GUIDELLM_MAX_SECONDS="${GUIDELLM_MAX_SECONDS:-${DURATION}}"
  if [[ -n "${GUIDELLM_OUT_DIR}" ]]; then
    OUT_DIR="${GUIDELLM_OUT_DIR}"; mkdir -p "${OUT_DIR}"
  else
    OUT_DIR="$(mktemp -d -t loadgen-guidellm.XXXXXX)"
  fi
  info "Running guidellm (profile=${GUIDELLM_PROFILE}, rate=${GUIDELLM_RATE}, max-seconds=${GUIDELLM_MAX_SECONDS})"
  echo "    target=${ENDPOINT}  model=${MODEL}  processor=${PROCESSOR}  data=${GUIDELLM_DATA}"
  bk="{\"verify\":false,\"validate_backend\":false"
  [[ -n "${API_KEY}" ]] && bk="${bk},\"api_key\":\"${API_KEY}\""
  bk="${bk}}"
  "${GUIDELLM_BIN}" benchmark run \
    --target "${ENDPOINT}" \
    --model "${MODEL}" \
    --processor "${PROCESSOR}" \
    --backend-kwargs "${bk}" \
    --profile "${GUIDELLM_PROFILE}" \
    --rate "${GUIDELLM_RATE}" \
    --max-seconds "${GUIDELLM_MAX_SECONDS}" \
    --data "${GUIDELLM_DATA}" \
    --output-dir "${OUT_DIR}" \
    --outputs json --outputs csv \
    --disable-console-interactive
  echo ""
  info "guidellm result files in ${OUT_DIR}:"
  ls -1 "${OUT_DIR}" | sed 's/^/  /'
  exit 0
fi

# ─────────────────────────────────────────────────────────────────────────
# Engine: curl (QPS-paced loop)
# ─────────────────────────────────────────────────────────────────────────
# Validate numeric inputs.
[[ "${QPS}" =~ ^[0-9]+([.][0-9]+)?$ ]] || die "--qps must be a number"
[[ "${DURATION}" =~ ^[0-9]+$ ]] || die "--duration must be an integer (seconds)"
if [[ -z "${CONCURRENCY}" ]]; then
  CONCURRENCY="$(awk -v q="${QPS}" 'BEGIN{c=int(q+0.999); if(c<4)c=4; print c}')"
fi

REQ_LOG="$(mktemp -t loadgen-req.XXXXXX)"
INTERVAL="$(awk -v q="${QPS}" 'BEGIN{printf "%.6f", 1.0/q}')"

BODY="{\"model\":\"${MODEL}\",\"messages\":[{\"role\":\"user\",\"content\":$(printf '%s' "${PROMPT}" | python3 -c 'import json,sys;print(json.dumps(sys.stdin.read()))')}],\"max_tokens\":${MAX_TOKENS}}"

# One request: append "<http_code> <time_total>" to the shared log.
send_one() {
  local code_time
  local c=(curl -sS -o /dev/null -w '%{http_code} %{time_total}' --max-time 120
    --resolve "${HOST_HEADER:-127.0.0.1}:${LOCAL_PORT}:127.0.0.1"
    -H 'Content-Type: application/json')
  [[ -n "${API_KEY}" ]] && c+=(-H "Authorization: Bearer ${API_KEY}")
  code_time="$("${c[@]}" -d "${BODY}" "${ENDPOINT}/v1/chat/completions" 2>/dev/null || echo '000 0')"
  echo "${code_time}" >>"${REQ_LOG}"
}

echo ""
if [[ -n "${MAX_REQUESTS}" ]]; then
  info "Sending traffic: qps=${QPS} (interval=${INTERVAL}s) concurrency=${CONCURRENCY} requests=${MAX_REQUESTS}"
else
  info "Sending traffic: qps=${QPS} (interval=${INTERVAL}s) concurrency=${CONCURRENCY} duration=${DURATION}s"
fi
echo "    endpoint=${ENDPOINT}/v1/chat/completions  model=${MODEL}  auth=$([[ -n "${API_KEY}" ]] && echo on || echo off)"

# Use whole-second wall clock (portable across macOS/Linux; avoids date +%N).
START_EPOCH="$(date +%s)"
DEADLINE=$((START_EPOCH + DURATION))
i=0
REQ_PIDS=""   # track only request jobs (NOT the port-forward, which runs forever)
while :; do
  # Stop conditions.
  if [[ -n "${MAX_REQUESTS}" ]]; then
    [[ "${i}" -ge "${MAX_REQUESTS}" ]] && break
  else
    [[ "$(date +%s)" -ge "${DEADLINE}" ]] && break
  fi

  # Throttle concurrent background curls (bash 3.2: no `wait -n`).
  # Count only live request PIDs so the long-running port-forward never blocks us.
  while :; do
    running=0; live_pids=""
    for p in ${REQ_PIDS}; do
      if kill -0 "${p}" 2>/dev/null; then running=$((running+1)); live_pids="${live_pids} ${p}"; fi
    done
    REQ_PIDS="${live_pids}"
    [[ "${running}" -lt "${CONCURRENCY}" ]] && break
    sleep 0.01
  done

  send_one &
  REQ_PIDS="${REQ_PIDS} $!"
  i=$((i+1))

  # Pace to the target QPS with a fractional sleep between launches.
  sleep "${INTERVAL}"
done

info "Waiting for in-flight requests to drain..."
# Wait only on request jobs; a bare `wait` would also block on the port-forward.
for p in ${REQ_PIDS}; do wait "${p}" 2>/dev/null || true; done
END_EPOCH="$(date +%s)"
ELAPSED=$((END_EPOCH - START_EPOCH)); [[ "${ELAPSED}" -lt 1 ]] && ELAPSED=1

# ── Aggregate + report ──────────────────────────────────────────────────────
echo ""
echo "════════════════════════ loadgen.sh summary ════════════════════════"
# Counts.
awk -v elapsed="${ELAPSED}" '
{
  code=$1; total++;
  if (code ~ /^2/) ok++;
  else if (code ~ /^5/) e5++;
  else if (code=="000") terr++;
  else other++;
}
END{
  printf "  requests sent      : %d\n", total+0;
  printf "  2xx success        : %d\n", ok+0;
  printf "  5xx errors         : %d\n", e5+0;
  printf "  other non-2xx      : %d\n", other+0;
  printf "  transport errors   : %d\n", terr+0;
  if (elapsed+0>0) printf "  achieved QPS       : %.2f\n", (total+0)/elapsed;
}' "${REQ_LOG}"
# Latency percentiles from curl %{time_total} (portable: sort -n + awk index).
awk '{print $2}' "${REQ_LOG}" | sort -n | awk '
function pct(p,   idx){ idx=int(NR*p); if(idx<1)idx=1; if(idx>NR)idx=NR; return v[idx] }
{ v[NR]=$1; sum+=$1 }
END{
  if (NR==0) exit;
  printf "  latency avg        : %.3fs\n", sum/NR;
  printf "  latency p50        : %.3fs\n", pct(0.50);
  printf "  latency p90        : %.3fs\n", pct(0.90);
  printf "  latency p99        : %.3fs\n", pct(0.99);
}'
echo "══════════════════════════════════════════════════════════════════════════"
