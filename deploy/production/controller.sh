#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
STATE_ROOT="${OCSERV_CONTROLLER_STATE_ROOT:-${OCSERV_CONTROLLER_STATE_DIR:-/var/lib/ocservia-controller}}"
COMPOSE_LAUNCHER="${OCSERV_CONTROLLER_COMPOSE_SH:-${ROOT}/deploy/production/compose.sh}"
SMOKE_SCRIPT="${OCSERV_CONTROLLER_SMOKE_SH:-${ROOT}/deploy/production/controller-release-smoke.sh}"
CURRENT_RELEASE=""
PREVIOUS_RELEASE=""
PENDING_RELEASE=""
STAGED_RELEASE=""
CANONICAL_RELEASE=""
PENDING_MANIFEST_TMP=""
PENDING_ACTIVE=false
PENDING_FAILURE_RECORDED=false

umask 077

usage() {
  echo "usage: $0 {install|upgrade} --release-file /path/controller-release.json" >&2
  exit 2
}

fail() {
  local message="$1" status="${2:-2}"
  if [[ "${PENDING_ACTIVE}" == true && "${PENDING_FAILURE_RECORDED}" != true ]]; then
    record_pending_failure "${message}" ||
      echo "controller lifecycle: could not persist pending failure evidence" >&2
  fi
  echo "controller lifecycle: ${message}" >&2
  exit "${status}"
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

validate_manifest_file() {
  local label="$1" path="$2" manifest_filter
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
  jq -e -s "${manifest_filter}" "${path}" >/dev/null ||
    fail "${label} is invalid"

  CANONICAL_RELEASE="$(mktemp "${STATE_ROOT}/.canonical-release.json.XXXXXX")" ||
    fail "cannot allocate manifest validation file"
  chmod 600 "${CANONICAL_RELEASE}"
  jq -s '.[0]' "${path}" >"${CANONICAL_RELEASE}" ||
    fail "${label} is not valid JSON"
  cmp -s "${path}" "${CANONICAL_RELEASE}" ||
    fail "${label} is not in canonical form"
  rm -f -- "${CANONICAL_RELEASE}"
  CANONICAL_RELEASE=""
}

stage_and_validate_manifest() {
  STAGED_RELEASE="$(mktemp "${STATE_ROOT}/.current-release.json.XXXXXX")" ||
    fail "cannot allocate release state staging file"
  chmod 600 "${STAGED_RELEASE}"
  cat -- "${RELEASE_FILE}" >"${STAGED_RELEASE}" || fail "cannot read release file"
  validate_manifest_file "release manifest" "${STAGED_RELEASE}"
}

map_manifest_images() {
  local manifest="$1"
  OCSERV_GATEWAY_IMAGE="$(jq -er -s '.[0].images.gateway' "${manifest}")"
  OCSERV_CONTROL_IMAGE="$(jq -er -s '.[0].images.control' "${manifest}")"
  OCSERV_TRANSPORT_IMAGE="$(jq -er -s '.[0].images.transport' "${manifest}")"
  OCSERV_BACKUP_IMAGE="$(jq -er -s '.[0].images.backup' "${manifest}")"
  OCSERV_POSTGRES_IMAGE="$(jq -er -s '.[0].images.postgres' "${manifest}")"
  OCSERV_OTEL_IMAGE="$(jq -er -s '.[0].images.otel' "${manifest}")"
  export OCSERV_GATEWAY_IMAGE OCSERV_CONTROL_IMAGE OCSERV_TRANSPORT_IMAGE
  export OCSERV_BACKUP_IMAGE OCSERV_POSTGRES_IMAGE OCSERV_OTEL_IMAGE
}

validate_prerequisites() {
  local compose_help
  command -v jq >/dev/null 2>&1 || fail "jq is required to validate release manifests"
  [[ -x "${COMPOSE_LAUNCHER}" && ! -L "${COMPOSE_LAUNCHER}" ]] ||
    fail "production Compose launcher is missing or not executable"
  require_absolute_canonical_path "production Compose launcher" "${COMPOSE_LAUNCHER}"
  [[ -x "${SMOKE_SCRIPT}" && ! -L "${SMOKE_SCRIPT}" ]] ||
    fail "Controller release smoke script is missing or not executable"
  require_absolute_canonical_path "Controller release smoke script" "${SMOKE_SCRIPT}"
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
  if [[ -n "${PENDING_MANIFEST_TMP}" && -e "${PENDING_MANIFEST_TMP}" ]]; then
    rm -f -- "${PENDING_MANIFEST_TMP}"
  fi
  if ((status != 0)) && [[ "${PENDING_ACTIVE}" == true && "${PENDING_FAILURE_RECORDED}" != true ]]; then
    record_pending_failure "Controller lifecycle command failed" || :
  fi
  exit "${status}"
}

validate_current_release() {
  if [[ -L "${CURRENT_RELEASE}" ]]; then
    fail "current release state is a symlink; refusing to upgrade"
  fi
  [[ -e "${CURRENT_RELEASE}" ]] || fail "current release state is missing; refusing to upgrade"
  validate_state_file_path "current release state" "${CURRENT_RELEASE}"
  validate_manifest_file "current release state" "${CURRENT_RELEASE}"
}

validate_previous_release() {
  if [[ -L "${PREVIOUS_RELEASE}" ]]; then
    fail "previous release state must not be a symlink"
  elif [[ -e "${PREVIOUS_RELEASE}" ]]; then
    validate_state_file_path "previous release state" "${PREVIOUS_RELEASE}"
    validate_manifest_file "previous release state" "${PREVIOUS_RELEASE}"
  fi
}

reconcile_completed_pending() {
  if [[ ! -e "${CURRENT_RELEASE}" || -L "${CURRENT_RELEASE}" ||
    ! -e "${PENDING_RELEASE}" || -L "${PENDING_RELEASE}" ]]; then
    return
  fi
  validate_state_file_path "current release state" "${CURRENT_RELEASE}"
  validate_state_file_path "pending release state" "${PENDING_RELEASE}"
  if jq -e --slurpfile current "${CURRENT_RELEASE}" \
    'type == "object" and
      ((if has("manifest") then .manifest else . end) == $current[0])' \
    "${PENDING_RELEASE}" >/dev/null; then
    rm -- "${PENDING_RELEASE}" || fail "cannot reconcile completed pending release state"
  fi
}

write_pending_state() {
  local manifest="$1" phase="$2" pending_staged
  pending_staged="$(mktemp "${STATE_ROOT}/.pending-release.json.XXXXXX")" || return 1
  chmod 600 "${pending_staged}"
  if ! jq -s --arg phase "${phase}" \
    '{manifest: .[0], phase: $phase, failure: null}' "${manifest}" >"${pending_staged}"; then
    rm -f -- "${pending_staged}"
    return 1
  fi
  if ! mv -- "${pending_staged}" "${PENDING_RELEASE}"; then
    rm -f -- "${pending_staged}"
    return 1
  fi
  PENDING_FAILURE_RECORDED=false
}

record_pending_failure() {
  local message="$1" pending_staged
  [[ "${PENDING_ACTIVE}" == true && -f "${PENDING_RELEASE}" ]] || return 0
  pending_staged="$(mktemp "${STATE_ROOT}/.pending-release.json.XXXXXX")" || return 1
  chmod 600 "${pending_staged}"
  if ! jq --arg message "${message}" '
    (if has("manifest") then . else {manifest: ., phase: "failed", failure: null} end)
    | .phase = "failed"
    | .failure = {message: $message}
  ' "${PENDING_RELEASE}" >"${pending_staged}"; then
    rm -f -- "${pending_staged}"
    return 1
  fi
  if ! mv -- "${pending_staged}" "${PENDING_RELEASE}"; then
    rm -f -- "${pending_staged}"
    return 1
  fi
  PENDING_FAILURE_RECORDED=true
}

validate_pending_release() {
  PENDING_MANIFEST_TMP="$(mktemp "${STATE_ROOT}/.pending-manifest.json.XXXXXX")" ||
    fail "cannot allocate pending release validation file"
  chmod 600 "${PENDING_MANIFEST_TMP}"
  jq -e '
    type == "object" and
    (if has("manifest") then
      (.manifest | type == "object") and
      (.phase | type == "string") and
      ((.failure == null) or
        ((.failure | type == "object") and (.failure.message | type == "string")))
    else true end)
  ' "${PENDING_RELEASE}" >/dev/null || fail "pending release state is invalid"
  jq -e 'if has("manifest") then .manifest else . end' \
    "${PENDING_RELEASE}" >"${PENDING_MANIFEST_TMP}" || fail "pending release state is invalid"
  validate_manifest_file "pending release manifest" "${PENDING_MANIFEST_TMP}"
  rm -f -- "${PENDING_MANIFEST_TMP}"
  PENDING_MANIFEST_TMP=""
}

ensure_pending_release() {
  if [[ -L "${PENDING_RELEASE}" ]]; then
    fail "pending release state must not be a symlink"
  elif [[ -e "${PENDING_RELEASE}" ]]; then
    validate_state_file_path "pending release state" "${PENDING_RELEASE}"
    validate_pending_release
    if ! jq -e --slurpfile target "${STAGED_RELEASE}" \
      '((if has("manifest") then .manifest else . end) == $target[0])' \
      "${PENDING_RELEASE}" >/dev/null; then
      fail "a different pending release exists; only the same manifest may be retried"
    fi
    PENDING_ACTIVE=true
    write_pending_state "${STAGED_RELEASE}" "preflight" ||
      fail "cannot refresh pending release state"
  else
    write_pending_state "${STAGED_RELEASE}" "preflight" ||
      fail "cannot persist pending release state"
    PENDING_ACTIVE=true
  fi
}

mark_pending_phase() {
  local phase="$1"
  write_pending_state "${STAGED_RELEASE}" "${phase}" ||
    fail "cannot persist pending release phase ${phase}"
}

run_release_smoke() {
  if ! "${SMOKE_SCRIPT}" --release-file "${STAGED_RELEASE}"; then
    fail "release smoke failed; confirmed release state remains unchanged" 1
  fi
}

commit_install_state() {
  local current_staged
  current_staged="$(mktemp "${STATE_ROOT}/.current-release.json.XXXXXX")" ||
    fail "cannot allocate current release state staging file"
  chmod 600 "${current_staged}"
  if ! cat -- "${STAGED_RELEASE}" >"${current_staged}"; then
    rm -f -- "${current_staged}"
    fail "cannot stage current release state"
  fi
  if [[ -e "${CURRENT_RELEASE}" || -L "${CURRENT_RELEASE}" ]]; then
    rm -f -- "${current_staged}"
    fail "current release state appeared during install"
  fi
  mv -- "${current_staged}" "${CURRENT_RELEASE}" || {
    rm -f -- "${current_staged}"
    fail "cannot commit current release state"
  }
  rm -- "${PENDING_RELEASE}" || fail "cannot remove pending release state"
  PENDING_ACTIVE=false
}

compare_decimal_strings() {
  local left="$1" right="$2"
  while [[ "${#left}" -gt 1 && "${left}" == 0* ]]; do left="${left#0}"; done
  while [[ "${#right}" -gt 1 && "${right}" == 0* ]]; do right="${right#0}"; done
  if ((${#left} < ${#right})); then
    echo -1
  elif ((${#left} > ${#right})); then
    echo 1
  elif [[ "${left}" == "${right}" ]]; then
    echo 0
  elif [[ "${left}" < "${right}" ]]; then
    echo -1
  else
    echo 1
  fi
}

compare_semver() {
  local left="$1" right="$2" left_major left_minor left_patch right_major right_minor right_patch part
  IFS=. read -r left_major left_minor left_patch <<<"${left}"
  IFS=. read -r right_major right_minor right_patch <<<"${right}"
  for part in "${left_major}" "${left_minor}" "${left_patch}" "${right_major}" "${right_minor}" "${right_patch}"; do
    [[ "${part}" =~ ^[0-9]+$ ]] || return 2
  done
  local comparison
  comparison="$(compare_decimal_strings "${left_major}" "${right_major}")"
  if [[ "${comparison}" != 0 ]]; then echo "${comparison}"; return; fi
  comparison="$(compare_decimal_strings "${left_minor}" "${right_minor}")"
  if [[ "${comparison}" != 0 ]]; then echo "${comparison}"; return; fi
  compare_decimal_strings "${left_patch}" "${right_patch}"
}

check_current_database_and_backup_health() {
  local health_json
  if ! health_json="$("${COMPOSE_LAUNCHER}" ps --format json postgres backup)"; then
    fail "cannot inspect current PostgreSQL and backup health; current release remains unchanged"
  fi
  if ! jq -s -e '
    if type != "array" then false
    else
      . as $services |
      ["postgres", "backup"] |
      all(.[];
        . as $service |
        any($services[]; .Service == $service and .State == "running" and .Health == "healthy"))
    end
  ' <<<"${health_json}" >/dev/null; then
    fail "current PostgreSQL and backup services are not healthy; current release remains unchanged"
  fi
}

commit_upgrade_state() {
  local previous_staged
  previous_staged="$(mktemp "${STATE_ROOT}/.previous-release.json.XXXXXX")" ||
    fail "cannot allocate previous release state staging file"
  chmod 600 "${previous_staged}"
  cat -- "${CURRENT_RELEASE}" >"${previous_staged}" || {
    rm -f -- "${previous_staged}"
    fail "cannot stage previous release state"
  }

  if [[ -L "${CURRENT_RELEASE}" || -L "${PREVIOUS_RELEASE}" ]]; then
    rm -f -- "${previous_staged}"
    fail "release state path became a symlink; target release was not confirmed"
  fi
  if ! mv -- "${STAGED_RELEASE}" "${CURRENT_RELEASE}"; then
    rm -f -- "${previous_staged}"
    fail "cannot commit current release state; target release was not confirmed"
  fi
  STAGED_RELEASE=""
  if ! mv -- "${previous_staged}" "${PREVIOUS_RELEASE}"; then
    if ! mv -- "${previous_staged}" "${CURRENT_RELEASE}"; then
      fail "release state rollover failed after activation; target release was not confirmed"
    fi
    fail "release state rollover failed after activation; target release was not confirmed"
  fi
  rm -- "${PENDING_RELEASE}" || fail "cannot remove pending release state after upgrade"
  PENDING_ACTIVE=false
}

install_controller() {
  local release_version
  prepare_state_root
  CURRENT_RELEASE="${STATE_ROOT}/current-release.json"
  PENDING_RELEASE="${STATE_ROOT}/pending-release.json"
  acquire_lock
  reconcile_completed_pending

  if [[ -L "${CURRENT_RELEASE}" ]]; then
    fail "current release state is a symlink; refusing to install"
  elif [[ -e "${CURRENT_RELEASE}" ]]; then
    fail "Controller is already installed; use upgrade"
  fi

  validate_release_file_path "${RELEASE_FILE}"
  stage_and_validate_manifest
  map_manifest_images "${STAGED_RELEASE}"
  release_version="$(jq -er -s '.[0].release_version' "${STAGED_RELEASE}")"
  validate_source_tree
  validate_prerequisites

  ensure_pending_release

  if ! "${COMPOSE_LAUNCHER}" config --quiet; then
    fail "production preflight failed; pending release state retained"
  fi
  if ! "${COMPOSE_LAUNCHER}" pull; then
    fail "target image pull failed; pending release state retained"
  fi
  mark_pending_phase activation
  if ! "${COMPOSE_LAUNCHER}" up -d --wait; then
    fail "activation started but was not confirmed successful; pending release state retained" 1
  fi

  mark_pending_phase smoke
  run_release_smoke
  commit_install_state
  echo "Controller ${release_version} installed"
}

upgrade_controller() {
  local current_version target_version comparison
  prepare_state_root
  CURRENT_RELEASE="${STATE_ROOT}/current-release.json"
  PREVIOUS_RELEASE="${STATE_ROOT}/previous-release.json"
  PENDING_RELEASE="${STATE_ROOT}/pending-release.json"
  acquire_lock
  reconcile_completed_pending

  validate_current_release
  validate_previous_release
  validate_release_file_path "${RELEASE_FILE}"
  stage_and_validate_manifest
  validate_source_tree
  current_version="$(jq -er -s '.[0].release_version' "${CURRENT_RELEASE}")"
  target_version="$(jq -er -s '.[0].release_version' "${STAGED_RELEASE}")"
  comparison="$(compare_semver "${target_version}" "${current_version}")" ||
    fail "release versions are not valid SemVer"

  case "${comparison}" in
    -1) fail "upgrade does not perform downgrade" ;;
    0)
      if cmp -s "${CURRENT_RELEASE}" "${STAGED_RELEASE}"; then
        echo "Controller ${target_version} is already current; no-op"
        return
      fi
      fail "target release version matches current but the manifest differs"
      ;;
    1) ;;
    *) fail "release versions are not valid SemVer" ;;
  esac

  validate_prerequisites
  map_manifest_images "${CURRENT_RELEASE}"
  check_current_database_and_backup_health

  map_manifest_images "${STAGED_RELEASE}"
  ensure_pending_release
  if ! "${COMPOSE_LAUNCHER}" config --quiet; then
    fail "production preflight failed for target release; current release remains unchanged"
  fi
  if ! "${COMPOSE_LAUNCHER}" pull; then
    fail "target image pull failed; current release remains unchanged"
  fi
  mark_pending_phase activation
  if ! "${COMPOSE_LAUNCHER}" up -d --wait; then
    fail "upgrade activation started but was not confirmed successful; current release state remains unchanged; do not automatically rollback old images" 1
  fi

  mark_pending_phase smoke
  run_release_smoke
  commit_upgrade_state
  echo "Controller ${target_version} upgraded"
}

if (($# != 3)) || [[ "$2" != "--release-file" ]] ||
  [[ "$1" != "install" && "$1" != "upgrade" ]]; then
  usage
fi

RELEASE_FILE="$3"
normalize_release_file_path
trap cleanup EXIT
if [[ "$1" == "install" ]]; then
  install_controller
else
  upgrade_controller
fi
