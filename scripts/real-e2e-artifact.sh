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
  [[ "${name}" =~ ^(real-e2e-(controller-ready|agent-endpoint|enrollment-token|enrollment-result)|g6-ha-(tunnel-fd-a|tunnel-fd-b|primary-up|standby|load|failover-ready|isolation|new-primary|pitr|post-promotion|fd-a-recovered|fd-a-rejoin|evidence)|g6-rd-(tunnel-fd-a|tunnel-fd-b|shared|primary-up|agents|load-active|isolation|new-primary|fd-a-evidence|evidence-bundle|fd-a-diagnostics|fd-b-diagnostics|verdict))-[0-9]+-[0-9]+$ ]] || {
    echo "invalid real E2E artifact name" >&2
    return 2
  }
  [[ "${name}" == *-"${GITHUB_RUN_ID}"-"${GITHUB_RUN_ATTEMPT}" ]] || {
    echo "artifact name does not belong to this run attempt" >&2
    return 2
  }
}

wait_download() {
  local name="${1:?artifact name is required}"
  local destination="${2:?destination is required}"
  local timeout_seconds="${3:-600}"
  local deadline response download_url archive staging entry
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
  while ((SECONDS < deadline)); do
    response="$(curl --fail --silent --show-error \
      --header "Authorization: Bearer ${GITHUB_TOKEN}" \
      --header "Accept: application/vnd.github+json" \
      --header "X-GitHub-Api-Version: 2022-11-28" \
      "${GITHUB_API_URL}/repos/${GITHUB_REPOSITORY}/actions/runs/${GITHUB_RUN_ID}/artifacts?per_page=100")"
    download_url="$(jq -r --arg name "${name}" \
      '[.artifacts[] | select(.name == $name and .expired == false)] | sort_by(.id) | last | .archive_download_url // empty' \
      <<<"${response}")"
    if [[ -n "${download_url}" ]]; then
      break
    fi
    sleep 5
  done
  [[ -n "${download_url:-}" ]] || {
    echo "timed out waiting for artifact ${name}" >&2
    return 1
  }

  archive="$(mktemp "${RUNNER_TEMP:-/tmp}/real-e2e-artifact.XXXXXX.zip")"
  staging="$(mktemp -d "${RUNNER_TEMP:-/tmp}/real-e2e-artifact.XXXXXX")"
  cleanup_download() { rm -f -- "${archive}"; rm -rf -- "${staging}"; }
  trap cleanup_download RETURN
  curl --fail --silent --show-error --location \
    --header "Authorization: Bearer ${GITHUB_TOKEN}" \
    --header "Accept: application/vnd.github+json" \
    --header "X-GitHub-Api-Version: 2022-11-28" \
    --output "${archive}" "${download_url}"
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
