#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# lib-parallel.sh — Common parallel-install helpers for E2E scripts.
#
# Sourced by hack/e2e/scripts/setup-cluster.sh and
# hack/e2e/scripts/install-components.sh. Provides:
#
#   fmt_dur <seconds>            — human-friendly duration formatter
#   run_phase <name> <fn>...     — run a list of shell functions in
#                                  parallel (or sequentially when
#                                  INSTALL_PARALLEL=0 or a single task),
#                                  capture per-task logs, aggregate
#                                  output at the end, and record
#                                  timings in PHASE_TIMINGS / TASK_TIMINGS.
#
# Callers must source this file BEFORE invoking run_phase. INSTALL_PARALLEL
# defaults to 1 (parallel on); set to 0 for sequential debugging.
# ---------------------------------------------------------------------------

# Guard against double-sourcing so the trap / arrays survive re-source.
if [[ "${__E2E_LIB_PARALLEL_SOURCED:-}" == "1" ]]; then
  return 0
fi
__E2E_LIB_PARALLEL_SOURCED=1

INSTALL_PARALLEL="${INSTALL_PARALLEL:-1}"

# Per-process scratch dir for parallel-task logs. Cleaned up on shell exit.
LIB_PARALLEL_LOGDIR="$(mktemp -d -t e2e-parallel-XXXXXX)"
trap 'rm -rf "${LIB_PARALLEL_LOGDIR}"' EXIT

# Public timing arrays. Callers may print these at end-of-run.
PHASE_TIMINGS=()  # "<phase-name>=<seconds>"
TASK_TIMINGS=()   # "<phase-name>/<task>=<seconds>"

# ── fmt_dur ───────────────────────────────────────────────────────────────
# Format an elapsed-seconds count as a human-friendly duration.
fmt_dur() {
  local s="$1"
  if (( s < 60 )); then
    printf '%ds' "${s}"
  else
    printf '%dm%02ds' "$((s/60))" "$((s%60))"
  fi
}

# ── run_phase ─────────────────────────────────────────────────────────────
# Run a list of zero-arg shell functions in parallel and aggregate logs.
# Usage: run_phase <phase-name> <fn1> <fn2> ...
#
# Sequential fallback is taken when INSTALL_PARALLEL!=1 OR the phase has
# only a single task (no point forking + buffering for one task).
run_phase() {
  local phase="$1"; shift
  local phase_dir="${LIB_PARALLEL_LOGDIR}/${phase}"
  mkdir -p "${phase_dir}"
  local phase_start=${SECONDS}

  if [[ "${INSTALL_PARALLEL}" != "1" || $# -le 1 ]]; then
    # Sequential fallback (or single-task phase): stream output directly.
    for fn in "$@"; do
      echo ""
      echo "── [${phase}] ${fn} ──"
      local task_start=${SECONDS}
      "${fn}"
      local task_dur=$((SECONDS - task_start))
      TASK_TIMINGS+=("${phase}/${fn}=${task_dur}")
      echo "  ⏱  [${phase}] ${fn} took $(fmt_dur "${task_dur}")"
    done
    PHASE_TIMINGS+=("${phase}=$((SECONDS - phase_start))")
    echo ""
    echo "⏱  Phase '${phase}' total: $(fmt_dur "$((SECONDS - phase_start))")"
    return 0
  fi

  echo ""
  echo "── [${phase}] launching $# tasks in parallel: $* ──"
  local pids=() names=() task_starts=()
  for fn in "$@"; do
    local task_start=${SECONDS}
    (
      set -e
      "${fn}"
    ) >"${phase_dir}/${fn}.log" 2>&1 &
    pids+=($!)
    names+=("${fn}")
    task_starts+=("${task_start}")
  done

  local rc=0
  local failed=()
  for i in "${!pids[@]}"; do
    if wait "${pids[$i]}"; then
      local d=$((SECONDS - task_starts[i]))
      echo "  ✅ [${phase}] ${names[$i]} ($(fmt_dur "${d}"))"
      TASK_TIMINGS+=("${phase}/${names[$i]}=${d}")
    else
      local d=$((SECONDS - task_starts[i]))
      echo "  ❌ [${phase}] ${names[$i]} ($(fmt_dur "${d}"))"
      TASK_TIMINGS+=("${phase}/${names[$i]}=${d}")
      failed+=("${names[$i]}")
      rc=1
    fi
  done

  # Always replay logs so users can see what each parallel task did.
  for n in "${names[@]}"; do
    echo ""
    echo "────── [${phase}] ${n} log ──────"
    cat "${phase_dir}/${n}.log"
  done

  local phase_dur=$((SECONDS - phase_start))
  PHASE_TIMINGS+=("${phase}=${phase_dur}")
  echo ""
  echo "⏱  Phase '${phase}' total: $(fmt_dur "${phase_dur}") (longest task gates this phase)"

  if [[ $rc -ne 0 ]]; then
    echo ""
    echo "❌ Phase '${phase}' failed: ${failed[*]}"
  fi
  return "${rc}"
}

# ── print_timing_summary ──────────────────────────────────────────────────
# Print a wall-clock summary of every phase + task recorded so far in
# PHASE_TIMINGS / TASK_TIMINGS. Intended to be called once at end-of-run.
# Usage: print_timing_summary <title>
print_timing_summary() {
  local title="${1:-timing summary}"
  echo ""
  echo "================ ${title} ================"
  local total=0 entry name secs
  for entry in "${PHASE_TIMINGS[@]}"; do
    name="${entry%%=*}"
    secs="${entry##*=}"
    total=$((total + secs))
    printf '  phase  %-30s %s\n' "${name}" "$(fmt_dur "${secs}")"
  done
  printf '  TOTAL  %-30s %s\n' "(sum of phase wall-clocks)" "$(fmt_dur "${total}")"
  echo ""
  echo "  Per-task wall-clocks (within a parallel phase, the longest task gates the phase):"
  for entry in "${TASK_TIMINGS[@]}"; do
    name="${entry%%=*}"
    secs="${entry##*=}"
    printf '    %-50s %s\n' "${name}" "$(fmt_dur "${secs}")"
  done
  echo "======================================================================"
}
