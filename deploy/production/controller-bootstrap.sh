#!/usr/bin/env bash
# Stage-1 versioned Controller bootstrap: prepare a durable, clean checkout
# of an exact vX.Y.Z release tag and hand off to the existing production
# installer.
#
# Scope: the operator runs this script from any directory holding the
# production configuration (./install.env and/or exported OCSERV_*
# variables). The script resolves that configuration once under the
# operator's own privileges and exports the effective allowlisted values,
# clones the requested exact release tag into a launcher-owned durable
# source root (the later lifecycle commands — upgrade, rollback, start,
# uninstall — keep operating on that checkout), verifies the checkout
# satisfies the production installer's clean exact-tag contract, selects
# the Docker lifecycle per the documented rules, and execs
# <checkout>/deploy/production/install.sh without changing the working
# directory. Because the effective configuration crosses the handoff as
# exported environment variables, releases whose installer predates the
# install.env loader (environment-only installers, e.g. v0.4.0) receive
# it exactly like install.env-aware installers, for which the explicit
# shell environment keeps its priority over the file.
#
# Boundaries (the existing authorities are unchanged):
# - No production secrets or trust material is created, downloaded, or
#   defaulted here: the release-signing public key still comes only from
#   OCSERV_CONTROLLER_RELEASE_PUBLIC_KEY through an independent protected
#   channel, and release assets are downloaded and verified by
#   install.sh / controller.sh, never by this script.
# - No manifest verification, no docker compose, no Docker installation,
#   and no modification of the Docker permission model (the host
#   bootstrap keeps that authority). This script itself never crosses a
#   privilege boundary and never calls sudo -E.
# - An existing target checkout is only reused when it is clean, exactly
#   at the requested release tag, and cloned from the expected
#   repository; anything else fails closed without rm -rf or overwrites
#   of pre-existing content.
# - The root lifecycle keeps relying on sudo's SUDO_UID semantics for
#   git's ownership trust of the launcher-owned checkout (install.sh
#   preserves SUDO_UID/SUDO_GID across its controlled re-exec); git's
#   safe.directory is never relaxed.
#
# Usage model:
#   cd <directory holding install.env>
#   deploy/production/controller-bootstrap.sh --version vX.Y.Z
#   deploy/production/controller-bootstrap.sh --version vX.Y.Z --root-lifecycle
#   deploy/production/controller-bootstrap.sh --version vX.Y.Z --check
#
# The version must be an explicit exact vX.Y.Z release tag; latest,
# branches, commits, and pre-releases are not accepted.
set -euo pipefail
umask 077

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
REPOSITORY_URL="https://github.com/GentleKingson/ocservia"
CONFIG_ROOT="${PWD}"
VERSION=""
ROOT_LIFECYCLE=false
CHECK_ONLY=false
SOURCE_ROOT=""
TARGET=""

# The install.env allowlist resolved by this bootstrap mirrors the
# production installer's own allowlist (deploy/production/install.sh).
# install.sh re-parses $PWD/install.env itself during the real run and
# remains the authority; keep both lists in sync.
INSTALL_ENV_NAMES=(
  OCSERV_AUDIT_EVENT_KEY_ID
  OCSERV_BACKUP_DIR
  OCSERV_BACKUP_INTERVAL_SECONDS
  OCSERV_BACKUP_RETENTION_COUNT
  OCSERV_CERTIFICATE_SIGNER_URL
  OCSERV_CONTROLLER_ENDPOINT_ID
  OCSERV_CONTROLLER_PUBLIC_URL
  OCSERV_CONTROLLER_RELEASE_PUBLIC_KEY
  OCSERV_CONTROLLER_STATE_DIR
  OCSERV_CONTROLLER_STATE_ROOT
  OCSERV_HTTPS_ADDRESS
  OCSERV_OIDC_CLIENT_ID
  OCSERV_OIDC_ISSUER
  OCSERV_OTEL_BACKEND_ENDPOINT
  OCSERV_PUBLIC_HOST
  OCSERV_RELAY_URL_A
  OCSERV_RELAY_URL_B
  OCSERV_SECRET_DIR
)

fail() {
  echo "controller bootstrap: $1" >&2
  exit 1
}

usage() {
  echo "usage: controller-bootstrap.sh --version vX.Y.Z [--root-lifecycle] [--check]" >&2
  exit 2
}

version_seen=false
while (($# > 0)); do
  case "$1" in
    --version)
      [[ "${version_seen}" == false && $# -ge 2 ]] || usage
      VERSION="$2"
      version_seen=true
      shift 2
      ;;
    --root-lifecycle)
      ROOT_LIFECYCLE=true
      shift
      ;;
    --check)
      CHECK_ONLY=true
      shift
      ;;
    *)
      usage
      ;;
  esac
done
[[ "${version_seen}" == true ]] || usage
[[ "${VERSION}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] ||
  fail "unsupported version '${VERSION}': an exact vX.Y.Z release tag is required (latest, branches, commits, and pre-releases are not accepted)"

resolve_source_root() {
  if [[ -n "${OCSERV_CONTROLLER_SOURCE_ROOT:-}" ]]; then
    SOURCE_ROOT="${OCSERV_CONTROLLER_SOURCE_ROOT}"
  else
    [[ -n "${HOME:-}" ]] ||
      fail "HOME is not set; set OCSERV_CONTROLLER_SOURCE_ROOT to an explicit durable source root"
    SOURCE_ROOT="${HOME}/.local/share/ocservia/controller/releases"
  fi
}

validate_source_component() {
  local component="$1" uid mode
  [[ ! -L "${component}" ]] ||
    fail "source root ancestry must not contain symlinks: ${component}"
  [[ -d "${component}" ]] ||
    fail "source root ancestry component is not a directory: ${component}"
  [[ "$(realpath -e -- "${component}")" == "${component}" ]] ||
    fail "source root ancestry must not contain symlinked paths: ${component}"
  IFS=: read -r uid mode < <(stat -c '%u:%a' "${component}")
  [[ "${uid}" == "0" || "${uid}" == "$(id -u)" ]] ||
    fail "source root ancestry must be root- or launcher-owned: ${component}"
  (( (8#${mode} & 8#022) == 0 )) ||
    fail "source root ancestry must not be group/world-writable: ${component} (mode ${mode})"
}

# Resolve ./install.env from the invoking directory once, under the
# operator's own privileges, and export the effective allowlisted values.
# --check uses this to validate the configuration before reporting
# success; the run path uses it so the handoff environment already carries
# the effective configuration, which releases whose installer predates the
# install.env loader require (they never read the file themselves).
# Explicit shell variables keep winning over the file, so install.env-aware
# installers observe exactly the same effective values as before.
load_config() {
  [[ -f "${ROOT}/deploy/lib/install-env.sh" ]] ||
    fail "the install.env loader is missing from this checkout: ${ROOT}/deploy/lib/install-env.sh"
  # shellcheck source=../lib/install-env.sh disable=SC1091
  source "${ROOT}/deploy/lib/install-env.sh"
  install_env_load "${CONFIG_ROOT}/install.env" "${INSTALL_ENV_NAMES[@]}"
}

# Walk the source root path top-down. Existing components are validated
# (canonical, root/launcher-owned, not group/world-writable); with
# create=true the missing tail is created component by component with
# mode 0700, so the durable checkout root is launcher-owned and private
# without ever relaxing an existing directory's permissions.
walk_source_root() {
  local create="$1" component="" part
  local -a parts=()
  [[ "${SOURCE_ROOT}" == /* ]] || fail "source root must be an absolute path: ${SOURCE_ROOT}"
  case "${SOURCE_ROOT}" in
    /|*/|*/../*|*/..|*/./*|*/.) fail "source root must be a canonical path without traversal: ${SOURCE_ROOT}" ;;
  esac
  [[ "${SOURCE_ROOT}" != *"//"* ]] ||
    fail "source root must not contain empty path components: ${SOURCE_ROOT}"
  IFS='/' read -r -a parts <<<"${SOURCE_ROOT#/}"
  for part in "${parts[@]}"; do
    component="${component%/}/${part}"
    if [[ -e "${component}" || -L "${component}" ]]; then
      validate_source_component "${component}"
    elif [[ "${create}" == true ]]; then
      mkdir -m 0700 -- "${component}" ||
        fail "cannot create source root component ${component}"
    fi
  done
  TARGET="${SOURCE_ROOT}/${VERSION}"
}

# Lifecycle selection (the host bootstrap keeps every Docker authority):
# - explicit --root-lifecycle wins without probing Docker;
# - no Docker client on a fresh host selects the deliberate root
#   lifecycle, because a freshly installed Docker grants no non-root
#   daemon access and the Docker permission model is never modified;
# - a reachable Docker daemon selects the normal launcher lifecycle;
# - a Docker client this user cannot use fails closed: no docker-group
#   edits, no silent root fallback.
select_lifecycle() {
  if [[ "${ROOT_LIFECYCLE}" == true ]]; then
    echo "lifecycle: root (explicit --root-lifecycle)"
    return
  fi
  if ! command -v docker >/dev/null 2>&1; then
    ROOT_LIFECYCLE=true
    echo "lifecycle: root (no Docker client installed; the host bootstrap will install Docker, and a fresh installation grants no non-root daemon access)"
    return
  fi
  if docker info >/dev/null 2>&1; then
    echo "lifecycle: launcher (Docker daemon reachable for this user)"
    return
  fi
  fail "the Docker client is installed but this user cannot use the Docker daemon; the Docker permission model is never modified here — grant this user Docker daemon access per Docker's official post-install steps and rerun, or pass --root-lifecycle explicitly"
}

validate_checkout() {
  local label="$1" origin_url head tag
  local -a matching=()
  [[ -d "${TARGET}" && ! -L "${TARGET}" ]] ||
    fail "refusing ${label} target ${TARGET}: it exists but is not a regular directory; move it aside deliberately"
  [[ "$(git -C "${TARGET}" rev-parse --is-inside-work-tree 2>/dev/null)" == "true" ]] ||
    fail "${label} ${TARGET} is not a Git repository"
  origin_url="$(git -C "${TARGET}" config --get remote.origin.url 2>/dev/null)" || origin_url=""
  [[ "${origin_url}" == "${REPOSITORY_URL}" ]] ||
    fail "${label} at ${TARGET} tracks origin '${origin_url:-<none>}' instead of ${REPOSITORY_URL}; refusing to reuse or overwrite it"
  head="$(git -C "${TARGET}" rev-parse HEAD 2>/dev/null)" ||
    fail "cannot resolve HEAD in ${label} ${TARGET}"
  while IFS= read -r tag; do
    [[ "${tag}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] && matching+=("${tag}")
  done < <(git -C "${TARGET}" tag --points-at "${head}")
  if (( ${#matching[@]} != 1 )) || [[ "${matching[0]:-}" != "${VERSION}" ]]; then
    fail "${label} at ${TARGET} is not exactly at the requested tag ${VERSION} (release tags at HEAD: ${matching[*]:-none}); refusing to reuse or overwrite it"
  fi
  git -C "${TARGET}" diff --quiet --exit-code -- . ||
    fail "${label} at ${TARGET} is dirty (unstaged changes); refusing to reuse or overwrite it"
  git -C "${TARGET}" diff --cached --quiet --exit-code -- . ||
    fail "${label} at ${TARGET} is dirty (staged changes); refusing to reuse or overwrite it"
  [[ -z "$(git -C "${TARGET}" status --porcelain --untracked-files=all)" ]] ||
    fail "${label} at ${TARGET} is dirty (untracked files); refusing to reuse or overwrite it"
}

prepare_checkout() {
  if [[ -e "${TARGET}" || -L "${TARGET}" ]]; then
    validate_checkout "existing"
    echo "reusing verified clean ${VERSION} checkout: ${TARGET}"
    return
  fi
  # mkdir claims the target atomically against a concurrent bootstrap;
  # git clone accepts an existing empty directory. Anything under the
  # claimed directory was created by this invocation, so the failure path
  # may remove exactly that directory and never pre-existing content.
  mkdir -m 0700 -- "${TARGET}" || fail "cannot create ${TARGET}"
  echo "cloning ${REPOSITORY_URL} at ${VERSION} into ${TARGET}"
  if ! git clone --quiet --branch "${VERSION}" --single-branch --depth 1 \
    "${REPOSITORY_URL}" "${TARGET}"; then
    rm -rf -- "${TARGET}"
    fail "cloning ${REPOSITORY_URL} at ${VERSION} failed; the release tag may not exist — check the published releases"
  fi
  if ! (validate_checkout "cloned"); then
    rm -rf -- "${TARGET}"
    fail "the cloned checkout at ${TARGET} did not satisfy the exact-${VERSION} clean-checkout contract"
  fi
  echo "cloned and verified clean ${VERSION} checkout: ${TARGET}"
}

check_release_installer() {
  [[ -x "${TARGET}/deploy/production/install.sh" && ! -L "${TARGET}/deploy/production/install.sh" ]] ||
    fail "release ${VERSION} at ${TARGET} does not ship the production installer (deploy/production/install.sh is missing or not executable); use a release that does"
}

require_run_tools() {
  command -v git >/dev/null 2>&1 ||
    fail "git is required to prepare the release checkout"
  if (( EUID != 0 )); then
    command -v sudo >/dev/null 2>&1 ||
      fail "sudo is required for a non-root launcher (the production installer invokes it for the host bootstrap and the root lifecycle)"
  fi
}

# --check-only, read-only presence probe: a reachable tag alone does not
# make a release installable through this bootstrap — it must also ship
# deploy/production/install.sh. A HEAD request against the tagged raw
# content answers that without cloning (the executed installer still comes
# from the verified clone, never from this probe).
check_release_installer_published() {
  local url
  [[ "${REPOSITORY_URL}" == https://github.com/* ]] ||
    fail "cannot probe release content for repository ${REPOSITORY_URL}"
  url="https://raw.githubusercontent.com/${REPOSITORY_URL#https://github.com/}/${VERSION}/deploy/production/install.sh"
  if ! curl -fsSLI -o /dev/null --proto '=https' --tlsv1.2 "${url}" 2>/dev/null; then
    fail "release ${VERSION} does not appear to ship deploy/production/install.sh; this bootstrap hands off only to releases that do"
  fi
  echo "release installer: deploy/production/install.sh is published for ${VERSION}"
}

run_check() {
  local key tag_ref
  command -v git >/dev/null 2>&1 || fail "git is required to inspect the release tag"
  command -v curl >/dev/null 2>&1 ||
    fail "curl is required to check the release and download the release bundle"
  if (( EUID != 0 )); then
    command -v sudo >/dev/null 2>&1 ||
      fail "sudo is required for a non-root launcher (the production installer invokes it for the host bootstrap and the root lifecycle)"
  fi
  load_config
  key="${OCSERV_CONTROLLER_RELEASE_PUBLIC_KEY:-}"
  [[ -n "${key}" ]] ||
    fail "OCSERV_CONTROLLER_RELEASE_PUBLIC_KEY is not set; provision the release-signing public key through an independent protected channel (set it in the environment or ${CONFIG_ROOT}/install.env)"
  [[ "${key}" == /* ]] ||
    fail "OCSERV_CONTROLLER_RELEASE_PUBLIC_KEY must be an absolute path to the trusted public key file"
  [[ -f "${key}" && ! -L "${key}" && -r "${key}" ]] ||
    fail "OCSERV_CONTROLLER_RELEASE_PUBLIC_KEY does not point to a readable regular file: ${key}"
  walk_source_root false
  echo "version: ${VERSION} (exact vX.Y.Z release tag)"
  echo "configuration directory: ${CONFIG_ROOT}"
  echo "release trust: OCSERV_CONTROLLER_RELEASE_PUBLIC_KEY=${key}"
  echo "source root: ${SOURCE_ROOT}"
  select_lifecycle
  tag_ref="$(git ls-remote "${REPOSITORY_URL}" "refs/tags/${VERSION}")"
  [[ -n "${tag_ref}" ]] ||
    fail "release tag ${VERSION} was not found on ${REPOSITORY_URL}; check the published releases"
  echo "release tag: ${VERSION} is reachable on ${REPOSITORY_URL}"
  check_release_installer_published
  echo "check passed: no host state was modified; rerun without --check to bootstrap"
}

hand_off() {
  local installer="${TARGET}/deploy/production/install.sh"
  if [[ "${ROOT_LIFECYCLE}" == true ]]; then
    # The installer performs its own controlled sudo re-exec for the root
    # lifecycle and resolves install.env from the invoking directory,
    # which is still ${CONFIG_ROOT}: this script never changed PWD.
    echo "handing off to the production installer: ${installer} --root-lifecycle (configuration stays in ${CONFIG_ROOT})"
    # An operator shell must never pre-set the installer's internal
    # install.env resolution marker: it would suppress the launcher-side
    # install.env parsing this bootstrap hands off to.
    unset OCSERV_INSTALL_ENV_RESOLVED
    exec "${installer}" --root-lifecycle
  fi
  echo "handing off to the production installer: ${installer} (configuration stays in ${CONFIG_ROOT})"
  unset OCSERV_INSTALL_ENV_RESOLVED
  exec "${installer}"
}

require_run_tools
resolve_source_root

if [[ "${CHECK_ONLY}" == true ]]; then
  run_check
  exit 0
fi

load_config
select_lifecycle
walk_source_root true
prepare_checkout
check_release_installer
hand_off
