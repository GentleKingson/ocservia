#!/usr/bin/env bash
# Thin production orchestrator for a one-command Controller install.
#
# Scope: starting from a clean checkout of an exact vX.Y.Z release tag with the
# operator-provisioned production environment already exported, run the host
# bootstrap (via sudo), download the release bundle for the host architecture
# into the protected lifecycle state root, and delegate activation to
# controller.sh install. This script never reimplements host bootstrap, release
# verification, or Controller lifecycle logic: bootstrap-host.sh,
# verify-controller-release-bundle.sh, and controller.sh remain the
# authorities. It never creates, replaces, or defaults production secrets or
# trust material; the release-signing public key must still come from
# OCSERV_CONTROLLER_RELEASE_PUBLIC_KEY through an independent protected
# channel, never from the release bundle itself.
#
# There is deliberately no curl|bash form: the Controller lifecycle binds to a
# clean exact release Git checkout, the manifest source_commit, and the
# operator-provisioned trust key.
#
# Usage model:
#   git clone --branch vX.Y.Z --depth 1 <ocservia repository>
#   cd ocservia
#   export OCSERV_BACKUP_DIR=... OCSERV_SECRET_DIR=...
#   export OCSERV_CONTROLLER_RELEASE_PUBLIC_KEY=...
#   # export the remaining production Controller configuration...
#   # (or keep the allowlisted configuration in ./install.env, parsed by the
#   # strict non-executing loader in deploy/lib/install-env.sh; explicit
#   # shell variables always win over the file)
#   deploy/production/install.sh                          # launcher user with Docker access
#   deploy/production/install.sh --root-lifecycle         # deliberate root lifecycle (fresh host)
#
# Launcher contract: run this script as the lifecycle launcher user, not as a
# whole-script sudo invocation. bootstrap-host.sh provisions the state root
# for the SUDO_USER launcher while controller.sh validates state ownership
# against the actual invoking user, so 'sudo install.sh' would mismatch them;
# In launcher mode sudo is invoked internally only for the host bootstrap
# step. A deliberate whole-lifecycle-as-root install is available through
# 'install.sh --root-lifecycle': it reaches root through a controlled sudo env,
# then strips SUDO_USER (which sudo -i retains) so the bootstrap provisions the
# state root for the same root user that activates the Controller, and never
# infers intent from SUDO_COMMAND. On a host without a
# Docker client the fresh-host path additionally requires this root lifecycle
# mode: a fresh Docker installation grants no non-root daemon access, and
# this installer never modifies the Docker permission model, so a non-root
# launcher fails closed up front instead of after host mutation.
set -euo pipefail
umask 077

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
BOOTSTRAP="${ROOT}/deploy/production/bootstrap-host.sh"
CONTROLLER="${ROOT}/deploy/production/controller.sh"
DOWNLOAD_BASE="https://github.com/GentleKingson/ocservia/releases/download"
RELEASE_TAG=""
RELEASE_COMMIT=""
ARCH_WORD=""
BUNDLE_DIR=""
ROOT_LIFECYCLE=false

# Only operator-supplied production configuration crosses the internal sudo
# boundary. Release image variables are generated from the verified manifest;
# bootstrap and test seams are intentionally not forwarded.
ROOT_LIFECYCLE_ENV_NAMES=(
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
  echo "controller install: $1" >&2
  exit 1
}

if (($# > 1)); then
  echo "usage: deploy/production/install.sh [--root-lifecycle] (the production environment comes from the operator session or ./install.env)" >&2
  exit 2
fi
case "${1:-}" in
  "") ;;
  --root-lifecycle) ROOT_LIFECYCLE=true ;;
  *)
    echo "usage: deploy/production/install.sh [--root-lifecycle] (the production environment comes from the operator session or ./install.env)" >&2
    exit 2
    ;;
esac

# Operator configuration comes from the invoking shell environment and, for
# allowlisted variables not exported there, from $PWD/install.env. The
# strict non-executing loader fail-closes on unknown keys, expansion
# syntax, and unsafe file metadata, so a configuration error surfaces
# before any host mutation below; an absent file is a no-op and explicit
# shell variables always win. OCSERV_INSTALL_ENV_RESOLVED marks the
# deliberate --root-lifecycle re-exec: the launcher user already resolved
# install.env under its own privileges and forwarded the effective values
# across the sudo boundary, so root must not treat a possibly-replaced
# $PWD/install.env as new authoritative input.
if [[ -z "${OCSERV_INSTALL_ENV_RESOLVED:-}" ]]; then
  # shellcheck source=../lib/install-env.sh disable=SC1091
  source "${ROOT}/deploy/lib/install-env.sh"
  install_env_load "${PWD}/install.env" \
    OCSERV_AUDIT_EVENT_KEY_ID \
    OCSERV_BACKUP_DIR \
    OCSERV_BACKUP_INTERVAL_SECONDS \
    OCSERV_BACKUP_RETENTION_COUNT \
    OCSERV_CERTIFICATE_SIGNER_URL \
    OCSERV_CONTROLLER_ENDPOINT_ID \
    OCSERV_CONTROLLER_PUBLIC_URL \
    OCSERV_CONTROLLER_RELEASE_PUBLIC_KEY \
    OCSERV_CONTROLLER_STATE_DIR \
    OCSERV_CONTROLLER_STATE_ROOT \
    OCSERV_HTTPS_ADDRESS \
    OCSERV_OIDC_CLIENT_ID \
    OCSERV_OIDC_ISSUER \
    OCSERV_OTEL_BACKEND_ENDPOINT \
    OCSERV_PUBLIC_HOST \
    OCSERV_RELAY_URL_A \
    OCSERV_RELAY_URL_B \
    OCSERV_SECRET_DIR
fi
# Every configuration-derived variable is computed only after the
# configuration sources above are final.
STATE_ROOT="${OCSERV_CONTROLLER_STATE_ROOT:-${OCSERV_CONTROLLER_STATE_DIR:-/var/lib/ocservia-controller}}"

forward_root_lifecycle() {
  local variable allowed
  local -a forwarded_environment=()
  while IFS= read -r variable; do
    for allowed in "${ROOT_LIFECYCLE_ENV_NAMES[@]}"; do
      [[ "${variable}" == "${allowed}" ]] || continue
      forwarded_environment+=("${variable}=${!variable}")
      break
    done
  done < <(compgen -e)
  command -v sudo >/dev/null 2>&1 ||
    fail "sudo is required for --root-lifecycle when the installer is not already running as root"
  # OCSERV_INSTALL_ENV_RESOLVED tells the root re-exec that install.env was
  # already resolved by the launcher user: root must not re-read a file that
  # may have been replaced across the privilege transition.
  exec sudo env "${forwarded_environment[@]+"${forwarded_environment[@]}"}" \
    OCSERV_INSTALL_ENV_RESOLVED=1 \
    "${ROOT}/deploy/production/install.sh" --root-lifecycle
}

if [[ "${ROOT_LIFECYCLE}" == true ]]; then
  (( EUID == 0 )) || forward_root_lifecycle
  # bootstrap-host.sh resolves its launcher from SUDO_USER, which sudo -i
  # retains: strip it so the state root is provisioned for the same root user
  # that activates the Controller. SUDO_UID/SUDO_GID stay untouched — git
  # trusts a checkout owned by the sudo-invoking user exactly through them.
  unset SUDO_USER
fi

if (( EUID == 0 )) && [[ "${ROOT_LIFECYCLE}" == false ]]; then
  case "${SUDO_USER:-}" in
    ""|root) ;;
    *)
      fail "run install.sh as the lifecycle launcher user; the installer will invoke sudo only for host bootstrap (whole-script sudo from '${SUDO_USER}' would provision the state root for a launcher that never activates it); for a deliberate whole-lifecycle-as-root install run 'deploy/production/install.sh --root-lifecycle'"
      ;;
  esac
fi

[[ -x "${BOOTSTRAP}" && ! -L "${BOOTSTRAP}" ]] ||
  fail "Controller host bootstrap is missing or not executable: ${BOOTSTRAP}"
[[ -x "${CONTROLLER}" && ! -L "${CONTROLLER}" ]] ||
  fail "Controller lifecycle entrypoint is missing or not executable: ${CONTROLLER}"

resolve_release_identity() {
  local tag matching=()
  command -v git >/dev/null 2>&1 ||
    fail "git is required to identify the release checkout"
  RELEASE_COMMIT="$(git -C "${ROOT}" rev-parse HEAD 2>/dev/null)" ||
    fail "${ROOT} is not a Git checkout; install from a clean checkout of an exact vX.Y.Z release tag"
  while IFS= read -r tag; do
    [[ "${tag}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] && matching+=("${tag}")
  done < <(git -C "${ROOT}" tag --points-at HEAD)
  ((${#matching[@]} == 1)) ||
    fail "checkout HEAD must correspond to exactly one exact vX.Y.Z release tag (found ${#matching[@]}); check out the release tag to install, e.g. git clone --branch vX.Y.Z --depth 1 <repository>"
  RELEASE_TAG="${matching[0]}"
  [[ -z "$(git -C "${ROOT}" status --porcelain --untracked-files=all)" ]] ||
    fail "the checkout at ${RELEASE_TAG} is dirty; install from a clean checkout (controller.sh enforces the same contract)"
  echo "release identity: ${RELEASE_TAG} (${RELEASE_COMMIT})"
}

resolve_architecture() {
  case "$(uname -m)" in
    x86_64) ARCH_WORD=amd64 ;;
    aarch64) ARCH_WORD=arm64 ;;
    *) fail "unsupported architecture '$(uname -m)'; Controller releases support linux/amd64 and linux/arm64" ;;
  esac
  echo "host architecture: ${ARCH_WORD} (selecting controller-release-${ARCH_WORD}.json)"
}

verify_fresh_host_launcher_path() {
  # When the Docker client is absent entirely, the bootstrap installs Docker
  # from scratch. A fresh Docker installation grants no non-root daemon
  # access, and neither this installer nor bootstrap-host.sh ever modifies the
  # Docker permission model (group membership, socket, listeners), so the
  # bootstrap's launcher access verification would fail for a non-root
  # launcher only after the host had already been mutated. Fail closed before
  # any host mutation instead and hand the operator the two supported paths.
  if (( EUID != 0 )) && ! command -v docker >/dev/null 2>&1; then
    fail "no Docker client is installed, so the host bootstrap would install Docker from scratch; a fresh Docker installation grants no non-root daemon access and this installer never modifies the Docker permission model — run 'deploy/production/install.sh --root-lifecycle' for a deliberate root Controller lifecycle on this fresh host, or install Docker separately and deliberately grant this launcher Docker daemon access per Docker's official post-install steps, then rerun install.sh as the launcher"
  fi
}

run_host_bootstrap() {
  local sudo_env=() args=(install)
  # sudo resets the environment, so an explicitly configured state root must
  # cross the sudo boundary for the bootstrap to provision the same root the
  # launcher will activate.
  if [[ -n "${OCSERV_CONTROLLER_STATE_ROOT:-}" ]]; then
    sudo_env+=("OCSERV_CONTROLLER_STATE_ROOT=${OCSERV_CONTROLLER_STATE_ROOT}")
  elif [[ -n "${OCSERV_CONTROLLER_STATE_DIR:-}" ]]; then
    sudo_env+=("OCSERV_CONTROLLER_STATE_DIR=${OCSERV_CONTROLLER_STATE_DIR}")
  fi
  if [[ -n "${OCSERV_BACKUP_DIR:-}" ]]; then
    args+=(--backup-dir "${OCSERV_BACKUP_DIR}")
  fi
  if (( EUID == 0 )); then
    # Root runs the bootstrap directly; under --root-lifecycle the sudo
    # identity was already stripped at startup, so the state root is
    # provisioned for the same root user that activates the Controller.
    env "${sudo_env[@]+"${sudo_env[@]}"}" "${BOOTSTRAP}" "${args[@]}"
  else
    sudo env "${sudo_env[@]+"${sudo_env[@]}"}" "${BOOTSTRAP}" "${args[@]}"
  fi
}

validate_state_root() {
  case "${STATE_ROOT}" in
    /*) ;;
    *) fail "state root ${STATE_ROOT} must be an absolute path" ;;
  esac
  case "${STATE_ROOT}" in
    /|*/|*/../*|*/..|*/./*|*/.)
      fail "state root ${STATE_ROOT} must be a canonical path without traversal" ;;
  esac
  [[ -d "${STATE_ROOT}" && ! -L "${STATE_ROOT}" ]] ||
    fail "state root ${STATE_ROOT} was not provisioned by the host bootstrap; rerun deploy/production/install.sh from the launcher user"
}

download_release_bundle() {
  local name names=(
    "controller-release-${ARCH_WORD}.json"
    "controller-release-${ARCH_WORD}.json.sha256"
    "SHA256SUMS"
    "SHA256SUMS.sig"
  )
  # The release-signing public key is intentionally absent: trust comes only
  # from OCSERV_CONTROLLER_RELEASE_PUBLIC_KEY.
  BUNDLE_DIR="${STATE_ROOT}/release-bundles/${RELEASE_TAG}"
  if [[ -L "${STATE_ROOT}/release-bundles" || -L "${BUNDLE_DIR}" ]]; then
    fail "release bundle directory ${BUNDLE_DIR} must not be a symlink"
  fi
  mkdir -p -m 0700 -- "${BUNDLE_DIR}"
  for name in "${names[@]}"; do
    echo "downloading ${DOWNLOAD_BASE}/${RELEASE_TAG}/${name}"
    curl -fsSL --proto '=https' --tlsv1.2 \
      --output "${BUNDLE_DIR}/${name}" "${DOWNLOAD_BASE}/${RELEASE_TAG}/${name}"
    chmod 600 -- "${BUNDLE_DIR}/${name}"
  done
  echo "release bundle stored: ${BUNDLE_DIR}"
}

resolve_release_identity
resolve_architecture
[[ -n "${OCSERV_CONTROLLER_RELEASE_PUBLIC_KEY:-}" ]] ||
  fail "OCSERV_CONTROLLER_RELEASE_PUBLIC_KEY is not set; provision the release-signing public key through an independent protected channel (controller.sh verifies it)"
verify_fresh_host_launcher_path

run_host_bootstrap
validate_state_root
download_release_bundle

exec "${CONTROLLER}" install \
  --release-file "${BUNDLE_DIR}/controller-release-${ARCH_WORD}.json"
