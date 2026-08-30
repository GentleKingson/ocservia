#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COMPOSE_LAUNCHER="${OCSERV_CONTROLLER_COMPOSE_SH:-${ROOT}/deploy/production/compose.sh}"
PUBLIC_URL="${OCSERV_CONTROLLER_PUBLIC_URL:-}"

usage() {
  echo "usage: $0 --release-file /path/controller-release.json" >&2
  exit 2
}

fail() {
  echo "controller release smoke: $1" >&2
  exit 1
}

if (($# != 2)) || [[ "$1" != "--release-file" ]]; then
  usage
fi

RELEASE_FILE="$2"
[[ "${RELEASE_FILE}" == /* && -f "${RELEASE_FILE}" && ! -L "${RELEASE_FILE}" ]] ||
  fail "release file must be an existing regular file"
[[ "$(realpath "${RELEASE_FILE}")" == "${RELEASE_FILE}" ]] ||
  fail "release file must be a canonical path without symlink ancestry"

command -v jq >/dev/null 2>&1 || fail "jq is required"
command -v curl >/dev/null 2>&1 || fail "curl is required"
[[ -x "${COMPOSE_LAUNCHER}" && ! -L "${COMPOSE_LAUNCHER}" ]] ||
  fail "production Compose launcher is missing or not executable"
[[ "${COMPOSE_LAUNCHER}" == /* && "$(realpath "${COMPOSE_LAUNCHER}")" == "${COMPOSE_LAUNCHER}" ]] ||
  fail "production Compose launcher must be a canonical path"

jq -e -s '
  length == 1 and
  (.[0] | type == "object" and
    (.release_version | type == "string") and
    (.source_commit | type == "string"))
' "${RELEASE_FILE}" >/dev/null || fail "release file is invalid"
expected_version="$(jq -er '.release_version' "${RELEASE_FILE}")"
expected_commit="$(jq -er '.source_commit' "${RELEASE_FILE}")"
[[ "${expected_commit}" =~ ^[0-9a-f]{40}$ ]] || fail "release source_commit is invalid"

if [[ -z "${PUBLIC_URL}" ]]; then
  [[ -n "${OCSERV_PUBLIC_HOST:-}" ]] || fail "OCSERV_PUBLIC_HOST or OCSERV_CONTROLLER_PUBLIC_URL is required"
  PUBLIC_URL="https://${OCSERV_PUBLIC_HOST}"
fi
PUBLIC_URL="${PUBLIC_URL%/}"
[[ "${PUBLIC_URL}" =~ ^https://[^[:space:]]+$ ]] ||
  fail "public Controller URL must use HTTPS"

health_json="$("${COMPOSE_LAUNCHER}" ps --format json postgres control-plane transportd backup)" ||
  fail "cannot inspect Compose health"
jq -s -e '
  if type != "array" then false
  else
    . as $services |
    ["postgres", "control-plane", "transportd", "backup"] |
    all(.[];
      . as $service |
      any($services[]; .Service == $service and .State == "running" and .Health == "healthy"))
  end
' <<<"${health_json}" >/dev/null || fail "required Compose services are not healthy"

request_json() {
  local path="$1"
  curl --fail --silent --show-error --proto '=https' --tlsv1.2 \
    --connect-timeout 5 --max-time 15 "${PUBLIC_URL}${path}"
}

ready_json="$(request_json /api/v1/readyz)" || fail "public Controller readiness probe failed"
jq -e 'type == "object" and .status == "ok"' <<<"${ready_json}" >/dev/null ||
  fail "public Controller readiness response is invalid"

version_json="$(request_json /api/v1/version)" || fail "public Controller version probe failed"
jq -e --arg expected_version "${expected_version}" --arg expected_commit "${expected_commit}" '
  type == "object" and
  .version == $expected_version and
  .commit == $expected_commit
' <<<"${version_json}" >/dev/null ||
  fail "running Controller identity does not match the target release"

echo "Controller release smoke passed (${expected_version})"
