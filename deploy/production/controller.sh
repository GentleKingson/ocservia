#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
STATE_ROOT="${OCSERV_CONTROLLER_STATE_ROOT:-${OCSERV_CONTROLLER_STATE_DIR:-/var/lib/ocservia-controller}}"
COMPOSE_LAUNCHER="${OCSERV_CONTROLLER_COMPOSE_SH:-${ROOT}/deploy/production/compose.sh}"
SMOKE_SCRIPT="${OCSERV_CONTROLLER_SMOKE_SH:-${ROOT}/deploy/production/controller-release-smoke.sh}"
CURRENT_RELEASE=""
PREVIOUS_RELEASE=""
STAGED_RELEASE=""
SOURCE_CHECKOUT_TMP=""
PURGE_DATA=false

umask 077

usage() {
  echo "usage: $0 {install|upgrade} --release-file /path/controller-release-<amd64|arm64>.json" >&2
  echo "       $0 rollback" >&2
  echo "       $0 {start|health}" >&2
  echo "       $0 uninstall [--purge-data]" >&2
  exit 2
}

fail() {
  local message="$1" status="${2:-2}"
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
    IFS=: read -r ancestor_uid ancestor_mode < <(stat -c '%u:%a' "${ancestor}")
    [[ "${ancestor_uid}" == "0" || "${ancestor_uid}" == "${EUID}" ]] ||
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
    return
  fi

  require_absolute_canonical_path "state root" "${STATE_ROOT}"
  validate_ancestor "$(dirname -- "${STATE_ROOT}")"
  IFS=: read -r state_uid state_mode < <(stat -c '%u:%a' "${STATE_ROOT}")
  [[ "${state_uid}" == "${EUID}" && "${state_mode}" == "700" ]] ||
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
  [[ "${lock_uid}" == "${EUID}" && "${lock_mode}" == "600" ]] ||
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
  validate_ancestor "$(dirname -- "${path}")"
  [[ -f "${path}" && ! -L "${path}" ]] || fail "release file must be a regular file and not a symlink"
  IFS=: read -r release_uid release_mode < <(stat -c '%u:%a' "${path}")
  [[ "${release_uid}" == "0" || "${release_uid}" == "${EUID}" ]] ||
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
  [[ "${state_uid}" == "${EUID}" && "${state_mode}" == "600" ]] ||
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

select_release_source() {
  local manifest="$1" manifest_commit current_commit dirty source_root
  manifest_commit="$(jq -er -s '.[0].source_commit' "${manifest}")"
  if [[ -f "${ROOT}/RELEASE" ]]; then
    local manifest_tag
    manifest_tag="$(jq -er -s '.[0].release_tag' "${manifest}")"
    # shellcheck source=deploy/lib/controller-runtime.sh
    source "${ROOT}/deploy/lib/controller-runtime.sh"
    source_root="${STATE_ROOT}/runtime-${manifest_tag}"
    if [[ ! -e "${source_root}" && ! -L "${source_root}" ]] &&
       [[ "$(cat "${ROOT}/RELEASE")" == "${manifest_tag} ${manifest_commit}" ]]; then
      SOURCE_CHECKOUT_TMP="$(mktemp -d "${STATE_ROOT}/.runtime.XXXXXX")"
      # A root lifecycle may start from a launcher-owned downloaded bundle.
      # Retained state belongs to the lifecycle user, never the source owner.
      cp -R --preserve=mode,timestamps "${ROOT}/." "${SOURCE_CHECKOUT_TMP}/"
      runtime_validate "${SOURCE_CHECKOUT_TMP}" "${manifest_tag}" "${manifest_commit}"
      mv -T -- "${SOURCE_CHECKOUT_TMP}" "${source_root}"
      SOURCE_CHECKOUT_TMP=""
    else
      runtime_fetch "${source_root}" "${manifest_tag}" "${manifest_commit}"
    fi
    require_absolute_canonical_path "release runtime" "${source_root}"
    validate_ancestor "${source_root}"
    COMPOSE_LAUNCHER="${OCSERV_CONTROLLER_COMPOSE_SH:-${source_root}/deploy/production/compose.sh}"
    SMOKE_SCRIPT="${OCSERV_CONTROLLER_SMOKE_SH:-${source_root}/deploy/production/controller-release-smoke.sh}"
    return
  fi
  # Legacy checkout invocation remains available; new releases use bundles.
  if ! current_commit="$(git -C "${ROOT}" rev-parse HEAD 2>/dev/null)"; then
    fail "Controller release must be installed from a Git checkout"
  fi
  source_root="${ROOT}"
  if [[ "${current_commit}" != "${manifest_commit}" ]]; then
    source_root="${STATE_ROOT}/source-${manifest_commit}"
    if [[ ! -e "${source_root}" && ! -L "${source_root}" ]]; then
      git -C "${ROOT}" cat-file -e "${manifest_commit}^{commit}" 2>/dev/null ||
        fail "release source_commit cannot be resolved locally"
      SOURCE_CHECKOUT_TMP="$(mktemp -d "${STATE_ROOT}/.source.XXXXXX")" ||
        fail "cannot allocate release source checkout"
      # Keep target bind mounts available after this command; never rewrite
      # an existing checkout or substitute the caller's deployment files.
      if ! (umask 022; git clone --quiet --no-hardlinks --no-checkout "${ROOT}" "${SOURCE_CHECKOUT_TMP}" &&
        git -C "${SOURCE_CHECKOUT_TMP}" checkout --quiet --detach "${manifest_commit}"); then
        fail "cannot prepare release source checkout"
      fi
      mv -- "${SOURCE_CHECKOUT_TMP}" "${source_root}" || fail "cannot retain release source checkout"
      SOURCE_CHECKOUT_TMP=""
    fi
    require_absolute_canonical_path "release source checkout" "${source_root}"
    validate_ancestor "${source_root}"
  fi
  [[ "$(git -C "${source_root}" rev-parse HEAD)" == "${manifest_commit}" ]] ||
    fail "checkout HEAD does not match release manifest source_commit"
  dirty="$(git -C "${source_root}" status --porcelain --untracked-files=all)"
  [[ -z "${dirty}" ]] || fail "checkout has untracked changes"
  COMPOSE_LAUNCHER="${OCSERV_CONTROLLER_COMPOSE_SH:-${source_root}/deploy/production/compose.sh}"
  SMOKE_SCRIPT="${OCSERV_CONTROLLER_SMOKE_SH:-${source_root}/deploy/production/controller-release-smoke.sh}"
}

validate_manifest_file() {
  local label="$1" path="$2" manifest_filter
  [[ -n "${path}" && -f "${path}" && ! -L "${path}" ]] || fail "${label} file missing: ${path:-<unset>}"
  # jq variables in this filter are intentionally not shell variables.
  # shellcheck disable=SC2016
  manifest_filter='
    def matches($pattern): if type == "string" then test($pattern) else false end;
    def positive_integer: if type == "number" then (floor == . and . >= 1 and . <= 9007199254740991) else false end;
    def migration_identity:
      if type == "object" then
        keys == ["epoch", "revision"] and
        (.epoch | positive_integer) and
        (.revision | type == "number" and floor == . and . >= 0 and . <= 9007199254740991)
      else positive_integer end;
    if length != 1 then false
    else
      .[0] as $manifest |
      ($manifest | type == "object") and
      (if $manifest.manifest_version == 1 then
        ($manifest | keys == ["database_migration", "images", "manifest_version", "platform", "release_tag", "release_version", "source_commit"]) and
        ($manifest.images | keys == ["backup", "control", "gateway", "otel", "postgres", "transport"])
      elif $manifest.manifest_version == 2 then
        ($manifest | keys == ["database_migration", "images", "manifest_version", "platform", "release_tag", "release_version", "signer_state_version", "source_commit"]) and
        ($manifest.signer_state_version == 1) and
        ($manifest.images | keys == ["backup", "control", "edge", "gateway", "otel", "postgres", "relay", "signer", "transport"])
      else false end) and
      ($manifest.release_version | matches("^[0-9]+\\.[0-9]+\\.[0-9]+(-rc[.][1-9][0-9]*)?$")) and
      ($manifest.release_tag | matches("^v[0-9]+\\.[0-9]+\\.[0-9]+(-rc[.][1-9][0-9]*)?$") and . == ("v" + $manifest.release_version)) and
      ($manifest.source_commit | matches("^[0-9a-f]{40}$")) and
      ($manifest.platform | IN("linux/amd64", "linux/arm64")) and
      ($manifest.database_migration | migration_identity) and
      ($manifest.images | type == "object" and
        all(.[]; matches("^[^[:space:]@]+@sha256:[0-9a-f]{64}$")))
    end
  '
  jq -e -s "${manifest_filter}" "${path}" >/dev/null ||
    fail "${label} is invalid"


}

check_active_profile_supported() {
  local profile="${STATE_ROOT}/deployment-profile.json"
  [[ -e "${profile}" || -L "${profile}" ]] || return 0
  command -v jq >/dev/null 2>&1 || fail "jq is required to validate deployment profile"
  validate_state_file_path "deployment profile" "${profile}"
  # tls_mode is recorded only for ACME, so releases without ACME refuse it.
  jq -e '(keys == ["database_backend", "database_deployment", "deployment_mode"] or
      (keys == ["database_backend", "database_deployment", "deployment_mode", "tls_mode"] and
        .deployment_mode == "integrated" and .tls_mode == "acme")) and
    (.deployment_mode | IN("standalone", "integrated")) and
    ([.database_backend, .database_deployment] | IN(["postgres", "bundled"], ["postgres", "external"]))' \
    "${profile}" >/dev/null || fail "invalid deployment profile"
}

map_manifest_images() {
  local manifest="$1"
  local mode="${OCSERV_DEPLOYMENT_MODE:-standalone}" profile="${STATE_ROOT}/deployment-profile.json"
  local backend="${OCSERV_DATABASE_BACKEND:-postgres}" deployment="${OCSERV_DATABASE_DEPLOYMENT:-bundled}"
  local tls_mode="${OCSERV_TLS_MODE:-manual}"
  if [[ -e "${profile}" || -L "${profile}" ]]; then
    check_active_profile_supported
    mode="$(jq -r '.deployment_mode' "${profile}")"
    backend="$(jq -r '.database_backend' "${profile}")"
    deployment="$(jq -r '.database_deployment' "${profile}")"
    tls_mode="$(jq -r '.tls_mode // "manual"' "${profile}")"
    [[ "${OCSERV_DEPLOYMENT_MODE:-${mode}}" == "${mode}" ]] ||
      fail "deployment mode cannot change during an existing installation"
    [[ "${OCSERV_DATABASE_BACKEND:-${backend}}" == "${backend}" && "${OCSERV_DATABASE_DEPLOYMENT:-${deployment}}" == "${deployment}" ]] ||
      fail "database deployment cannot change during an existing installation"
    [[ "${OCSERV_TLS_MODE:-${tls_mode}}" == "${tls_mode}" ]] ||
      fail "TLS mode cannot change during an existing installation"
  elif [[ -e "${CURRENT_RELEASE}" && "${mode}" != standalone ]]; then
    fail "existing standalone installation cannot be switched to Integrated"
  fi
  case "${mode}" in standalone|integrated) ;; *) fail "invalid deployment mode" ;; esac
  case "${backend}:${deployment}" in
    postgres:bundled|postgres:external) ;;
    *) fail "invalid database deployment; expected postgres with bundled or external" ;;
  esac
  export OCSERV_DEPLOYMENT_MODE="${mode}"
  export OCSERV_DATABASE_BACKEND="${backend}" OCSERV_DATABASE_DEPLOYMENT="${deployment}"
  export OCSERV_TLS_MODE="${tls_mode}"
  if [[ "${mode}" == integrated && "$(jq -r '.manifest_version' "${manifest}")" != 2 ]]; then
    fail "Integrated deployment requires a v2 release manifest"
  fi
  local role image
  while IFS=$'\t' read -r role image; do
    export "OCSERV_${role^^}_IMAGE=${image}"
  done < <(jq -r '.images | to_entries[] | [.key, .value] | @tsv' "${manifest}")
}

check_manifest_platform() {
  local manifest="$1" manifest_platform host_platform server_arch
  manifest_platform="$(jq -er -s '.[0].platform' "${manifest}")"
  # Ask the Docker daemon, not the host kernel: the daemon resolves and
  # activates the release images, so its architecture decides which variant
  # Compose pulls.
  if ! server_arch="$(docker version --format '{{.Server.Arch}}')"; then
    fail "cannot determine the Docker server architecture"
  fi
  case "${server_arch}" in
    amd64) host_platform="linux/amd64" ;;
    arm64|aarch64) host_platform="linux/arm64" ;;
    *) fail "Docker server architecture is not a supported Controller platform: ${server_arch}" ;;
  esac
  [[ "${manifest_platform}" == "${host_platform}" ]] ||
    fail "release manifest platform ${manifest_platform} does not match the Docker host platform ${host_platform}"
}

record_deployment_profile() {
  [[ ! -e "${STATE_ROOT}/deployment-profile.json" ]] || return 0
  local staged
  staged="$(mktemp "${STATE_ROOT}/.deployment-profile.XXXXXX")"
  jq -n --arg mode "${OCSERV_DEPLOYMENT_MODE}" --arg backend "${OCSERV_DATABASE_BACKEND}" \
    --arg deployment "${OCSERV_DATABASE_DEPLOYMENT}" \
    --arg tls "${OCSERV_TLS_MODE}" '{deployment_mode:$mode, database_backend:$backend,
      database_deployment:$deployment} + (if $tls == "acme" then {tls_mode:$tls} else {} end)' >"${staged}"
  mv -- "${staged}" "${STATE_ROOT}/deployment-profile.json"
}

checkpoint_signer() {
  [[ "${OCSERV_DEPLOYMENT_MODE:-standalone}" == integrated ]] || return 0
  local checkpoint="${STATE_ROOT}/signer-checkpoint.json" staged
  "${COMPOSE_LAUNCHER}" stop control-plane transportd signer ||
    fail "cannot stop writers for exclusive Signer state validation"
  staged="$(mktemp "${STATE_ROOT}/.signer-checkpoint.XXXXXX")"
  if ! "${COMPOSE_LAUNCHER}" run --rm --no-deps signer inspect >"${staged}" ||
    ! jq -e '.state_version == 1 and (.issuer_sha256 | test("^[0-9a-f]{64}$")) and
      (.policy | type == "string") and (.revision | type == "number" and . >= 0 and floor == .)' "${staged}" >/dev/null; then
    rm -f -- "${staged}"
    fail "Signer state inspection failed; preserve stopped state for recovery"
  fi
  if [[ -e "${checkpoint}" || -L "${checkpoint}" ]]; then
    validate_state_file_path "Signer checkpoint" "${checkpoint}"
    if ! jq -e --slurpfile previous "${checkpoint}" '
      .state_version == $previous[0].state_version and .issuer_sha256 == $previous[0].issuer_sha256 and
      .policy == $previous[0].policy and .revision >= $previous[0].revision' "${staged}" >/dev/null; then
      rm -f -- "${staged}"
      fail "Signer identity or minimum revision changed; reconcile without resetting state"
    fi
  fi
  mv -f -- "${staged}" "${checkpoint}"
}

validate_prerequisites() {
  local compose_help require_release_runtime="$1"
  command -v jq >/dev/null 2>&1 || fail "jq is required to validate release manifests"
  [[ -x "${COMPOSE_LAUNCHER}" && ! -L "${COMPOSE_LAUNCHER}" ]] ||
    fail "production Compose launcher is missing or not executable"
  require_absolute_canonical_path "production Compose launcher" "${COMPOSE_LAUNCHER}"
  command -v docker >/dev/null 2>&1 || fail "docker is required"
  if [[ "${require_release_runtime}" == true ]]; then
    [[ -x "${SMOKE_SCRIPT}" && ! -L "${SMOKE_SCRIPT}" ]] ||
      fail "Controller release smoke script is missing or not executable"
    require_absolute_canonical_path "Controller release smoke script" "${SMOKE_SCRIPT}"
    compose_help="$(docker compose up --help 2>&1)" || fail "Docker Compose v2 is required"
    [[ "${compose_help}" == *"--wait"* ]] ||
      fail "Docker Compose must support 'docker compose up --wait'"
  fi
}

command="${1:-}"
case "${command}" in
  install|upgrade)
    [[ $# == 3 && "$2" == --release-file ]] || usage
    RELEASE_FILE="$3"
    normalize_release_file_path
    validate_release_file_path "${RELEASE_FILE}"
    ;;
  start|rollback|health) (($# == 1)) || usage ;;
  uninstall)
    if (($# == 2)) && [[ "$2" == --purge-data ]]; then
      PURGE_DATA=true
    else
      (($# == 1)) || usage
    fi ;;
  *) usage ;;
esac

prepare_state_root
acquire_lock
work="$(mktemp -d "${STATE_ROOT}/.lifecycle.XXXXXX")"
trap 'rm -rf -- "${work}" ${SOURCE_CHECKOUT_TMP:+"${SOURCE_CHECKOUT_TMP}"}' EXIT
history="${STATE_ROOT}/release-history.json"
CURRENT_RELEASE="${work}/current.json"
PREVIOUS_RELEASE="${work}/previous.json"
STAGED_RELEASE="${work}/target.json"

# Import the last confirmed v1 state. Finish an in-flight legacy transaction
# using its original launcher before switching to the new history format.
if [[ -e "${history}" || -L "${history}" ]]; then
  validate_state_file_path "release history" "${history}"
  jq -e '.current' "${history}" >"${CURRENT_RELEASE}"
  if jq -e '.previous != null' "${history}" >/dev/null; then
    jq -e '.previous' "${history}" >"${PREVIOUS_RELEASE}"
  fi
else
  [[ ! -e "${STATE_ROOT}/pending-release.json" && ! -L "${STATE_ROOT}/pending-release.json" ]] ||
    fail "finish the pending release with its original launcher before upgrading"
  for slot in current previous; do
    path="${STATE_ROOT}/${slot}-release.json"
    if [[ -e "${path}" || -L "${path}" ]]; then
      validate_state_file_path "${slot} release" "${path}"
      cp -- "${path}" "${work}/${slot}.json"
    fi
  done
fi

if [[ "${command}" == install ]]; then
  [[ ! -e "${CURRENT_RELEASE}" ]] || fail "Controller is already installed; use upgrade"
else
  validate_manifest_file "current release" "${CURRENT_RELEASE}"
fi

case "${command}" in
  install|upgrade) jq -e . "${RELEASE_FILE}" >"${STAGED_RELEASE}" ;;
  rollback)
    [[ -e "${PREVIOUS_RELEASE}" ]] || fail "no previous release to roll back to"
    cp -- "${PREVIOUS_RELEASE}" "${STAGED_RELEASE}" ;;
  *) cp -- "${CURRENT_RELEASE}" "${STAGED_RELEASE}" ;;
esac
validate_manifest_file "target release" "${STAGED_RELEASE}"
select_release_source "${STAGED_RELEASE}"
if [[ "${command}" == uninstall ]]; then validate_prerequisites false; else validate_prerequisites true; fi
map_manifest_images "${STAGED_RELEASE}"
check_manifest_platform "${STAGED_RELEASE}"

# Current runtimes expose a shell function so the protected host inputs are
# validated once for pull/inspect/up. Older release launchers remain executable.
if [[ -z "${OCSERV_CONTROLLER_COMPOSE_SH:-}" && ! -e "$(dirname -- "${COMPOSE_LAUNCHER}")/compose.postgres.yaml" ]]; then
  # shellcheck disable=SC1090
  source "${COMPOSE_LAUNCHER}"
  COMPOSE_LAUNCHER=production_compose
fi

case "${command}" in
  health) "${SMOKE_SCRIPT}" --release-file "${STAGED_RELEASE}"; exit ;;
  uninstall)
    down=(down)
    if [[ "${PURGE_DATA}" == true ]]; then
      [[ "${OCSERV_DEPLOYMENT_MODE}" != integrated ]] ||
        fail "Integrated uninstall preserves Signer state"
      down+=(--volumes)
    fi
    "${COMPOSE_LAUNCHER}" "${down[@]}"
    if [[ "${PURGE_DATA}" == true ]]; then
      rm -f -- "${history}" "${STATE_ROOT}/"{current-release,previous-release,deployment-profile}.json
    fi
    exit ;;
esac

"${COMPOSE_LAUNCHER}" pull
record_deployment_profile
if [[ "${command}" == install && "${OCSERV_DEPLOYMENT_MODE}" == integrated ]]; then
  "${COMPOSE_LAUNCHER}" stop signer
  if [[ ! -e "${STATE_ROOT}/signer-init-attempted" ]]; then
    [[ ! -e "${OCSERV_SIGNER_STATE_DIR}/ledger.db" && ! -L "${OCSERV_SIGNER_STATE_DIR}/ledger.db" ]] ||
      fail "first install refuses preexisting Signer state"
    printf '%s\n' attempted >"${STATE_ROOT}/signer-init-attempted"
  fi
  # A failed first init can be retried until inspect has confirmed a ledger.
  if [[ ! -e "${STATE_ROOT}/signer-checkpoint.json" && ! -e "${OCSERV_SIGNER_STATE_DIR}/ledger.db" ]]; then
    "${COMPOSE_LAUNCHER}" run --rm --no-deps signer init
  fi
fi
checkpoint_signer
"${COMPOSE_LAUNCHER}" up -d --wait
"${SMOKE_SCRIPT}" --release-file "${STAGED_RELEASE}"

# A single rename records both generations after public readiness succeeds.
# Retrying the same release reapplies Compose and keeps the rollback target.
if [[ ! -e "${CURRENT_RELEASE}" ]]; then
  jq '{current: ., previous: null}' "${STAGED_RELEASE}" >"${work}/history"
elif cmp -s "${CURRENT_RELEASE}" "${STAGED_RELEASE}"; then
  if [[ -e "${history}" ]]; then exit 0; fi
  previous="${PREVIOUS_RELEASE}"
  [[ -e "${previous}" ]] || previous=/dev/null
  jq --slurpfile previous "${previous}" '{current: ., previous: $previous[0]}' "${STAGED_RELEASE}" >"${work}/history"
else
  jq --slurpfile old "${CURRENT_RELEASE}" '{current: ., previous: $old[0]}' "${STAGED_RELEASE}" >"${work}/history"
fi
mv -fT -- "${work}/history" "${history}"
