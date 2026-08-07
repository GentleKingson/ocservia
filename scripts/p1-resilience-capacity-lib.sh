#!/usr/bin/env bash

I08_MAX_AGENTS=500
I08_MAX_HEARTBEATS=32
I08_MAX_HEARTBEAT_INTERVAL_MS=30000
I08_MAX_REQUEST_CONCURRENCY=32
I08_MAX_QUEUE_CAPACITY=4096
I08_REQUIRED_SAMPLE_PHASES="capacity-load,slow-sse,controller-restart,transport-interrupt,transport-recovery,postgres-paused,postgres-recovered"

validate_capacity_settings() {
  local agent_count=$1 heartbeat_count=$2 heartbeat_interval_ms=$3 request_concurrency=$4 queue_capacity=$5
  local name value
  for name in AGENT_COUNT HEARTBEAT_COUNT HEARTBEAT_INTERVAL_MS REQUEST_CONCURRENCY QUEUE_CAPACITY; do
    case "${name}" in
      AGENT_COUNT) value=${agent_count} ;;
      HEARTBEAT_COUNT) value=${heartbeat_count} ;;
      HEARTBEAT_INTERVAL_MS) value=${heartbeat_interval_ms} ;;
      REQUEST_CONCURRENCY) value=${request_concurrency} ;;
      QUEUE_CAPACITY) value=${queue_capacity} ;;
    esac
    if [[ ! "${value}" =~ ^[1-9][0-9]*$ ]]; then
      echo "${name} must be a positive integer" >&2
      return 2
    fi
  done
  if ((agent_count > I08_MAX_AGENTS)); then
    echo "AGENT_COUNT exceeds the I08 envelope maximum ${I08_MAX_AGENTS}" >&2
    return 2
  fi
  if ((heartbeat_count > I08_MAX_HEARTBEATS)); then
    echo "HEARTBEAT_COUNT exceeds the I08 envelope maximum ${I08_MAX_HEARTBEATS}" >&2
    return 2
  fi
  if ((heartbeat_interval_ms > I08_MAX_HEARTBEAT_INTERVAL_MS)); then
    echo "HEARTBEAT_INTERVAL_MS exceeds the I08 envelope maximum ${I08_MAX_HEARTBEAT_INTERVAL_MS}" >&2
    return 2
  fi
  if ((request_concurrency > I08_MAX_REQUEST_CONCURRENCY)); then
    echo "REQUEST_CONCURRENCY exceeds the I08 envelope maximum ${I08_MAX_REQUEST_CONCURRENCY}" >&2
    return 2
  fi
  if ((queue_capacity > I08_MAX_QUEUE_CAPACITY)); then
    echo "QUEUE_CAPACITY exceeds the I08 envelope maximum ${I08_MAX_QUEUE_CAPACITY}" >&2
    return 2
  fi
}

interrupted_operation_is_final() {
  [[ $1 == "unknown" ]]
}

write_interrupted_operation_evidence() {
  local initial_file=$1 final_file=$2 summary_file=$3
  local initial_id final_id initial_state final_state

  if ! initial_id="$(jq -er '.id | strings | select(length > 0)' "${initial_file}")" \
    || ! final_id="$(jq -er '.id | strings | select(length > 0)' "${final_file}")" \
    || ! initial_state="$(jq -er '.state | select(. == "queued" or . == "dispatched")' "${initial_file}")" \
    || ! final_state="$(jq -er '.state | select(. == "unknown")' "${final_file}")"; then
    echo "interrupted operation evidence has invalid state or identity" >&2
    return 1
  fi
  if [[ "${initial_id}" != "${final_id}" ]]; then
    echo "interrupted operation evidence identity mismatch" >&2
    return 1
  fi

  jq -n --arg operation_id "${initial_id}" --arg initial_state "${initial_state}" \
    --arg final_state "${final_state}" \
    '{operation_id:$operation_id,initial_state:$initial_state,final_state:$final_state}' \
    >"${summary_file}"
}

wait_for_interrupted_operation() {
  local operation_id=$1 attempts=$2 delay=$3 reader=$4
  local attempt state="unread"
  for ((attempt = 1; attempt <= attempts; attempt++)); do
    if ! state="$(${reader} "${operation_id}")"; then
      echo "failed to read interrupted operation ${operation_id} at attempt ${attempt}" >&2
      return 1
    fi
    if interrupted_operation_is_final "${state}"; then
      return 0
    fi
    case "${state}" in
      queued | dispatched | accepted | running) ;;
      *)
        echo "interrupted operation ${operation_id} reached disallowed state ${state}" >&2
        return 1
        ;;
    esac
    sleep "${delay}"
  done
  echo "interrupted operation ${operation_id} did not converge; final state=${state}" >&2
  return 1
}

sampler_is_running() {
  [[ -n "${SAMPLE_PID:-}" ]] && jobs -pr | grep -Fxq "${SAMPLE_PID}"
}

reap_sampler() {
  local status
  if [[ "${SAMPLER_REAPED:-0}" == 1 || -z "${SAMPLE_PID:-}" ]]; then
    return "${SAMPLER_EXIT:-0}"
  fi
  if wait "${SAMPLE_PID}"; then status=0; else status=$?; fi
  SAMPLER_EXIT=${status}
  SAMPLER_REAPED=1
  return "${status}"
}

check_sampler() {
  if sampler_is_running; then
    return 0
  fi
  reap_sampler || true
  if [[ "${SAMPLER_STOP_REQUESTED:-0}" == 1 && "${SAMPLER_EXIT:-1}" == 0 ]]; then
    return 0
  fi
  echo "resource sampler exited unexpectedly with status ${SAMPLER_EXIT:-1}" >&2
  return 1
}

stop_sampler() {
  if [[ -z "${SAMPLE_PID:-}" ]]; then
    return 0
  fi
  SAMPLER_STOP_REQUESTED=1
  if sampler_is_running; then
    kill -TERM "${SAMPLE_PID}" 2>/dev/null || true
  fi
  if ! reap_sampler; then
    echo "resource sampler failed during active stop with status ${SAMPLER_EXIT}" >&2
    return 1
  fi
  SAMPLE_PID=""
}

validate_resource_samples() {
  local samples=$1 summary=$2 minimum_samples=$3 minimum_span=$4 sampler_exit=$5
  local required_phases=${6:-${I08_REQUIRED_SAMPLE_PHASES}}
  local total_run_seconds=${7:-0}
  jq -se --arg phases "${required_phases}" --argjson minimum_samples "${minimum_samples}" \
    --argjson minimum_span "${minimum_span}" --argjson sampler_exit "${sampler_exit}" \
    --argjson total_run_seconds "${total_run_seconds}" '
    def number: type == "number";
    def valid_sample:
      (.at | type == "string" and length > 0) and
      (.epoch_seconds | number) and (.phase | type == "string" and length > 0) and
      (.runtime.goroutines | number) and (.runtime.db_acquired | number) and
      (.runtime.db_idle | number) and (.runtime.db_total | number) and
      (.stub.active_tasks | number) and (.stub.task_capacity | number) and
      (.control_rss_kib | number) and (.control_fd | number) and
      (.stub_rss_kib | number) and (.stub_fd | number) and
      (.postgres.active | number) and (.postgres.waiting | number) and
      (.postgres.available | type == "boolean");
    ($phases | split(",")) as $required |
    (reduce .[] as $sample ({}; .[$sample.phase] = ((.[$sample.phase] // 0) + 1))) as $coverage |
    if length < $minimum_samples then error("insufficient resource samples")
    elif any(.[]; valid_sample | not) then error("invalid resource sample")
    elif (.[-1].epoch_seconds - .[0].epoch_seconds) < $minimum_span then error("resource sample span is too short")
    elif any($required[]; ($coverage[.] // 0) == 0) then error("required phase is missing")
    else
      {
        samples: length,
        first_timestamp: .[0].at,
        last_timestamp: .[-1].at,
        sample_span_seconds: (.[-1].epoch_seconds - .[0].epoch_seconds),
        total_run_seconds: $total_run_seconds,
        phase_coverage: $coverage,
        max_goroutines: (map(.runtime.goroutines) | max),
        max_tokio_tasks: (map(.stub.active_tasks) | max),
        max_control_rss_kib: (map(.control_rss_kib) | max),
        max_stub_rss_kib: (map(.stub_rss_kib) | max),
        max_control_fd: (map(.control_fd) | max),
        max_stub_fd: (map(.stub_fd) | max),
        max_db_acquired: (map(.runtime.db_acquired) | max),
        max_db_idle: (map(.runtime.db_idle) | max),
        max_db_total: (map(.runtime.db_total) | max),
        sampler_exit: $sampler_exit
      }
    end
  ' "${samples}" >"${summary}"
}
