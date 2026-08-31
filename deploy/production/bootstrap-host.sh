#!/usr/bin/env bash
# Minimal, idempotent prerequisite bootstrap for an ocservia Controller host.
#
# Scope: the host software required by the Controller lifecycle (Docker Engine
# and the Compose v2 plugin from Docker's official apt repository when Docker
# is absent, plus jq, flock, curl, openssl, and CA certificates) and the
# non-secret lifecycle directories. The bootstrap never creates or rotates
# business trust material (OIDC credentials, TLS keys, session/audit/signing
# keys, Iroh identities, relay tokens, database passwords), never changes the
# Docker permission model or the firewall, never upgrades, reinstalls, or
# removes an existing container runtime, and never uses the get.docker.com
# convenience script. Detected conflicts fail closed with an actionable
# message. controller.sh keeps its own prerequisite checks as the final
# fail-closed boundary; this script only prepares the host.
#
# Supported hosts: Ubuntu 24.04 on amd64 or arm64.
#
# Environment seams (automation and fixture tests):
#   OCSERV_CONTROLLER_STATE_ROOT / OCSERV_CONTROLLER_STATE_DIR
#     Lifecycle state root, resolved exactly like controller.sh
#     (default /var/lib/ocservia-controller).
#   OCSERV_BOOTSTRAP_OS_RELEASE       os-release file (default /etc/os-release)
#   OCSERV_BOOTSTRAP_APT_KEYRING_DIR  default /etc/apt/keyrings
#   OCSERV_BOOTSTRAP_APT_SOURCES_DIR  default /etc/apt/sources.list.d
set -euo pipefail

OS_RELEASE_FILE="${OCSERV_BOOTSTRAP_OS_RELEASE:-/etc/os-release}"
KEYRING_DIR="${OCSERV_BOOTSTRAP_APT_KEYRING_DIR:-/etc/apt/keyrings}"
APT_SOURCES_DIR="${OCSERV_BOOTSTRAP_APT_SOURCES_DIR:-/etc/apt/sources.list.d}"
STATE_ROOT="${OCSERV_CONTROLLER_STATE_ROOT:-${OCSERV_CONTROLLER_STATE_DIR:-/var/lib/ocservia-controller}}"
COMMAND=""
BACKUP_DIR=""
ARCH_WORD=""
OS_ID=""
OS_VERSION_ID=""
OS_CODENAME=""
launcher_user=""
launcher_uid=""
launcher_gid=""
failures=()
pending=()

usage() {
  cat >&2 <<'EOF'
usage: bootstrap-host.sh check [--backup-dir /path]
       bootstrap-host.sh install [--backup-dir /path]

check    read-only report of the Controller host prerequisites
install  install only the missing prerequisites (root/sudo, idempotent)
EOF
  exit 2
}

fail() {
  echo "controller host bootstrap: $1" >&2
  exit 1
}

record_failure() {
  failures+=("$1")
  echo "controller host bootstrap: [fail] $1" >&2
}

record_pending() {
  pending+=("$1")
}

package_installed() {
  [[ "$(dpkg-query -W -f='${db:Status-Abbrev}' "$1" 2>/dev/null)" == "ii "* ]]
}

docker_client_present() {
  command -v docker >/dev/null 2>&1
}

docker_daemon_active() {
  docker info >/dev/null 2>&1
}

compose_plugin_available() {
  docker_client_present && docker compose version >/dev/null 2>&1
}

compose_supports_wait() {
  docker compose up --help 2>&1 | grep -q -- '--wait'
}

detect_platform() {
  if [[ ! -r "${OS_RELEASE_FILE}" ]]; then
    fail "cannot read the OS release file ${OS_RELEASE_FILE}; supported hosts are Ubuntu 24.04 on amd64/arm64"
  fi
  # shellcheck disable=SC1090
  . "${OS_RELEASE_FILE}"
  # shellcheck disable=SC2154
  OS_ID="${ID:-}"
  # shellcheck disable=SC2154
  OS_VERSION_ID="${VERSION_ID:-}"
  # shellcheck disable=SC2154
  OS_CODENAME="${VERSION_CODENAME:-${UBUNTU_CODENAME:-}}"
  if [[ "${OS_ID}" != ubuntu || "${OS_VERSION_ID}" != 24.04 ]]; then
    fail "unsupported OS '${OS_ID:-unknown} ${OS_VERSION_ID:-unknown}'; supported hosts are Ubuntu 24.04 on amd64/arm64"
  fi
  [[ -n "${OS_CODENAME}" ]] ||
    fail "the Ubuntu 24.04 OS release file is missing VERSION_CODENAME"
  case "$(uname -m)" in
    x86_64) ARCH_WORD=amd64 ;;
    aarch64) ARCH_WORD=arm64 ;;
    *) fail "unsupported architecture '$(uname -m)'; supported hosts are Ubuntu 24.04 on amd64/arm64" ;;
  esac
  echo "platform: Ubuntu ${OS_VERSION_ID} (${ARCH_WORD})"
}

resolve_launcher() {
  if (( EUID == 0 )); then
    launcher_user="${SUDO_USER:-root}"
  else
    launcher_user="$(id -un)"
  fi
  launcher_uid="$(id -u "${launcher_user}")"
  launcher_gid="$(id -g "${launcher_user}")"
}

detect_conflicts() {
  local package
  for package in docker.io docker-doc podman-docker; do
    if package_installed "${package}"; then
      record_failure "conflicting package '${package}' is installed; remove it deliberately (this bootstrap never uninstalls an existing runtime); see https://docs.docker.com/engine/install/ubuntu/"
    fi
  done
  if command -v docker-compose >/dev/null 2>&1; then
    if compose_plugin_available; then
      echo "note: a standalone 'docker-compose' binary is on PATH alongside the Compose v2 plugin; the Controller lifecycle uses 'docker compose' and no action was taken"
    else
      record_failure "standalone 'docker-compose' is installed while the Docker Compose v2 plugin is unavailable; align the existing installation deliberately instead of letting this bootstrap shadow it"
    fi
  fi
}

install_missing_base_tools() {
  local package missing=()
  for package in jq util-linux curl openssl ca-certificates; do
    package_installed "${package}" || missing+=("${package}")
  done
  if ((${#missing[@]} == 0)); then
    echo "base tools present: jq, flock (util-linux), curl, openssl, ca-certificates"
    return 0
  fi
  echo "installing missing base tools: ${missing[*]}"
  apt-get update
  DEBIAN_FRONTEND=noninteractive apt-get install -y "${missing[@]}"
}

setup_docker_apt_repository() {
  local source_file="${APT_SOURCES_DIR}/docker.list" expected
  echo "configuring the Docker official apt repository"
  install -m 0755 -d "${KEYRING_DIR}"
  curl -fsSL "https://download.docker.com/linux/${OS_ID}/gpg" -o "${KEYRING_DIR}/docker.asc"
  chmod a+r "${KEYRING_DIR}/docker.asc"
  expected="$(printf 'deb [arch=%s signed-by=%s/docker.asc] https://download.docker.com/linux/%s %s stable' \
    "${ARCH_WORD}" "${KEYRING_DIR}" "${OS_ID}" "${OS_CODENAME}")"
  if [[ -e "${source_file}" ]]; then
    [[ "$(cat -- "${source_file}")" == "${expected}" ]] ||
      fail "refusing to overwrite an existing ${source_file} that differs from the official Docker repository line"
    echo "Docker apt source already configured: ${source_file}"
  else
    printf '%s\n' "${expected}" >"${source_file}"
    echo "Docker apt source written: ${source_file}"
  fi
  apt-get update
}

install_docker_engine() {
  setup_docker_apt_repository
  echo "installing Docker Engine and the Compose plugin from the official repository"
  DEBIAN_FRONTEND=noninteractive apt-get install -y \
    docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
}

revive_docker_daemon() {
  docker_daemon_active && return 0
  if command -v systemctl >/dev/null 2>&1; then
    echo "Docker daemon is not active; enabling and starting the docker service"
    systemctl enable --now docker
  fi
}

validate_docker_readiness() {
  if ! docker_client_present; then
    record_failure "Docker is required but no usable docker client is installed"
    return
  fi
  if ! docker_daemon_active; then
    record_failure "Docker daemon is not active; investigate with 'systemctl status docker'"
    return
  fi
  echo "docker: server $(docker version --format '{{.Server.Version}}' 2>/dev/null || echo present)"
  if ! docker compose version >/dev/null 2>&1; then
    record_failure "Docker Compose v2 is required by the Controller lifecycle"
    return
  fi
  echo "compose: $(docker compose version)"
  compose_supports_wait ||
    record_failure "Docker Compose must support 'docker compose up --wait'; upgrade the existing Compose installation deliberately (this bootstrap never upgrades an existing Docker installation)"
}

check_docker() {
  if ! docker_client_present; then
    record_failure "Docker is required but not installed; run 'bootstrap-host.sh install' to install it from the official Docker apt repository"
    return
  fi
  if ! docker_daemon_active; then
    record_failure "Docker daemon is not active; start it with 'sudo systemctl enable --now docker'"
  else
    echo "docker: server $(docker version --format '{{.Server.Version}}' 2>/dev/null || echo present)"
  fi
  if ! docker compose version >/dev/null 2>&1; then
    record_failure "Docker Compose v2 plugin is required; align the existing Docker installation deliberately (this bootstrap never upgrades an existing runtime)"
    return
  fi
  echo "compose: $(docker compose version)"
  compose_supports_wait ||
    record_failure "Docker Compose must support 'docker compose up --wait'"
}

check_base_tools() {
  local package any_missing=false
  for package in jq util-linux curl openssl ca-certificates; do
    if ! package_installed "${package}"; then
      record_failure "required package '${package}' is not installed; run 'bootstrap-host.sh install' to install missing host prerequisites"
      any_missing=true
    fi
  done
  if [[ "${any_missing}" == false ]]; then
    echo "base tools present: jq, flock (util-linux), curl, openssl, ca-certificates"
  fi
}

state_root_stat() {
  stat -c '%u:%a' "${STATE_ROOT}"
}

check_state_root() {
  local stat_line uid mode
  if [[ ! -e "${STATE_ROOT}" && ! -L "${STATE_ROOT}" ]]; then
    record_pending "lifecycle state root ${STATE_ROOT} is not provisioned yet; 'bootstrap-host.sh install' (or controller.sh) creates it"
    return
  fi
  if [[ -L "${STATE_ROOT}" || ! -d "${STATE_ROOT}" ]]; then
    record_failure "state root ${STATE_ROOT} must be a real directory and not a symlink"
    return
  fi
  stat_line="$(state_root_stat)"
  uid="${stat_line%%:*}"
  mode="${stat_line##*:}"
  if [[ "${mode}" != 700 || ( "${uid}" != 0 && "${uid}" != "${launcher_uid}" ) ]]; then
    record_failure "state root ${STATE_ROOT} must be mode 0700 owned by root or the launcher user (found uid ${uid}, mode ${mode})"
    return
  fi
  echo "state root validated: ${STATE_ROOT} (mode 0700)"
}

ensure_state_root() {
  local stat_line uid mode parent
  if [[ -L "${STATE_ROOT}" ]]; then
    fail "state root ${STATE_ROOT} must not be a symlink"
  fi
  if [[ -e "${STATE_ROOT}" ]]; then
    [[ -d "${STATE_ROOT}" ]] || fail "state root ${STATE_ROOT} must be a directory"
    stat_line="$(state_root_stat)"
    uid="${stat_line%%:*}"
    mode="${stat_line##*:}"
    if [[ "${mode}" != 700 || ( "${uid}" != 0 && "${uid}" != "${launcher_uid}" ) ]]; then
      fail "existing state root ${STATE_ROOT} must be mode 0700 owned by root or ${launcher_user} (uid ${launcher_uid}); fix it deliberately — ownership is only set when this bootstrap creates the directory"
    fi
    echo "state root validated: ${STATE_ROOT} (mode 0700)"
    return
  fi
  parent="$(dirname -- "${STATE_ROOT}")"
  [[ -d "${parent}" ]] || fail "state root parent ${parent} does not exist; create it first"
  mkdir -m 0700 -- "${STATE_ROOT}"
  chown "${launcher_uid}:${launcher_gid}" -- "${STATE_ROOT}"
  echo "state root created: ${STATE_ROOT} (owner ${launcher_user}, mode 0700)"
}

check_backup_dir() {
  local stat_line
  if [[ ! -e "${BACKUP_DIR}" && ! -L "${BACKUP_DIR}" ]]; then
    record_pending "backup directory ${BACKUP_DIR} does not exist yet; 'bootstrap-host.sh install --backup-dir' creates it"
    return
  fi
  if [[ -L "${BACKUP_DIR}" || ! -d "${BACKUP_DIR}" ]]; then
    record_failure "backup directory ${BACKUP_DIR} must be a real directory and not a symlink"
    return
  fi
  stat_line="$(stat -c '%u:%g:%a' "${BACKUP_DIR}")"
  [[ "${stat_line}" == "999:999:700" ]] ||
    record_failure "backup directory ${BACKUP_DIR} must be owned by uid:gid 999:999 with mode 0700 (found ${stat_line})"
}

ensure_backup_dir() {
  local stat_line
  if [[ -L "${BACKUP_DIR}" ]]; then
    fail "backup directory ${BACKUP_DIR} must not be a symlink"
  fi
  if [[ -e "${BACKUP_DIR}" ]]; then
    [[ -d "${BACKUP_DIR}" ]] || fail "backup directory ${BACKUP_DIR} must be a directory"
    stat_line="$(stat -c '%u:%g:%a' "${BACKUP_DIR}")"
    [[ "${stat_line}" == "999:999:700" ]] ||
      fail "existing backup directory ${BACKUP_DIR} must be owned by uid:gid 999:999 with mode 0700; fix it deliberately with: install -d -o 999 -g 999 -m 0700 ${BACKUP_DIR}"
    echo "backup directory validated: ${BACKUP_DIR} (999:999, mode 0700)"
    return
  fi
  install -d -o 999 -g 999 -m 0700 -- "${BACKUP_DIR}"
  echo "backup directory created: ${BACKUP_DIR} (999:999, mode 0700)"
}

print_operator_prerequisites() {
  local secret_dir="${OCSERV_SECRET_DIR:-}" secret_stat secret_uid secret_mode
  local backup_env="${OCSERV_BACKUP_DIR:-}" backup_stat
  local public_key="${OCSERV_CONTROLLER_RELEASE_PUBLIC_KEY:-}"

  echo
  echo "Operator prerequisites outside this bootstrap's scope:"
  if [[ -z "${secret_dir}" ]]; then
    record_pending "OCSERV_SECRET_DIR is not set; provision the launcher-owned mode-0700 secret directory described in docs/operations/production-deployment.md (this bootstrap never creates secrets)"
  elif [[ ! -d "${secret_dir}" || -L "${secret_dir}" ]]; then
    record_pending "OCSERV_SECRET_DIR (${secret_dir}) is not an existing real directory"
  else
    secret_stat="$(stat -c '%u:%a' "${secret_dir}" 2>/dev/null || echo unknown)"
    secret_uid="${secret_stat%%:*}"
    secret_mode="${secret_stat##*:}"
    if [[ "${secret_mode}" != 700 || ( "${secret_uid}" != 0 && "${secret_uid}" != "${launcher_uid}" ) ]]; then
      record_pending "OCSERV_SECRET_DIR (${secret_dir}) must be launcher- or root-owned with mode 0700 (found ${secret_stat})"
    else
      echo "- OCSERV_SECRET_DIR (${secret_dir}) is a compliant directory; the launcher still validates every secret file"
    fi
  fi

  if [[ -n "${BACKUP_DIR}" && -z "${backup_env}" ]]; then
    record_pending "export OCSERV_BACKUP_DIR=${BACKUP_DIR} for the lifecycle environment"
  fi
  if [[ -z "${backup_env}" ]]; then
    record_pending "OCSERV_BACKUP_DIR is not set; provision the 999:999 mode-0700 backup bind mount described in docs/operations/production-deployment.md"
  elif [[ ! -d "${backup_env}" || -L "${backup_env}" ]]; then
    record_pending "OCSERV_BACKUP_DIR (${backup_env}) is not an existing real directory"
  else
    backup_stat="$(stat -c '%u:%g:%a' "${backup_env}" 2>/dev/null || echo unknown)"
    if [[ "${backup_stat}" != "999:999:700" ]]; then
      record_pending "OCSERV_BACKUP_DIR (${backup_env}) must be owned by uid:gid 999:999 with mode 0700 (found ${backup_stat})"
    else
      echo "- OCSERV_BACKUP_DIR (${backup_env}) is compliant"
    fi
  fi

  if [[ -z "${public_key}" ]]; then
    record_pending "OCSERV_CONTROLLER_RELEASE_PUBLIC_KEY is not set; provision the release-signing public key through an independent protected channel"
  elif [[ ! -f "${public_key}" || -L "${public_key}" ]]; then
    record_pending "OCSERV_CONTROLLER_RELEASE_PUBLIC_KEY (${public_key}) is not an existing regular file"
  else
    echo "- OCSERV_CONTROLLER_RELEASE_PUBLIC_KEY (${public_key}) is present"
  fi

  if ! command -v git >/dev/null 2>&1; then
    record_pending "git is required by controller.sh (a clean checkout matching the release manifest); install it before 'controller.sh install'"
  fi
  record_pending "download the controller-release-${ARCH_WORD}.json release bundle (manifest, SHA256SUMS, SHA256SUMS.sig, and the manifest .sha256) for this host architecture through a protected channel"

  if command -v ufw >/dev/null 2>&1; then
    if (( EUID == 0 )) && ufw status 2>/dev/null | grep -q '^Status: active'; then
      echo "- warning: ufw is active; Docker published ports bypass ufw by default, so review the Docker/ufw interaction before exposing TCP 443 (this bootstrap never modifies the firewall)" >&2
    else
      echo "- note: ufw is installed; verify its interaction with Docker published ports (this bootstrap never modifies the firewall)"
    fi
  fi

  local item
  for item in "${pending[@]}"; do
    echo "- pending: ${item}"
  done
}

finish() {
  print_operator_prerequisites
  if ((${#failures[@]} > 0)); then
    echo "controller host bootstrap: ${#failures[@]} host prerequisite failure(s) remain; see the [fail] lines above" >&2
    exit 1
  fi
  echo "Controller host prerequisites satisfied; the pending operator prerequisites above remain outside this bootstrap's scope"
}

run_check() {
  detect_platform
  resolve_launcher
  detect_conflicts
  check_docker
  check_base_tools
  check_state_root
  if [[ -n "${BACKUP_DIR}" ]]; then
    check_backup_dir
  fi
  finish
}

run_install() {
  (( EUID == 0 )) || fail "install must run as root or via sudo"
  detect_platform
  resolve_launcher
  detect_conflicts
  if ((${#failures[@]} > 0)); then
    echo "controller host bootstrap: refusing to mutate the host while conflicting software is installed" >&2
    exit 1
  fi

  if docker_client_present; then
    echo "existing Docker detected; preserving it without reinstall or upgrade"
    install_missing_base_tools
  else
    echo "Docker is absent; installing Docker Engine and the Compose plugin from the official Docker apt repository"
    install_missing_base_tools
    install_docker_engine
  fi
  revive_docker_daemon
  validate_docker_readiness

  ensure_state_root
  if [[ -n "${BACKUP_DIR}" ]]; then
    ensure_backup_dir
  fi
  finish
}

(($# >= 1)) || usage
COMMAND="$1"
shift
case "${COMMAND}" in
  check|install) ;;
  *) usage ;;
esac
if (($# > 0)); then
  [[ "$1" == "--backup-dir" && $# -eq 2 ]] || usage
  BACKUP_DIR="$2"
  [[ "${BACKUP_DIR}" == /* ]] || {
    echo "controller host bootstrap: --backup-dir must be an absolute path" >&2
    exit 2
  }
fi

case "${COMMAND}" in
  check) run_check ;;
  install) run_install ;;
esac
