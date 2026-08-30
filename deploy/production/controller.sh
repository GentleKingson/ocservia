#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
STATE_ROOT="${OCSERV_CONTROLLER_STATE_ROOT:-${OCSERV_CONTROLLER_STATE_DIR:-/var/lib/ocservia-controller}}"
COMPOSE_LAUNCHER="${OCSERV_CONTROLLER_COMPOSE_SH:-${ROOT}/deploy/production/compose.sh}"
CURRENT_RELEASE=""
PENDING_RELEASE=""
STAGED_RELEASE=""
CANONICAL_RELEASE=""

umask 077

usage() {
  echo "usage: $0 install --release-file /path/controller-release.json" >&2
  exit 2
}

fail() {
  echo "controller lifecycle: $1" >&2
  exit "${2:-2}"
}

require_absolute_canonical_path() {
  local label="$1" path="$2" resolved
  [[ "${path}" == /* ]] || fail "${label} must be an absolute path"
  case "${path}" in
    /|*/|*/../*|*/..|*/./*|*/.) fail "${label} must be a canonical path without traversal" ;;
  esac
  resolved="$(realpath -e -- "${path}")" || fail "${label} does not exist"
  [[ "${resolved}" == "${path}" ]] || fail "${label} must not contain symlink ancestry"
}

validate_ancestor() {
  local ancestor="$1" ancestor_uid ancestor_mode
  while true; do
    [[ "$(realpath -e -- "${ancestor}")" == "${ancestor}" ]] ||
      fail "state path ancestry must not contain symlinks"
    IFS=: read -r ancestor_uid ancestor_mode < <(stat -c '%u:%a' "${ancestor}")
    [[ "${ancestor_uid}" == "0" || "${ancestor_uid}" == "$(id -u)" ]] ||
      fail "state path ancestry must be root- or launcher-owned"
    (( (8#${ancestor_mode} & 8#022) == 0 )) ||
      fail "state path ancestry must not be group/world writable"
    [[ "${ancestor}" == "/" ]] && break
    ancestor="$(dirname -- "${ancestor}")"
  done
}

prepare_state_root() {
  local parent state_uid state_mode
  [[ "${STATE_ROOT}" == /* ]] || fail "state root must be an absolute path"
  case "${STATE_ROOT}" in
    /|*/|*/../*|*/..|*/./*|*/.) fail "state root must be a canonical path without traversal" ;;
  esac

  if [[ -L "${STATE_ROOT}" ]]; then
    fail "state root must not be a symlink"
  elif [[ -e "${STATE_ROOT}" ]]; then
    [[ -d "${STATE_ROOT}" ]] || fail "state root must be a directory"
  else
    parent="$(dirname -- "${STATE_ROOT}")"
    require_absolute_canonical_path "state root parent" "${parent}"
    validate_ancestor "${parent}"
    mkdir -m 700 -- "${STATE_ROOT}" || fail "cannot create state root"
  fi

  require_absolute_canonical_path "state root" "${STATE_ROOT}"
  validate_ancestor "${STATE_ROOT}"
  IFS=: read -r state_uid state_mode < <(stat -c '%u:%a' "${STATE_ROOT}")
  [[ "${state_uid}" == "$(id -u)" && "${state_mode}" == "700" ]] ||
    fail "state root must be owned by the launcher user with mode 0700"
}

acquire_lock() {
  local lock_file="${STATE_ROOT}/lifecycle.lock" lock_uid lock_mode
  if [[ -L "${lock_file}" || ( -e "${lock_file}" && ! -f "${lock_file}" ) ]]; then
    fail "lifecycle lock must be a regular file and not a symlink"
  fi
  if [[ ! -e "${lock_file}" ]]; then
    : >"${lock_file}" || fail "cannot create lifecycle lock"
  fi
  IFS=: read -r lock_uid lock_mode < <(stat -c '%u:%a' "${lock_file}")
  [[ "${lock_uid}" == "$(id -u)" && "${lock_mode}" == "600" ]] ||
    fail "lifecycle lock must be launcher-owned with mode 0600"
  command -v flock >/dev/null 2>&1 || fail "flock is required for lifecycle locking"
  exec 9>>"${lock_file}"
  if ! flock -n 9; then
    fail "another Controller lifecycle command is already running" 75
  fi
}

validate_release_file_path() {
  local path="$1" release_uid release_mode
  require_absolute_canonical_path "release file" "${path}"
  [[ -f "${path}" && ! -L "${path}" ]] || fail "release file must be a regular file and not a symlink"
  IFS=: read -r release_uid release_mode < <(stat -c '%u:%a' "${path}")
  [[ "${release_uid}" == "0" || "${release_uid}" == "$(id -u)" ]] ||
    fail "release file must be root- or launcher-owned"
  (( (8#${release_mode} & 8#022) == 0 )) ||
    fail "release file must not be group/world writable"
}

validate_state_file_path() {
  local label="$1" path="$2" state_uid state_mode
  if [[ -L "${path}" ]]; then
    fail "${label} must not be a symlink"
  fi
  [[ -f "${path}" ]] || fail "${label} must be a regular file"
  require_absolute_canonical_path "${label}" "${path}"
  IFS=: read -r state_uid state_mode < <(stat -c '%u:%a' "${path}")
  [[ "${state_uid}" == "$(id -u)" && "${state_mode}" == "600" ]] ||
    fail "${label} must be launcher-owned with mode 0600"
}

normalize_release_file_path() {
  local path="${RELEASE_FILE}"
  if [[ "${path}" != /* ]]; then
    while [[ "${path}" == ./* ]]; do
      path="${path#./}"
    done
    case "${path}" in
      ""|.|../*|*/../*|*/..|*/./*|*/.)
        fail "release file must not contain traversal"
        ;;
    esac
    path="$(pwd -P)/${path}"
  fi
  RELEASE_FILE="${path}"
}

validate_source_tree() {
  local manifest_commit current_commit dirty
  manifest_commit="$(jq -er -s '.[0].source_commit' "${STAGED_RELEASE}")"
  if ! current_commit="$(git -C "${ROOT}" rev-parse HEAD 2>/dev/null)"; then
    fail "Controller release must be installed from a Git checkout"
  fi
  [[ "${current_commit}" == "${manifest_commit}" ]] ||
    fail "checkout HEAD does not match release manifest source_commit"
  if ! git -C "${ROOT}" diff --quiet --exit-code -- .; then
    fail "checkout has unstaged changes; refusing to install a release"
  fi
  if ! git -C "${ROOT}" diff --cached --quiet --exit-code -- .; then
    fail "checkout has staged changes; refusing to install a release"
  fi
  dirty="$(git -C "${ROOT}" status --porcelain --untracked-files=all)"
  [[ -z "${dirty}" ]] || fail "checkout has untracked changes"
}

stage_and_validate_manifest() {
  local manifest_filter
  STAGED_RELEASE="$(mktemp "${STATE_ROOT}/.current-release.json.XXXXXX")" ||
    fail "cannot allocate release state staging file"
  chmod 600 "${STAGED_RELEASE}"
  cat -- "${RELEASE_FILE}" >"${STAGED_RELEASE}" || fail "cannot read release file"

  # jq variables in this filter are intentionally not shell variables.
  # shellcheck disable=SC2016
  manifest_filter='
    def matches($pattern): if type == "string" then test($pattern) else false end;
    def positive_integer: if type == "number" then (floor == . and . >= 1 and . <= 9007199254740991) else false end;
    if length != 1 then false
    else
      .[0] as $manifest |
      ($manifest | type == "object") and
      ($manifest | keys == ["database_migration", "images", "manifest_version", "platform", "release_tag", "release_version", "source_commit"]) and
      ($manifest.manifest_version | positive_integer and . == 1) and
      ($manifest.release_version | matches("^[0-9]+\\.[0-9]+\\.[0-9]+$")) and
      ($manifest.release_tag | matches("^v[0-9]+\\.[0-9]+\\.[0-9]+$") and . == ("v" + $manifest.release_version)) and
      ($manifest.source_commit | matches("^[0-9a-f]{40}$")) and
      ($manifest.platform == "linux/amd64") and
      ($manifest.database_migration | positive_integer) and
      ($manifest.images | type == "object" and
        keys == ["backup", "control", "gateway", "otel", "postgres", "transport"] and
        all(.[]; matches("^[^[:space:]@]+@sha256:[0-9a-f]{64}$")))
    end
  '
  jq -e -s "${manifest_filter}" "${STAGED_RELEASE}" >/dev/null ||
    fail "release manifest is invalid"

  CANONICAL_RELEASE="$(mktemp "${STATE_ROOT}/.canonical-release.json.XXXXXX")" ||
    fail "cannot allocate manifest validation file"
  chmod 600 "${CANONICAL_RELEASE}"
  jq -s '.[0]' "${STAGED_RELEASE}" >"${CANONICAL_RELEASE}" ||
    fail "release manifest is not valid JSON"
  cmp -s "${STAGED_RELEASE}" "${CANONICAL_RELEASE}" ||
    fail "release manifest is not in canonical form"
}

map_manifest_images() {
  OCSERV_GATEWAY_IMAGE="$(jq -er -s '.[0].images.gateway' "${STAGED_RELEASE}")"
  OCSERV_CONTROL_IMAGE="$(jq -er -s '.[0].images.control' "${STAGED_RELEASE}")"
  OCSERV_TRANSPORT_IMAGE="$(jq -er -s '.[0].images.transport' "${STAGED_RELEASE}")"
  OCSERV_BACKUP_IMAGE="$(jq -er -s '.[0].images.backup' "${STAGED_RELEASE}")"
  OCSERV_POSTGRES_IMAGE="$(jq -er -s '.[0].images.postgres' "${STAGED_RELEASE}")"
  OCSERV_OTEL_IMAGE="$(jq -er -s '.[0].images.otel' "${STAGED_RELEASE}")"
  export OCSERV_GATEWAY_IMAGE OCSERV_CONTROL_IMAGE OCSERV_TRANSPORT_IMAGE
  export OCSERV_BACKUP_IMAGE OCSERV_POSTGRES_IMAGE OCSERV_OTEL_IMAGE
}

validate_prerequisites() {
  local compose_help
  command -v jq >/dev/null 2>&1 || fail "jq is required to validate release manifests"
  [[ -x "${COMPOSE_LAUNCHER}" && ! -L "${COMPOSE_LAUNCHER}" ]] ||
    fail "production Compose launcher is missing or not executable"
  require_absolute_canonical_path "production Compose launcher" "${COMPOSE_LAUNCHER}"
  command -v docker >/dev/null 2>&1 || fail "docker is required"
  compose_help="$(docker compose up --help 2>&1)" || fail "Docker Compose v2 is required"
  [[ "${compose_help}" == *"--wait"* ]] ||
    fail "Docker Compose must support 'docker compose up --wait'"
}

cleanup() {
  local status=$?
  if [[ -n "${STAGED_RELEASE}" && -e "${STAGED_RELEASE}" ]]; then
    rm -f -- "${STAGED_RELEASE}"
  fi
  if [[ -n "${CANONICAL_RELEASE}" && -e "${CANONICAL_RELEASE}" ]]; then
    rm -f -- "${CANONICAL_RELEASE}"
  fi
  exit "${status}"
}

install_controller() {
  local release_version
  prepare_state_root
  CURRENT_RELEASE="${STATE_ROOT}/current-release.json"
  PENDING_RELEASE="${STATE_ROOT}/pending-release.json"
  acquire_lock

  if [[ -L "${CURRENT_RELEASE}" ]]; then
    fail "current release state is a symlink; refusing to install"
  elif [[ -e "${CURRENT_RELEASE}" ]]; then
    fail "Controller is already installed; use upgrade"
  fi

  validate_release_file_path "${RELEASE_FILE}"
  stage_and_validate_manifest
  map_manifest_images
  release_version="$(jq -er -s '.[0].release_version' "${STAGED_RELEASE}")"
  validate_source_tree
  validate_prerequisites

  if [[ -L "${PENDING_RELEASE}" ]]; then
    fail "pending release state must not be a symlink"
  elif [[ -e "${PENDING_RELEASE}" ]]; then
    validate_state_file_path "pending release state" "${PENDING_RELEASE}"
    cmp -s "${PENDING_RELEASE}" "${STAGED_RELEASE}" ||
      fail "a different pending release exists; only the same manifest may be retried"
  else
    mv -- "${STAGED_RELEASE}" "${PENDING_RELEASE}" || fail "cannot persist pending release state"
    STAGED_RELEASE=""
  fi

  "${COMPOSE_LAUNCHER}" config --quiet
  "${COMPOSE_LAUNCHER}" pull
  "${COMPOSE_LAUNCHER}" up -d --wait

  [[ ! -e "${CURRENT_RELEASE}" && ! -L "${CURRENT_RELEASE}" ]] ||
    fail "current release state appeared during install"
  mv -- "${PENDING_RELEASE}" "${CURRENT_RELEASE}" || fail "cannot commit current release state"
  echo "Controller ${release_version} installed"
}

if (($# != 3)) || [[ "$1" != "install" || "$2" != "--release-file" ]]; then
  usage
fi

RELEASE_FILE="$3"
normalize_release_file_path
trap cleanup EXIT
install_controller
