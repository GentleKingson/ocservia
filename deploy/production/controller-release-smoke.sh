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
    (.source_commit | type == "string") and
    (.images | type == "object"))
' "${RELEASE_FILE}" >/dev/null || fail "release file is invalid"
expected_version="$(jq -er '.release_version' "${RELEASE_FILE}")"
expected_commit="$(jq -er '.source_commit' "${RELEASE_FILE}")"
[[ "${expected_commit}" =~ ^[0-9a-f]{40}$ ]] || fail "release source_commit is invalid"

for image in gateway control transport backup postgres otel; do
  case "${image}" in
    gateway) variable=OCSERV_GATEWAY_IMAGE ;;
    control) variable=OCSERV_CONTROL_IMAGE ;;
    transport) variable=OCSERV_TRANSPORT_IMAGE ;;
    backup) variable=OCSERV_BACKUP_IMAGE ;;
    postgres) variable=OCSERV_POSTGRES_IMAGE ;;
    otel) variable=OCSERV_OTEL_IMAGE ;;
  esac
  if ! value="$(jq -er --arg image "${image}" '.images[$image]' "${RELEASE_FILE}")" ||
    [[ ! "${value}" =~ ^[^[:space:]@]+@sha256:[0-9a-f]{64}$ ]]; then
    fail "release image ${image} is invalid"
  fi
  export "${variable}=${value}"
done
if [[ "$(jq -r '.manifest_version' "${RELEASE_FILE}")" == 2 ]]; then
  for image in edge relay signer; do
    value="$(jq -er --arg image "${image}" '.images[$image]' "${RELEASE_FILE}")"
    [[ "${value}" =~ ^[^[:space:]@]+@sha256:[0-9a-f]{64}$ ]] || fail "invalid ${image} digest"
    export "OCSERV_${image^^}_IMAGE=${value}"
  done
  case "${OCSERV_DATABASE_BACKEND:-postgres}" in
    mysql|mariadb)
      OCSERV_DATABASE_BACKUP_IMAGE="$(jq -er --arg role "${OCSERV_DATABASE_BACKEND}_backup" '.images[$role]' "${RELEASE_FILE}")"
      export OCSERV_DATABASE_BACKUP_IMAGE
      ;;
  esac
fi

if [[ -z "${PUBLIC_URL}" ]]; then
  [[ -n "${OCSERV_PUBLIC_HOST:-}" ]] || fail "OCSERV_PUBLIC_HOST or OCSERV_CONTROLLER_PUBLIC_URL is required"
  PUBLIC_URL="https://${OCSERV_PUBLIC_HOST}"
fi
PUBLIC_URL="${PUBLIC_URL%/}"
[[ "${PUBLIC_URL}" =~ ^https://[^[:space:]]+$ ]] ||
  fail "public Controller URL must use HTTPS"

services=(control-plane transportd backup)
if [[ "${OCSERV_DATABASE_BACKEND:-postgres}:${OCSERV_DATABASE_DEPLOYMENT:-bundled}" == postgres:bundled ]]; then
  services=(postgres control-plane transportd backup)
fi
if [[ "${OCSERV_DEPLOYMENT_MODE:-standalone}" == integrated ]]; then
  services+=(relay signer)
fi
health_json="$("${COMPOSE_LAUNCHER}" ps --format json "${services[@]}")" ||
  fail "cannot inspect Compose health"
required_json="$(printf '%s\n' "${services[@]}" | jq -R . | jq -s .)"
jq -s -e --argjson required "${required_json}" '
  if type != "array" then false
  else
    . as $services |
    $required | all(.[];
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
