#!/usr/bin/env bash
set -euo pipefail

# The allowlist covers every harness that rendezvous through this helper:
# the real-E2E controller/node pair, the G6 HA/PITR failure-domain pair, and
# the G6 readiness failure-domain pair. Names stay closed — a new artifact
# name is a deliberate contract change.
validate_name() {
  local name="${1:-}"
  [[ -n "${GITHUB_RUN_ID:-}" && -n "${GITHUB_RUN_ATTEMPT:-}" ]] || {
    echo "GITHUB_RUN_ID and GITHUB_RUN_ATTEMPT are required" >&2
    return 2
  }
  [[ "${name}" =~ ^(real-e2e-(controller-ready|agent-endpoint|enrollment-token|enrollment-result)|g6-ha-(tunnel-fd-a|tunnel-fd-b|primary-up|standby|load|failover-ready|isolation|new-primary|pitr|post-promotion|fd-a-recovered|fd-a-rejoin|evidence)|g6-rd-(tunnel-fd-a|tunnel-fd-b|shared|primary-up|agents|agents-enrolled-fd-b|trust-ready|load-active|isolation|new-primary|fd-a-ready|final-freeze|fd-a-evidence|evidence-bundle|fd-a-diagnostics|fd-b-diagnostics|verdict))-[0-9]+-[0-9]+$ ]] || {
    echo "invalid real E2E artifact name" >&2
    return 2
  }
  [[ "${name}" == *-"${GITHUB_RUN_ID}"-"${GITHUB_RUN_ATTEMPT}" ]] || {
    echo "artifact name does not belong to this run attempt" >&2
    return 2
  }
}

github_json() {
  local url="${1:?GitHub API URL is required}"
  curl --fail --silent --show-error \
    --connect-timeout 5 \
    --max-time 20 \
    --retry 2 \
    --retry-all-errors \
    --retry-delay 1 \
    --retry-max-time 30 \
    --header "Authorization: Bearer ${GITHUB_TOKEN}" \
    --header "Accept: application/vnd.github+json" \
    --header "X-GitHub-Api-Version: 2022-11-28" \
    "${url}"
}

download_artifact() {
  local url="${1:?artifact download URL is required}"
  local output="${2:?artifact output path is required}"
  curl --fail --silent --show-error --location \
    --connect-timeout 5 \
    --max-time 120 \
    --retry 3 \
    --retry-all-errors \
    --retry-delay 2 \
    --retry-max-time 180 \
    --header "Authorization: Bearer ${GITHUB_TOKEN}" \
    --header "Accept: application/vnd.github+json" \
    --header "X-GitHub-Api-Version: 2022-11-28" \
    --output "${output}" "${url}"
}

# G6 readiness is the only artifact rendezvous where both sides execute as
# independent concurrent jobs. Map the current job to its peer so a failed
# side cannot leave the other side polling for a never-to-be-created artifact.
peer_job_name() {
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
  local peer response state
  peer="$(peer_job_name)" || return 0
  if ! response="$(github_json \
    "${GITHUB_API_URL}/repos/${GITHUB_REPOSITORY}/actions/runs/${GITHUB_RUN_ID}/attempts/${GITHUB_RUN_ATTEMPT}/jobs?per_page=100")"; then
    echo "warning: unable to inspect peer job ${peer}; artifact polling will continue" >&2
    return 0
  fi
  if ! jq -e '.jobs | type == "array"' >/dev/null 2>&1 <<<"${response}"; then
    echo "warning: GitHub returned an invalid peer-job response; artifact polling will continue" >&2
    return 0
  fi
  state="$(jq -c --arg name "${peer}" '
    [.jobs[] | select(.name == $name)] | sort_by(.id) | last |
    if . == null then empty else {
      status: (.status // ""),
      conclusion: (.conclusion // ""),
      failed_step: (
        [.steps[]? |
          select(
            (.conclusion // "") == "failure" or
            (.conclusion // "") == "cancelled" or
            (.conclusion // "") == "timed_out" or
            (.conclusion // "") == "action_required" or
            (.conclusion // "") == "startup_failure" or
            (.conclusion // "") == "stale"
          ) |
          {name: (.name // "unknown"), conclusion: (.conclusion // "unknown")}
        ] | first // null
      )
    } end
  ' <<<"${response}")"
  [[ -n "${state}" ]] && printf '%s\n' "${state}"
}

wait_download() {
  local name="${1:?artifact name is required}"
  local destination="${2:?destination is required}"
  local timeout_seconds="${3:-600}"
  local deadline response download_url archive staging entry
  local next_peer_check peer_state peer_status peer_conclusion
  local failed_step failed_step_conclusion peer_success_seen_at=-1
  validate_name "${name}"
  [[ "${timeout_seconds}" =~ ^[0-9]+$ && "${timeout_seconds}" -ge 1 && "${timeout_seconds}" -le 3600 ]] || {
    echo "artifact wait timeout must be 1..3600 seconds" >&2
    return 2
  }
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

  deadline=$((SECONDS + timeout_seconds))
  next_peer_check="${SECONDS}"
  download_url=""
  while ((SECONDS < deadline)); do
    if response="$(github_json \
      "${GITHUB_API_URL}/repos/${GITHUB_REPOSITORY}/actions/runs/${GITHUB_RUN_ID}/artifacts?per_page=100&name=${name}")"; then
      if jq -e '.artifacts | type == "array"' >/dev/null 2>&1 <<<"${response}"; then
        download_url="$(jq -r --arg name "${name}" \
          '[.artifacts[] | select(.name == $name and .expired == false)] | sort_by(.id) | last | .archive_download_url // empty' \
          <<<"${response}")"
      else
        echo "warning: GitHub returned an invalid artifact response; retrying" >&2
      fi
    else
      echo "warning: GitHub artifact query failed; retrying within the bounded wait" >&2
    fi
    if [[ -n "${download_url}" ]]; then
      break
    fi

    if ((SECONDS >= next_peer_check)); then
      next_peer_check=$((SECONDS + 10))
      peer_state="$(peer_job_state || true)"
      if [[ -n "${peer_state}" ]]; then
        peer_status="$(jq -r '.status' <<<"${peer_state}")"
        peer_conclusion="$(jq -r '.conclusion' <<<"${peer_state}")"
        failed_step="$(jq -r '.failed_step.name // empty' <<<"${peer_state}")"
        failed_step_conclusion="$(jq -r '.failed_step.conclusion // empty' <<<"${peer_state}")"
        if [[ -n "${failed_step}" ]]; then
          echo "peer job $(peer_job_name) failed at step ${failed_step} (${failed_step_conclusion}); refusing to wait for ${name}" >&2
          return 1
        fi
        if [[ "${peer_status}" == completed ]]; then
          if [[ "${peer_conclusion}" != success ]]; then
            echo "peer job $(peer_job_name) completed with ${peer_conclusion:-unknown}; refusing to wait for ${name}" >&2
            return 1
          fi
          if ((peer_success_seen_at < 0)); then
            peer_success_seen_at="${SECONDS}"
          elif ((SECONDS - peer_success_seen_at >= 60)); then
            echo "peer job $(peer_job_name) succeeded without publishing required artifact ${name}" >&2
            return 1
          fi
        else
          peer_success_seen_at=-1
        fi
      fi
    fi
    sleep 5
  done
  [[ -n "${download_url}" ]] || {
    echo "timed out waiting for artifact ${name}" >&2
    return 1
  }

  archive="$(mktemp "${RUNNER_TEMP:-/tmp}/real-e2e-artifact.XXXXXX.zip")"
  staging="$(mktemp -d "${RUNNER_TEMP:-/tmp}/real-e2e-artifact.XXXXXX")"
  cleanup_download() { rm -f -- "${archive}"; rm -rf -- "${staging}"; }
  trap cleanup_download RETURN
  download_artifact "${download_url}" "${archive}"
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
  done < <(unzip -Z1 "${archive}")
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
    wait_download "${2:-}" "${3:-}" "${4:-600}"
    ;;
  *)
    echo "usage: $0 {validate-name NAME|wait-download NAME ABSOLUTE_DESTINATION [TIMEOUT_SECONDS]}" >&2
    exit 2
    ;;
esac
