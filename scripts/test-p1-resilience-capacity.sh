#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/p1-resilience-capacity-lib.sh
source "${ROOT}/scripts/p1-resilience-capacity-lib.sh"
temporary="$(mktemp -d)"
trap 'rm -rf "${temporary}"' EXIT INT TERM

accepts() { validate_capacity_settings 1 1 1 "$1" 1 >/dev/null; }
rejects() { ! validate_capacity_settings "$@" >/dev/null 2>&1; }

accepts 1
accepts 32
rejects 1 1 1 33 1
rejects 1 1 1 0 1
rejects 1 1 1 nope 1
rejects 501 1 1 1 1
rejects 1 33 1 1 1
rejects 1 1 30001 1 1
rejects 1 1 1 1 4097

invalid_run_id="I08-invalid-concurrency-$$"
invalid_prefix="$(printf '%s' "${invalid_run_id}" | tr '[:upper:]_' '[:lower:]-' | tr -cd 'a-z0-9-')"
if RUN_ID="${invalid_run_id}" RUNNER_TEMP="${temporary}" REQUEST_CONCURRENCY=33 \
  "${ROOT}/scripts/p1-resilience-capacity.sh" >"${temporary}/invalid.out" 2>"${temporary}/invalid.err"; then
  echo "capacity harness accepted excessive request concurrency" >&2
  exit 1
fi
grep -Fq 'REQUEST_CONCURRENCY exceeds the I08 envelope maximum 32' "${temporary}/invalid.err"
test ! -e "${temporary}/ocservia-${invalid_prefix}"

invalid_profile_id="I08-invalid-profile-$$"
if RUN_ID="${invalid_profile_id}" RUNNER_TEMP="${temporary}" P1_PROFILE=custom \
  "${ROOT}/scripts/p1-resilience-capacity.sh" >"${temporary}/profile.out" 2>"${temporary}/profile.err"; then
  echo "capacity harness accepted an unknown profile" >&2
  exit 1
fi
grep -Fq 'P1_PROFILE must be smoke or full' "${temporary}/profile.err"

interrupted_operation_is_final unknown
for state in queued dispatched accepted running; do
  if interrupted_operation_is_final "${state}"; then
    echo "transient operation state ${state} accepted as final" >&2
    exit 1
  fi
done

state_file="${temporary}/states"
printf '%s\n' running dispatched unknown >"${state_file}"
read_state() {
  local state
  state="$(head -1 "${state_file}")"
  sed '1d' "${state_file}" >"${state_file}.next"
  mv "${state_file}.next" "${state_file}"
  printf '%s\n' "${state}"
}
wait_for_interrupted_operation operation-id 3 0 read_state
test ! -s "${state_file}"
always_running() { printf '%s\n' running; }
if wait_for_interrupted_operation operation-id 2 0 always_running >/dev/null 2>&1; then
  echo "interrupted operation timeout was accepted" >&2
  exit 1
fi

initial_operation="${temporary}/interrupted-operation-initial.json"
final_operation="${temporary}/interrupted-operation-final.json"
operation_summary="${temporary}/interrupted-operation-summary.json"
jq -n '{id:"operation-id",state:"queued"}' >"${initial_operation}"
jq -n '{id:"operation-id",state:"unknown",attempts:1}' >"${final_operation}"
write_interrupted_operation_evidence "${initial_operation}" "${final_operation}" "${operation_summary}"
jq -e '.state == "queued"' "${initial_operation}" >/dev/null
jq -e '.state == "unknown"' "${final_operation}" >/dev/null
jq -e '.operation_id == "operation-id" and .initial_state == "queued" and .final_state == "unknown"' \
  "${operation_summary}" >/dev/null
jq -n '{id:"operation-id",state:"running"}' >"${final_operation}"
if write_interrupted_operation_evidence "${initial_operation}" "${final_operation}" "${operation_summary}" \
  >/dev/null 2>&1; then
  echo "non-final interrupted operation evidence was accepted" >&2
  exit 1
fi

make_sample() {
  local phase=$1 epoch=$2
  jq -cn --arg phase "${phase}" --arg at "2026-08-04T00:00:${epoch}Z" --argjson epoch "${epoch}" \
    '{at:$at,epoch_seconds:$epoch,phase:$phase,runtime:{goroutines:2,db_acquired:1,db_idle:1,db_total:2,sse_active_streams:4,sse_rejected_streams:0,sse_watchers:1,sse_unhealthy_watchers:0,sse_sql_queries:3,sse_slow_consumer_disconnects:0,sse_database_backoff_seconds:0},stub:{active_tasks:3,task_capacity:8},postgres:{active:1,waiting:0,available:true},control_rss_kib:10,control_fd:4,stub_rss_kib:8,stub_fd:3}'
}
samples="${temporary}/samples.jsonl"
epoch=0
IFS=',' read -r -a phases <<<"${I08_REQUIRED_SAMPLE_PHASES}"
for phase in "${phases[@]}" capacity-load capacity-load capacity-load; do
  make_sample "${phase}" "${epoch}" >>"${samples}"
  epoch=$((epoch + 1))
done
validate_resource_samples "${samples}" "${temporary}/summary.json" 10 9 0
jq -e '.samples == 10 and .sampler_exit == 0 and .phase_coverage["transport-recovery"] == 1' "${temporary}/summary.json" >/dev/null

if validate_resource_samples "${samples}" "${temporary}/short.json" 11 0 0 >/dev/null 2>&1; then
  echo "insufficient samples were accepted" >&2
  exit 1
fi
grep -v 'postgres-recovered' "${samples}" >"${temporary}/missing-phase.jsonl"
if validate_resource_samples "${temporary}/missing-phase.jsonl" "${temporary}/missing.json" 1 0 0 >/dev/null 2>&1; then
  echo "missing sample phase was accepted" >&2
  exit 1
fi
cp "${samples}" "${temporary}/corrupt.jsonl"
printf '%s\n' '{broken' >>"${temporary}/corrupt.jsonl"
if validate_resource_samples "${temporary}/corrupt.jsonl" "${temporary}/corrupt.json" 1 0 0 >/dev/null 2>&1; then
  echo "corrupt sample JSON was accepted" >&2
  exit 1
fi
jq -c '.runtime.goroutines = "2"' "${samples}" >"${temporary}/string-number.jsonl"
if validate_resource_samples "${temporary}/string-number.jsonl" "${temporary}/string-number.json" 1 0 0 >/dev/null 2>&1; then
  echo "string resource counter was accepted" >&2
  exit 1
fi

SAMPLER_EXIT=0
SAMPLER_REAPED=0
SAMPLER_STOP_REQUESTED=0
sampler_ready="${temporary}/sampler-ready"
bash -c 'trap "exit 0" TERM; : >"$1"; while :; do sleep 1; done' _ "${sampler_ready}" &
SAMPLE_PID=$!
for _ in $(seq 1 100); do
  [[ -f "${sampler_ready}" ]] && break
  sleep 0.01
done
test -f "${sampler_ready}"
stop_sampler
test "${SAMPLER_EXIT}" = 0

cleanup_marker="${temporary}/cleanup-ran"
if (
  trap 'touch "${cleanup_marker}"' EXIT
  SAMPLER_EXIT=0
  # shellcheck disable=SC2034
  SAMPLER_REAPED=0
  # shellcheck disable=SC2034
  SAMPLER_STOP_REQUESTED=0
  bash -c 'exit 7' &
  # shellcheck disable=SC2034
  SAMPLE_PID=$!
  sleep 0.1
  check_sampler 2>/dev/null
); then
  echo "failed sampler did not fail the test flow" >&2
  exit 1
fi
test -f "${cleanup_marker}"

echo "P1 resilience capacity boundary tests passed"
