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
# message.
#
# Launcher model: the Controller lifecycle runs as the sudo-invoking user
# (or as root when the bootstrap itself runs as root). That launcher user must
# already have Docker daemon access — configured deliberately by the operator
# per Docker's official post-install steps — or the whole flow must run as
# root; check and install verify the launcher's Docker access and fail closed
# with remediation. controller.sh keeps its own prerequisite checks as the
# final fail-closed boundary; this script only prepares the host.
#
# Supported hosts: Ubuntu 22.04, 24.04, and 26.04 and Debian 11, 12, and 13 on
# amd64 or arm64. Ubuntu 20.04 is a legacy compatibility host: it can run the
# Controller against an already-installed compatible Docker, but automatic
# Docker bootstrap is unavailable there because Ubuntu 20.04 is outside Docker's
# current official Ubuntu support matrix.
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
SUPPORTED_HOSTS="Ubuntu 20.04 (existing Docker only), 22.04, 24.04, 26.04, and Debian 11, 12, 13 on amd64/arm64"
COMMAND=""
BACKUP_DIR=""
ARCH_WORD=""
OS_ID=""
OS_VERSION_ID=""
OS_CODENAME=""
DOCKER_REPO_CODENAME=""
DOCKER_INSTALL_DOC=""
# full: Docker may be installed from the official Docker apt repository.
# existing-docker: only an already-installed compatible Docker is accepted.
SUPPORT_MODE=""
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
    fail "cannot read the OS release file ${OS_RELEASE_FILE}; supported hosts are ${SUPPORTED_HOSTS}"
  fi
  # shellcheck disable=SC1090
  . "${OS_RELEASE_FILE}"
  # shellcheck disable=SC2154
  OS_ID="${ID:-}"
  # shellcheck disable=SC2154
  OS_VERSION_ID="${VERSION_ID:-}"
  # Docker's official Ubuntu repository suite follows ${UBUNTU_CODENAME:-$VERSION_CODENAME};
  # the Debian repository suite uses $VERSION_CODENAME only.
  case "${OS_ID}" in
    ubuntu) OS_CODENAME="${UBUNTU_CODENAME:-${VERSION_CODENAME:-}}" ;;
    *) OS_CODENAME="${VERSION_CODENAME:-}" ;;
  esac
  case "${OS_ID} ${OS_VERSION_ID}" in
    "ubuntu 20.04") SUPPORT_MODE="existing-docker"; DOCKER_REPO_CODENAME="focal" ;;
    "ubuntu 22.04") SUPPORT_MODE="full"; DOCKER_REPO_CODENAME="jammy" ;;
    "ubuntu 24.04") SUPPORT_MODE="full"; DOCKER_REPO_CODENAME="noble" ;;
    "ubuntu 26.04") SUPPORT_MODE="full"; DOCKER_REPO_CODENAME="resolute" ;;
    "debian 11") SUPPORT_MODE="full"; DOCKER_REPO_CODENAME="bullseye" ;;
    "debian 12") SUPPORT_MODE="full"; DOCKER_REPO_CODENAME="bookworm" ;;
    "debian 13") SUPPORT_MODE="full"; DOCKER_REPO_CODENAME="trixie" ;;
    *)
      fail "unsupported OS '${OS_ID:-unknown} ${OS_VERSION_ID:-unknown}'; supported hosts are ${SUPPORTED_HOSTS}"
      ;;
  esac
  [[ -n "${OS_CODENAME}" ]] ||
    fail "the ${OS_ID} ${OS_VERSION_ID} OS release file is missing its distribution codename"
  [[ "${OS_CODENAME}" == "${DOCKER_REPO_CODENAME}" ]] ||
    fail "OS release codename '${OS_CODENAME}' does not match the expected '${DOCKER_REPO_CODENAME}' for ${OS_ID} ${OS_VERSION_ID}; derived or customized distributions are unsupported"
  case "$(uname -m)" in
    x86_64) ARCH_WORD=amd64 ;;
    aarch64) ARCH_WORD=arm64 ;;
    *) fail "unsupported architecture '$(uname -m)'; supported hosts are ${SUPPORTED_HOSTS}" ;;
  esac
  DOCKER_INSTALL_DOC="https://docs.docker.com/engine/install/${OS_ID}/"
  echo "platform: ${OS_ID} ${OS_VERSION_ID} (${DOCKER_REPO_CODENAME}, ${ARCH_WORD})"
  if [[ "${OS_ID} ${OS_VERSION_ID}" == "ubuntu 20.04" ]]; then
    echo "warning: Ubuntu 20.04 standard security maintenance ended 2025-05-31 and requires Ubuntu Pro/ESM or an equivalent maintenance strategy; this host supports only the existing-Docker compatibility path (this bootstrap never configures Ubuntu Pro/ESM)" >&2
  fi
  if [[ "${OS_ID} ${OS_VERSION_ID}" == "debian 11" ]]; then
    echo "warning: Debian 11 regular Debian LTS security maintenance ended 2026-08-31; production hosts need Debian ELTS or an equivalent sustained security-maintenance strategy (this bootstrap never configures ELTS)" >&2
  fi
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
  local package compose_packages=(docker-compose)
  # Ubuntu's own docker-compose-v2 package joins the distro compose set; the
  # official Docker Debian conflict list does not carry it.
  [[ "${OS_ID}" != ubuntu ]] || compose_packages+=(docker-compose-v2)
  # Official Docker ${OS_ID} conflicts that always break the lifecycle contract.
  for package in docker.io docker-doc docker-buildx podman-docker containerd runc; do
    if package_installed "${package}"; then
      record_failure "conflicting package '${package}' is installed; remove it deliberately (this bootstrap never uninstalls an existing runtime); see ${DOCKER_INSTALL_DOC}"
    fi
  done
  # The distribution's own compose packages only conflict while the Compose v2
  # plugin this lifecycle requires is unavailable.
  if ! compose_plugin_available; then
    for package in "${compose_packages[@]}"; do
      if package_installed "${package}"; then
        record_failure "conflicting package '${package}' is installed while the Docker Compose v2 plugin is unavailable; align the existing installation deliberately (this bootstrap never uninstalls an existing runtime)"
      fi
    done
  fi
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
  echo "configuring the Docker official apt repository for ${OS_ID} ${OS_VERSION_ID}"
  install -m 0755 -d "${KEYRING_DIR}"
  curl -fsSL "https://download.docker.com/linux/${OS_ID}/gpg" -o "${KEYRING_DIR}/docker.asc"
  chmod a+r "${KEYRING_DIR}/docker.asc"
  expected="$(printf 'deb [arch=%s signed-by=%s/docker.asc] https://download.docker.com/linux/%s %s stable' \
    "${ARCH_WORD}" "${KEYRING_DIR}" "${OS_ID}" "${DOCKER_REPO_CODENAME}")"
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
  local info_output
  if ! docker_client_present; then
    if [[ "${SUPPORT_MODE}" == "existing-docker" ]]; then
      record_failure "Ubuntu 20.04 can run ocservia with an existing compatible Docker installation, but automatic Docker bootstrap is unavailable on this legacy host (Ubuntu 20.04 is outside Docker's current official Ubuntu support matrix; standard security maintenance ended 2025-05-31 and requires Ubuntu Pro/ESM or an equivalent maintenance strategy, which this bootstrap never configures)"
    else
      record_failure "Docker is required but not installed; run 'bootstrap-host.sh install' to install it from the official Docker apt repository"
    fi
    return
  fi
  if ! docker_daemon_active; then
    info_output="$(docker info 2>&1 || true)"
    if grep -q 'permission denied' <<<"${info_output}"; then
      record_failure "the invoking user cannot access the Docker daemon; either run the Controller lifecycle as root, or deliberately grant this user Docker access per Docker's official post-install steps — this bootstrap never modifies the Docker permission model or group membership"
    else
      record_failure "Docker daemon is not active; start it with 'sudo systemctl enable --now docker'"
    fi
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

verify_launcher_docker_access() {
  [[ "${launcher_user}" != root ]] || return 0
  if (( EUID != 0 )); then
    # The invoking user is the launcher; check_docker already probed daemon
    # access directly and classified permission failures.
    return 0
  fi
  command -v runuser >/dev/null 2>&1 || {
    record_failure "runuser (util-linux) is required to verify Docker access for the launcher user ${launcher_user}"
    return 0
  }
  if runuser -u "${launcher_user}" -- env PATH="${PATH}" docker info >/dev/null 2>&1; then
    echo "launcher access: ${launcher_user} can reach the Docker daemon"
  else
    record_failure "launcher user ${launcher_user} cannot access the Docker daemon; either deliberately grant it Docker access per Docker's official post-install steps, or run this bootstrap and the Controller lifecycle as root instead of via sudo — this bootstrap never modifies the Docker permission model or group membership"
  fi
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

# The state root contract mirrors controller.sh prepare_state_root exactly so
# a bootstrap pass can never be followed by a lifecycle ownership failure.
state_root_absolute_canonical() {
  local resolved
  if [[ "${STATE_ROOT}" != /* ]]; then
    record_failure "state root ${STATE_ROOT} must be an absolute path"
    return 1
  fi
  case "${STATE_ROOT}" in
    /|*/|*/../*|*/..|*/./*|*/.)
      record_failure "state root ${STATE_ROOT} must be a canonical path without traversal"
      return 1 ;;
  esac
  if ! resolved="$(realpath -e -- "${STATE_ROOT}" 2>/dev/null)"; then
    record_failure "state root ${STATE_ROOT} does not exist"
    return 1
  fi
  if [[ "${resolved}" != "${STATE_ROOT}" ]]; then
    record_failure "state root ${STATE_ROOT} must not contain symlink ancestry"
    return 1
  fi
  return 0
}

validate_lifecycle_ancestry() {
  local ancestor="$1" stat_line ancestor_uid ancestor_mode
  while true; do
    if [[ "$(realpath -e -- "${ancestor}" 2>/dev/null)" != "${ancestor}" ]]; then
      record_failure "state root ancestry must not contain symlinks: ${ancestor}"
      return 1
    fi
    stat_line="$(stat -c '%u:%a' "${ancestor}")"
    ancestor_uid="${stat_line%%:*}"
    ancestor_mode="${stat_line##*:}"
    if [[ "${ancestor_uid}" != 0 && "${ancestor_uid}" != "${launcher_uid}" ]]; then
      record_failure "state root ancestry must be root- or launcher-owned: ${ancestor} (uid ${ancestor_uid})"
      return 1
    fi
    if (( (8#${ancestor_mode} & 8#022) != 0 )); then
      record_failure "state root ancestry must not be group/world writable: ${ancestor} (mode ${ancestor_mode})"
      return 1
    fi
    [[ "${ancestor}" == "/" ]] && break
    ancestor="$(dirname -- "${ancestor}")"
  done
  return 0
}

state_root_ownership() {
  local stat_line uid mode
  stat_line="$(stat -c '%u:%a' "${STATE_ROOT}")"
  uid="${stat_line%%:*}"
  mode="${stat_line##*:}"
  if [[ "${uid}" != "${launcher_uid}" || "${mode}" != 700 ]]; then
    record_failure "state root ${STATE_ROOT} must be owned by the launcher user (uid ${launcher_uid}) with mode 0700 (found uid ${uid}, mode ${mode}), exactly as controller.sh requires"
    return 1
  fi
  return 0
}

check_state_root() {
  # The path contract applies whether or not the directory exists yet.
  if [[ "${STATE_ROOT}" != /* ]]; then
    record_failure "state root ${STATE_ROOT} must be an absolute path"
    return
  fi
  case "${STATE_ROOT}" in
    /|*/|*/../*|*/..|*/./*|*/.)
      record_failure "state root ${STATE_ROOT} must be a canonical path without traversal"
      return ;;
  esac
  if [[ ! -e "${STATE_ROOT}" && ! -L "${STATE_ROOT}" ]]; then
    record_pending "lifecycle state root ${STATE_ROOT} is not provisioned yet; 'bootstrap-host.sh install' (or controller.sh) creates it"
    return
  fi
  if [[ -L "${STATE_ROOT}" || ! -d "${STATE_ROOT}" ]]; then
    record_failure "state root ${STATE_ROOT} must be a real directory and not a symlink"
    return
  fi
  state_root_absolute_canonical || return
  validate_lifecycle_ancestry "${STATE_ROOT}" || return
  if state_root_ownership; then
    echo "state root validated: ${STATE_ROOT} (launcher-owned, mode 0700)"
  fi
}

ensure_state_root() {
  local parent
  if [[ -L "${STATE_ROOT}" ]]; then
    fail "state root ${STATE_ROOT} must not be a symlink"
  fi
  if [[ -e "${STATE_ROOT}" ]]; then
    [[ -d "${STATE_ROOT}" ]] || fail "state root ${STATE_ROOT} must be a directory"
    state_root_absolute_canonical || return 1
    validate_lifecycle_ancestry "${STATE_ROOT}" || return 1
    state_root_ownership || return 1
    echo "state root validated: ${STATE_ROOT} (launcher-owned, mode 0700)"
    return
  fi
  [[ "${STATE_ROOT}" == /* ]] || fail "state root ${STATE_ROOT} must be an absolute path"
  case "${STATE_ROOT}" in
    /|*/|*/../*|*/..|*/./*|*/.) fail "state root ${STATE_ROOT} must be a canonical path without traversal" ;;
  esac
  parent="$(dirname -- "${STATE_ROOT}")"
  [[ -d "${parent}" ]] || fail "state root parent ${parent} does not exist; create it first"
  [[ "$(realpath -e -- "${parent}")" == "${parent}" ]] ||
    fail "state root parent ${parent} must not contain symlink ancestry"
  validate_lifecycle_ancestry "${parent}" || return 1
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
    if [[ "${secret_uid}" != "${launcher_uid}" || "${secret_mode}" != 700 ]]; then
      record_pending "OCSERV_SECRET_DIR (${secret_dir}) must be launcher-owned (uid ${launcher_uid}) with mode 0700, matching the compose.sh contract (found ${secret_stat})"
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
  verify_launcher_docker_access
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
  elif [[ "${SUPPORT_MODE}" == "existing-docker" ]]; then
    fail "Ubuntu 20.04 can run ocservia with an existing compatible Docker installation, but automatic Docker bootstrap is unavailable on this legacy host (Ubuntu 20.04 is outside Docker's current official Ubuntu support matrix; standard security maintenance ended 2025-05-31 and requires Ubuntu Pro/ESM or an equivalent maintenance strategy, which this bootstrap never configures)"
  else
    echo "Docker is absent; installing Docker Engine and the Compose plugin from the official Docker apt repository"
    install_missing_base_tools
    install_docker_engine
  fi
  revive_docker_daemon
  validate_docker_readiness
  verify_launcher_docker_access
  if ((${#failures[@]} > 0)); then
    echo "controller host bootstrap: refusing to create lifecycle directories while host prerequisite failures remain" >&2
    exit 1
  fi

  ensure_state_root ||
    {
      echo "controller host bootstrap: the state root does not satisfy the controller.sh contract" >&2
      exit 1
    }
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
