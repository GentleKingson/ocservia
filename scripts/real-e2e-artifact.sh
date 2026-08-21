#!/usr/bin/env bash
set -euo pipefail

# The allowlist covers every harness that rendezvous through this helper:
# the real-E2E controller/node pair, the G6 HA/PITR failure-domain pair, and
# the G6 readiness failure-domain pair. Names stay closed - a new artifact
# name is a deliberate contract change.
validate_name() {
  local name="${1:-}"
  [[ -n "${GITHUB_RUN_ID:-}" && -n "${GITHUB_RUN_ATTEMPT:-}" ]] || {
    echo "GITHUB_RUN_ID and GITHUB_RUN_ATTEMPT are required" >&2
    return 2
  }
  [[ "${name}" =~ ^(real-e2e-(controller-ready|agent-endpoint|enrollment-token|enrollment-result)|g6-ha-(tunnel-fd-a|tunnel-fd-b|primary-up|standby|load|failover-ready|isolation|new-primary|pitr|post-promotion|fd-a-recovered|fd-a-rejoin|evidence)|g6-rd-(tunnel-fd-a|tunnel-fd-b|shared|primary-up|agents|agents-enrolled-fd-b|trust-ready|load-active|isolation|new-primary|relay-rejoin-ready|relay-pre-fault|fd-a-ready|window-barrier-arm-request|window-barrier-armed-fd-a|final-freeze|fd-a-evidence|evidence-bundle|fd-a-diagnostics|fd-b-diagnostics|verdict))-[0-9]+-[0-9]+$ ]] || {
    echo "invalid real E2E artifact name" >&2
    return 2
  }
  [[ "${name}" == *-"${GITHUB_RUN_ID}"-"${GITHUB_RUN_ATTEMPT}" ]] || {
    echo "artifact name does not belong to this run attempt" >&2
    return 2
  }
}

validate_positive_integer() {
  local value="${1:-}" description="${2:?description is required}" maximum="${3:?maximum is required}"
  [[ "${value}" =~ ^[0-9]+$ && "${value}" -ge 1 && "${value}" -le "${maximum}" ]] || {
    echo "${description} must be 1..${maximum}" >&2
    return 2
  }
}

validate_http_budget() {
  validate_positive_integer "${REAL_E2E_ARTIFACT_CONNECT_TIMEOUT_SECONDS:-5}" \
    "artifact API connect timeout" 30
  validate_positive_integer "${REAL_E2E_ARTIFACT_API_TIMEOUT_SECONDS:-20}" \
    "artifact API total timeout" 60
  validate_positive_integer "${REAL_E2E_ARTIFACT_DOWNLOAD_TIMEOUT_SECONDS:-120}" \
    "artifact download total timeout" 600
  validate_positive_integer "${REAL_E2E_ARTIFACT_DOWNLOAD_RETRY_TOTAL_SECONDS:-90}" \
    "artifact download retry total" 300
  [[ "${REAL_E2E_ARTIFACT_RETRIES:-2}" =~ ^[0-9]+$ \
    && "${REAL_E2E_ARTIFACT_RETRIES:-2}" -le 5 ]] || {
    echo "artifact API retries must be 0..5" >&2
    return 2
  }
  validate_positive_integer "${REAL_E2E_ARTIFACT_RETRY_TOTAL_SECONDS:-30}" \
    "artifact API retry total" 180
  validate_positive_integer "${REAL_E2E_ARTIFACT_MAX_CONSECUTIVE_ERRORS:-3}" \
    "artifact API consecutive error limit" 10
  validate_positive_integer "${REAL_E2E_ARTIFACT_POLL_INTERVAL_SECONDS:-5}" \
    "artifact poll interval" 30
  validate_positive_integer "${REAL_E2E_ARTIFACT_PROPAGATION_GRACE_SECONDS:-30}" \
    "artifact propagation grace" 300
}

github_get() {
  local url="${1:?GitHub API URL is required}"
  local max_time="${2:?request maximum time is required}"
  local retry_max_time="${REAL_E2E_ARTIFACT_RETRY_TOTAL_SECONDS:-30}"
  ((retry_max_time <= max_time)) || retry_max_time="${max_time}"
  curl --fail --silent --show-error --location \
    --connect-timeout "${REAL_E2E_ARTIFACT_CONNECT_TIMEOUT_SECONDS:-5}" \
    --max-time "${max_time}" \
    --retry "${REAL_E2E_ARTIFACT_RETRIES:-2}" \
    --retry-delay 1 \
    --retry-max-time "${retry_max_time}" \
    --retry-all-errors \
    --header "Authorization: Bearer ${GITHUB_TOKEN}" \
    --header "Accept: application/vnd.github+json" \
    --header "X-GitHub-Api-Version: 2022-11-28" \
    "${url}"
}

download_artifact() {
  local url="${1:?artifact download URL is required}"
  local archive="${2:?artifact archive path is required}"
  local deadline remaining request_timeout
  local failures=0
  local retry_total="${REAL_E2E_ARTIFACT_DOWNLOAD_RETRY_TOTAL_SECONDS:-90}"
  local poll_interval="${REAL_E2E_ARTIFACT_POLL_INTERVAL_SECONDS:-5}"
  deadline=$((SECONDS + retry_total))

  # The redirect/download service can return a longer 5xx burst than the
  # metadata APIs. Retry for its dedicated wall-clock budget rather than
  # reusing the polling error count; the deadline and each curl invocation
  # remain independently bounded.
  while ((SECONDS < deadline)); do
    remaining=$((deadline - SECONDS))
    request_timeout="${REAL_E2E_ARTIFACT_DOWNLOAD_TIMEOUT_SECONDS:-120}"
    ((request_timeout <= remaining)) || request_timeout="${remaining}"
    : >"${archive}"
    if github_get "${url}" "${request_timeout}" >"${archive}"; then
      return 0
    fi
    failures=$((failures + 1))
    if ((SECONDS < deadline)); then
      remaining=$((deadline - SECONDS))
      ((poll_interval <= remaining)) || poll_interval="${remaining}"
      ((poll_interval > 0)) && sleep "${poll_interval}"
    fi
  done

  echo "artifact download failed after ${failures} bounded requests within its ${retry_total}-second retry window" >&2
  return 1
}

default_peer_job_name() {
  if [[ -n "${REAL_E2E_ARTIFACT_PEER_JOB_NAME:-}" ]]; then
    printf '%s\n' "${REAL_E2E_ARTIFACT_PEER_JOB_NAME}"
    return 0
  fi
  case "${GITHUB_JOB:-}" in
    g6-rd-fd-a) printf '%s\n' 'G6 Readiness Failure Domain B' ;;
    g6-rd-fd-b) printf '%s\n' 'G6 Readiness Failure Domain A' ;;
    *) return 1 ;;
  esac
}

peer_job_state() {
  local response="${1:?jobs API response is required}"
  local peer_job="${2:?peer job name is required}"
  local count failed_step failed_conclusion failed_step_json status conclusion
  count="$(jq -er --arg peer "${peer_job}" \
    '[.jobs[] | select(.name == $peer)] | length' <<<"${response}")" || return 2
  if [[ "${count}" != 1 ]]; then
    echo "expected exactly one peer job named ${peer_job}, found ${count}" >&2
    return 2
  fi
  failed_step_json="$(jq -c --arg peer "${peer_job}" '
    [.jobs[] | select(.name == $peer) | .steps[]? |
      select(.status == "completed" and
        (.conclusion | IN(
          "failure", "cancelled", "timed_out", "action_required",
          "startup_failure", "stale"
        ))) |
      {name, conclusion}] | first // {}
  ' <<<"${response}")" || return 2
  failed_step="$(jq -r '.name // empty' <<<"${failed_step_json}")" || return 2
  failed_conclusion="$(jq -r '.conclusion // empty' <<<"${failed_step_json}")" || return 2
  if [[ -n "${failed_step}" ]]; then
    echo "peer job ${peer_job} failed at step ${failed_step} (${failed_conclusion}); refusing to wait" >&2
    return 1
  fi
  read -r status conclusion < <(jq -r --arg peer "${peer_job}" \
    '.jobs[] | select(.name == $peer) | [.status, (.conclusion // "")] | @tsv' \
    <<<"${response}")
  if [[ "${status}" == completed && "${conclusion}" != success ]]; then
    echo "peer job ${peer_job} completed with conclusion ${conclusion:-unknown}; refusing to wait" >&2
    return 1
  fi
  if [[ "${status}" == completed ]]; then
    printf '%s\n' success
  else
    printf '%s\n' running
  fi
}

wait_download() {
  local name="${1:?artifact name is required}"
  local destination="${2:?destination is required}"
  local timeout_seconds="${3:-600}"
  local peer_job="${4:-}"
  local deadline response jobs_response artifact_id download_url archive staging entry listing
  local artifact_api_errors=0 jobs_api_errors=0 peer_success_at=0
  local peer_state peer_state_status poll_interval max_consecutive_errors propagation_grace
  local peer_checked
  validate_name "${name}"
  validate_positive_integer "${timeout_seconds}" "artifact wait timeout" 3600
  validate_http_budget
  [[ -n "${GITHUB_TOKEN:-}" && -n "${GITHUB_REPOSITORY:-}" && -n "${GITHUB_API_URL:-}" ]] || {
    echo "GitHub artifact API environment is incomplete" >&2
    return 2
  }
  [[ "${destination}" == /* ]] || {
    echo "artifact destination must be absolute" >&2
    return 2
  }
  if [[ -d "${destination}" ]] && find "${destination}" -mindepth 1 -print -quit | grep -q .; then
    echo "artifact destination must be empty" >&2
    return 2
  fi
  if [[ -z "${peer_job}" ]]; then
    peer_job="$(default_peer_job_name || true)"
  fi
  if [[ -n "${peer_job}" && ("${peer_job}" == *$'\n'* || ${#peer_job} -gt 128) ]]; then
    echo "peer job name is invalid" >&2
    return 2
  fi

  deadline=$((SECONDS + timeout_seconds))
  poll_interval="${REAL_E2E_ARTIFACT_POLL_INTERVAL_SECONDS:-5}"
  max_consecutive_errors="${REAL_E2E_ARTIFACT_MAX_CONSECUTIVE_ERRORS:-3}"
  propagation_grace="${REAL_E2E_ARTIFACT_PROPAGATION_GRACE_SECONDS:-30}"
  artifact_id=""
  while ((SECONDS < deadline)); do
    peer_checked=0
    if response="$(github_get \
      "${GITHUB_API_URL}/repos/${GITHUB_REPOSITORY}/actions/runs/${GITHUB_RUN_ID}/artifacts?per_page=100&name=${name}" \
      "${REAL_E2E_ARTIFACT_API_TIMEOUT_SECONDS:-20}")"; then
      if ! jq -e '.artifacts | type == "array"' <<<"${response}" >/dev/null 2>&1; then
        echo "artifact API returned an invalid JSON document" >&2
        return 1
      fi
      artifact_id="$(jq -r --arg name "${name}" '
        [.artifacts[] |
          select(.name == $name and .expired == false and (.id | type == "number"))]
        | sort_by(.id) | last | .id // empty
      ' <<<"${response}")"
      artifact_api_errors=0
    else
      artifact_api_errors=$((artifact_api_errors + 1))
      if ((artifact_api_errors >= max_consecutive_errors)); then
        echo "artifact API failed ${artifact_api_errors} consecutive bounded requests" >&2
        return 1
      fi
    fi

    if [[ -n "${peer_job}" ]]; then
      if jobs_response="$(github_get \
        "${GITHUB_API_URL}/repos/${GITHUB_REPOSITORY}/actions/runs/${GITHUB_RUN_ID}/attempts/${GITHUB_RUN_ATTEMPT}/jobs?per_page=100" \
        "${REAL_E2E_ARTIFACT_API_TIMEOUT_SECONDS:-20}")"; then
        if ! jq -e '.jobs | type == "array"' <<<"${jobs_response}" >/dev/null 2>&1; then
          echo "jobs API returned an invalid JSON document" >&2
          return 1
        fi
        if peer_state="$(peer_job_state "${jobs_response}" "${peer_job}")"; then
          :
        else
          peer_state_status=$?
          return "${peer_state_status}"
        fi
        jobs_api_errors=0
        peer_checked=1
        if [[ "${peer_state}" == success && -z "${artifact_id}" ]]; then
          ((peer_success_at > 0)) || peer_success_at=${SECONDS}
          if ((SECONDS - peer_success_at >= propagation_grace)); then
            echo "peer job ${peer_job} succeeded but did not publish artifact ${name} within the propagation grace period" >&2
            return 1
          fi
        fi
      else
        jobs_api_errors=$((jobs_api_errors + 1))
        if ((jobs_api_errors >= max_consecutive_errors)); then
          echo "jobs API failed ${jobs_api_errors} consecutive bounded requests" >&2
          return 1
        fi
      fi
    fi
    if [[ -n "${artifact_id}" && (-z "${peer_job}" || "${peer_checked}" -eq 1) ]]; then
      break
    fi
    sleep "${poll_interval}"
  done
  [[ -n "${artifact_id}" ]] || {
    echo "timed out waiting for artifact ${name}" >&2
    return 1
  }
  download_url="${GITHUB_API_URL}/repos/${GITHUB_REPOSITORY}/actions/artifacts/${artifact_id}/zip"

  archive="$(mktemp "${RUNNER_TEMP:-/tmp}/real-e2e-artifact.XXXXXX.zip")"
  staging="$(mktemp -d "${RUNNER_TEMP:-/tmp}/real-e2e-artifact.XXXXXX")"
  cleanup_download() { rm -f -- "${archive}"; rm -rf -- "${staging}"; }
  trap cleanup_download RETURN
  download_artifact "${download_url}" "${archive}"
  if ! listing="$(unzip -Z1 "${archive}")"; then
    echo "artifact download is not a valid ZIP archive" >&2
    return 1
  fi
  while IFS= read -r entry; do
    [[ -n "${entry}" && "${entry}" != /* && "${entry}" != *\\* ]] || {
      echo "artifact contains an unsafe member name" >&2
      return 1
    }
    IFS=/ read -r -a parts <<<"${entry}"
    for part in "${parts[@]}"; do
      [[ "${part}" != ".." ]] || {
        echo "artifact contains path traversal" >&2
        return 1
      }
    done
  done <<<"${listing}"
  if unzip -Z -l "${archive}" | awk '
    substr($1, 1, 1) == "l" { found = 1 }
    END { exit(found ? 0 : 1) }
  '; then
    echo "artifact contains a symbolic link" >&2
    return 1
  fi
  unzip -q "${archive}" -d "${staging}"
  if find "${staging}" -type l -print -quit | grep -q .; then
    echo "artifact contains a symbolic link" >&2
    return 1
  fi
  mkdir -p -- "${destination}"
  cp -a -- "${staging}/." "${destination}/"
  trap - RETURN
  cleanup_download
}

case "${1:-}" in
  validate-name)
    validate_name "${2:-}"
    ;;
  wait-download)
    wait_download "${2:-}" "${3:-}" "${4:-600}" "${5:-}"
    ;;
  *)
    echo "usage: $0 {validate-name NAME|wait-download NAME ABSOLUTE_DESTINATION [TIMEOUT_SECONDS [PEER_JOB_NAME]]}" >&2
    exit 2
    ;;
esac
