#!/usr/bin/env bash
# Install a pinned native package and enroll a managed node.
# --join-token accepts the Controller's root-only installation token at a hidden
# prompt. --join-token-file accepts a protected file for unattended installation.
# The token authenticates HTTPS configuration retrieval, derives a separate
# Agent-only enrollment token, and binds root privd registration to that node.
# Services start automatically only after the exact privd key is registered;
# success requires Controller-observed online state and fresh telemetry.
# --resume-install reuses /var/lib/ocservia-bootstrap and the existing identity.
# Failed or ambiguous responses never regenerate identities or rotate keys.
#
# Hidden input, --join-token-file and --resume-install differ only in the
# credential source. Configuration and replay authority come from the Controller.
# Native packages preserve identity and keys; local process state alone never
# settles installation. Manual enrollment remains an Agent CLI recovery duty.
#
# Supported hosts, mirroring what this repository's installers and CI actually
# exercise: x86_64 and aarch64; Ubuntu 22.04/24.04/26.04 and Debian 12/13
# through dpkg; Rocky Linux 9 through rpm; systemd required. Ubuntu 20.04 and
# Debian 11 remain outside the verified native runtime baseline. Any other
# platform fails closed before any host mutation.
#
# Launcher contract (mirrors deploy/production/install.sh): run as the
# operator launcher user, not as a whole-script sudo invocation. Privileged
# steps cross a scoped sudo boundary one command at a time with explicit
# arguments only, so the operator environment is never handed to root as a
# whole. Secrets cross only as protected root-owned file paths, and token or
# key material is never printed. A deliberate whole-lifecycle-as-root run is
# available through 'install.sh --root-lifecycle'.
#
# OCSERV_MANAGED_NODE_OS_RELEASE and OCSERV_MANAGED_NODE_SYSROOT are fixture
# seams for scripts/test-managed-node-install.sh (the os-release source and a
# staging prefix for the fixed node paths); they are not operator
# configuration.
set -euo pipefail
umask 077
readonly DEFAULT_VERSION=""
SCRIPT_LIB="$(dirname "${BASH_SOURCE[0]}")/../lib"
# shellcheck source=deploy/lib/node-bootstrap.sh
source "${SCRIPT_LIB}/node-bootstrap.sh" # release-embed
NON_INTERACTIVE=false
TOKEN_SOURCE_SEEN=false
JOIN_TOKEN_FILE=""
JOIN_STATE_DIR=""
JOIN_ORIGIN=""

# Re-execute the same installer across the sudo boundary.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
INSTALLER_PATH="${SCRIPT_DIR}/$(basename "${BASH_SOURCE[0]}")"
DOWNLOAD_BASE="https://github.com/GentleKingson/ocservia/releases/download"
OS_RELEASE_FILE="${OCSERV_MANAGED_NODE_OS_RELEASE:-/etc/os-release}"
SYSROOT="${OCSERV_MANAGED_NODE_SYSROOT:-}"
AGENT_BINARY="${SYSROOT}/usr/libexec/ocservia/ocservia-agent"
PRIVD_BINARY="${SYSROOT}/usr/libexec/ocservia/ocservia-privd"
RELAY_DROPIN="${SYSROOT}/usr/lib/systemd/system/ocservia-agent.service.d/10-production-relays.conf"
REQUEST_MARKER="${SYSROOT}/etc/ocservia/agent-install-production-relays"
AGENT_CONF_DIR="${SYSROOT}/etc/ocservia-agent"
AGENT_ENV_FILE="${AGENT_CONF_DIR}/agent.env"
RELAYS_ENV_FILE="${AGENT_CONF_DIR}/relays.env"
RELAY_TOKEN_FILE="${AGENT_CONF_DIR}/relay-access-token"
RELAY_CA_FILE="${AGENT_CONF_DIR}/relay-ca.pem"
COMMAND_KEY_FILE="${AGENT_CONF_DIR}/controller-command-verification-key.pem"
ENROLLMENT_TOKEN_FILE="${AGENT_CONF_DIR}/enrollment-token"
USER_SEAL_KEY_FILE="${AGENT_CONF_DIR}/user-password-seal-private.pem"
P12_SEAL_KEY_FILE="${AGENT_CONF_DIR}/p12-password-seal-private.pem"
IDENTITY_DIR="${SYSROOT}/var/lib/ocservia-agent/identity"
# The pristine placeholder relay URL installed by the verified package from
# deploy/production/systemd/relays.env.example.
PLACEHOLDER_RELAY_URL_A="https://relay-a.example.com"
PLACEHOLDER_NODE_ID="00000000-0000-7000-8000-000000000000"
SUPPORTED_HOSTS="Ubuntu 22.04/24.04/26.04 and Debian 12/13 (dpkg), Rocky Linux 9 (rpm), x86_64/aarch64, systemd"
VERSION="${DEFAULT_VERSION}"
ARCH_WORD=""
RPM_ARCH=""
PACKAGE_FAMILY=""
PACKAGE_MANAGER=""
PACKAGE_FILE=""
STAGING_DIR=""
PACKAGE_STAGING_DIR=""

EXPECTED_PACKAGE_DIGEST=""
AGENT_GID=""
ENDPOINT_ID=""
ENROLLED_NODE_ID=""
USER_SEAL_DESCRIPTOR=""
P12_SEAL_DESCRIPTOR=""

# Only fixture path seams and public sealing key identifiers cross sudo.
# Controller configuration and secret paths are obtained after elevation.
ROOT_LIFECYCLE_ENV_NAMES=(
  OCSERV_MANAGED_NODE_OS_RELEASE
  OCSERV_MANAGED_NODE_SYSROOT
  USER_PASSWORD_SEAL_KEY_ID
  P12_PASSWORD_SEAL_KEY_ID
)

fail() {
  echo "managed-node install: $1" >&2
  exit 1
}

usage() {
  echo "No arguments: hidden installation-token prompt (TTY required). --non-interactive: unattended, no prompts." >&2
  echo "Options: --join-token (hidden prompt), --join-token-file PATH, --resume-install, --version vX.Y.Z." >&2
  echo "usage: deploy/managed-node/install.sh [--version vX.Y.Z] [--root-lifecycle] [--join-token | --join-token-file PATH | --resume-install]" >&2
  exit 2
}

# Release assets embed their tag. Automation can supply an exact tag explicitly.
version_seen=false
while (($# > 0)); do
  case "$1" in
    --join-token|--interactive)
      [[ "${TOKEN_SOURCE_SEEN}" == false ]] || fail "choose one installation token source"
      TOKEN_SOURCE_SEEN=true; shift ;;
    --join-token-file)
      [[ "${TOKEN_SOURCE_SEEN}" == false ]] || fail "choose one installation token source"
      (($# >= 2)) || fail "--join-token-file requires a protected file"
      [[ -f "$2" && ! -L "$2" ]] || fail "--join-token-file requires a regular file, not a symlink"
      TOKEN_SOURCE_SEEN=true; JOIN_TOKEN_FILE="$(realpath -- "$2")"; shift 2 ;;
    --resume-install)
      [[ "${TOKEN_SOURCE_SEEN}" == false ]] || fail "choose one installation token source"
      TOKEN_SOURCE_SEEN=true; JOIN_TOKEN_FILE=resume; shift ;;
    --non-interactive) NON_INTERACTIVE=true; shift ;;
    --config)
      fail "manual configuration installation is retired; use an installation token or --resume-install; manual recovery uses the Agent enrollment CLI" ;;
    --help|-h) usage ;;
    --version)
      [[ "${version_seen}" == false && $# -ge 2 ]] || usage
      VERSION="$2"
      version_seen=true
      shift 2
      ;;
    --root-lifecycle)
      shift
      ;;
    *)
      usage
      ;;
  esac
done
[[ "${NON_INTERACTIVE}" == false || -n "${JOIN_TOKEN_FILE}" ]] || fail "unattended token install requires --join-token-file or --resume-install"
if [[ -z "${JOIN_TOKEN_FILE}" ]]; then
  [[ -t 0 && -t 1 ]] || fail "installation token input requires a TTY; use --join-token-file or --resume-install"
fi
[[ -n "${VERSION}" ]] || fail "use the pinned Release installer or supply --version vX.Y.Z"
[[ "${VERSION}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-rc[.][1-9][0-9]*)?$ ]] ||
  fail "unsupported version '${VERSION}': an exact vX.Y.Z or vX.Y.Z-rc.N release tag is required (latest, branches, commits, and other pre-releases are not accepted)"
RELEASE_VERSION="${VERSION#v}"

USER_PASSWORD_SEAL_KEY_ID="${USER_PASSWORD_SEAL_KEY_ID:-}"
P12_PASSWORD_SEAL_KEY_ID="${P12_PASSWORD_SEAL_KEY_ID:-}"

# Privileged steps run one command at a time: directly when this process is
# already the deliberate root lifecycle, otherwise through sudo with explicit
# arguments only. The node configuration directory is mode 0750
# root:ocserv-agent, so existence probes and metadata reads below the
# privileged boundary must also cross it.
priv() {
  if ((EUID == 0)); then
    "$@"
  else
    if [[ "${NON_INTERACTIVE}" == true ]]; then sudo -n "$@"; else sudo "$@"; fi
  fi
}

# The Agent binary refuses to run as root, so the offline identity
# preparation and the one-shot enrollment always run as ocserv-agent.
as_agent() {
  if ((EUID == 0)); then
    runuser -u ocserv-agent -- "$@"
  else
    if [[ "${NON_INTERACTIVE}" == true ]]; then sudo -n -u ocserv-agent "$@"; else sudo -u ocserv-agent "$@"; fi
  fi
}

path_exists() {
  priv test -e "$1" || priv test -L "$1"
}

stat_string() {
  priv stat -c '%u:%g:%a:%h' -- "$1"
}

forward_root_lifecycle() {
  local variable allowed
  local -a forwarded_environment=()
  local -a reexec_arguments=(--root-lifecycle)
  case "${JOIN_TOKEN_FILE}" in
    resume) reexec_arguments+=(--resume-install) ;;
    "") reexec_arguments+=(--join-token) ;;
    *) reexec_arguments+=(--join-token-file "${JOIN_TOKEN_FILE}") ;;
  esac
  [[ "${NON_INTERACTIVE}" == false ]] || reexec_arguments+=(--non-interactive)
  # The --version pin is operator-chosen public release identity, not node
  # configuration: it re-crosses the privilege boundary as the same explicit
  # command-line argument instead of an environment variable.
  reexec_arguments+=(--version "${VERSION}")
  while IFS= read -r variable; do
    for allowed in "${ROOT_LIFECYCLE_ENV_NAMES[@]}"; do
      [[ "${variable}" == "${allowed}" ]] || continue
      forwarded_environment+=("${variable}=${!variable}")
      break
    done
  done < <(compgen -e)
  command -v sudo >/dev/null 2>&1 ||
    fail "sudo is required for --root-lifecycle when the installer is not already running as root"
  # Re-execute this exact release asset without passing the operator environment.
  local -a sudo_args=()
  [[ "${NON_INTERACTIVE}" == false ]] || sudo_args+=(-n)
  exec sudo "${sudo_args[@]}" env "${forwarded_environment[@]+"${forwarded_environment[@]}"}" \
    bash "${INSTALLER_PATH}" "${reexec_arguments[@]}"
}

resolve_architecture() {
  case "$(uname -m)" in
    x86_64) ARCH_WORD=amd64; RPM_ARCH=x86_64 ;;
    aarch64) ARCH_WORD=arm64; RPM_ARCH=aarch64 ;;
    *) fail "unsupported architecture '$(uname -m)'; managed-node releases support linux/amd64 and linux/arm64" ;;
  esac
}

detect_platform() {
  local os_id os_version_id
  [[ -r "${OS_RELEASE_FILE}" ]] ||
    fail "cannot read the OS release file ${OS_RELEASE_FILE}; supported hosts are ${SUPPORTED_HOSTS}"
  # shellcheck disable=SC1090
  . "${OS_RELEASE_FILE}"
  # shellcheck disable=SC2154
  os_id="${ID:-}"
  # shellcheck disable=SC2154
  os_version_id="${VERSION_ID:-}"
  case "${os_id} ${os_version_id}" in
    "ubuntu 22.04" | "ubuntu 24.04" | "ubuntu 26.04" | "debian 12" | "debian 13")
      PACKAGE_FAMILY=deb
      PACKAGE_FILE="ocservia-agent_${RELEASE_VERSION}-1_${ARCH_WORD}.deb"
      ;;
    "rocky 9" | "rocky 9."*)
      PACKAGE_FAMILY=rpm
      PACKAGE_FILE="ocservia-agent-${RELEASE_VERSION}-1.${RPM_ARCH}.rpm"
      ;;
    *)
      fail "unsupported OS '${os_id:-unknown} ${os_version_id:-unknown}'; supported managed-node hosts are ${SUPPORTED_HOSTS}"
      ;;
  esac
  command -v systemctl >/dev/null 2>&1 ||
    fail "systemd is required on a managed node"
  case "${PACKAGE_FAMILY}" in
    deb) PACKAGE_MANAGER=dpkg ;;
    rpm) PACKAGE_MANAGER=rpm ;;
  esac
  command -v "${PACKAGE_MANAGER}" >/dev/null 2>&1 ||
    fail "${PACKAGE_MANAGER} is required to install the native package"
  echo "platform: ${os_id} ${os_version_id} (${PACKAGE_FAMILY}, ${ARCH_WORD})"
}

validate_sysroot() {
  if [[ -n "${SYSROOT}" ]]; then
    case "${SYSROOT}" in
      /*) ;;
      *) fail "OCSERV_MANAGED_NODE_SYSROOT must be an absolute staging prefix" ;;
    esac
    case "${SYSROOT}" in
      / | */ | *//* | */./* | */../* | */..)
        fail "OCSERV_MANAGED_NODE_SYSROOT must be a canonical path without traversal"
        ;;
    esac
  fi
}

require_commands() {
  local tool
  for tool in curl openssl sha256sum awk; do
    command -v "${tool}" >/dev/null 2>&1 ||
      fail "${tool} is required by the managed-node installer"
  done
}

validate_operator_inputs() {
  local variable
  for variable in CONTROLLER_ENDPOINT_ID RELAY_URL_A \
    RELAY_ACCESS_TOKEN_SOURCE CONTROLLER_COMMAND_VERIFICATION_KEY_SOURCE; do
    [[ -n "${!variable:-}" ]] ||
      fail "${variable} is not set; export the managed-node configuration before running the installer"
  done
  [[ "${CONTROLLER_ENDPOINT_ID}" =~ ^[0-9a-f]{64}$ ]] ||
    fail "CONTROLLER_ENDPOINT_ID must be the 64-lowercase-hex Controller EndpointID"
  # shellcheck disable=SC2153 # A was required through the allowlisted loop above.
  case "${RELAY_URL_A}" in
    https://*) ;;
    *) fail "RELAY_URL_A must be an https:// dedicated relay URL" ;;
  esac
  # This value is written literally to a systemd EnvironmentFile.
  # Reject characters that cannot round-trip; the binary validates URLs.
  if [[ "${RELAY_URL_A}" =~ [[:space:]\"\'\`\$\\\?#@] ]]; then
    fail "RELAY_URL_A must be literal credential-free HTTPS without query or fragment"
  fi
  USER_PASSWORD_SEAL_KEY_ID="${USER_PASSWORD_SEAL_KEY_ID:-user-password-v1}"
  P12_PASSWORD_SEAL_KEY_ID="${P12_PASSWORD_SEAL_KEY_ID:-p12-password-v1}"
  for variable in USER_PASSWORD_SEAL_KEY_ID P12_PASSWORD_SEAL_KEY_ID; do
    [[ "${!variable}" =~ ^[A-Za-z0-9._-]{1,128}$ ]] ||
      fail "${variable} must match the Agent sealing key ID charset [A-Za-z0-9._-]"
  done
  [[ "${USER_PASSWORD_SEAL_KEY_ID}" != "${P12_PASSWORD_SEAL_KEY_ID}" ]] ||
    fail "the two password sealing key IDs must be distinct"
  ENROLLMENT_ENVIRONMENT="${ENROLLMENT_ENVIRONMENT:-production}"
  [[ "${ENROLLMENT_ENVIRONMENT}" =~ ^[^[:space:]]{1,64}$ ]] ||
    fail "ENROLLMENT_ENVIRONMENT must be 1-64 characters without whitespace"
  for variable in RELAY_ACCESS_TOKEN_SOURCE CONTROLLER_COMMAND_VERIFICATION_KEY_SOURCE; do
    [[ -f "${!variable}" && ! -L "${!variable}" && -s "${!variable}" ]] ||
      fail "${variable} must be a non-empty regular file (not a symlink) provisioned through a protected channel"
  done
  openssl pkey -pubin -in "${CONTROLLER_COMMAND_VERIFICATION_KEY_SOURCE}" >/dev/null 2>&1 ||
    fail "CONTROLLER_COMMAND_VERIFICATION_KEY_SOURCE is not a readable public key PEM"
}

validate_bootstrap_token_source() {
  local bootstrap_mode
  [[ -n "${BOOTSTRAP_TOKEN_SOURCE}" ]] || return 0
  [[ -f "${BOOTSTRAP_TOKEN_SOURCE}" && ! -L "${BOOTSTRAP_TOKEN_SOURCE}" && -s "${BOOTSTRAP_TOKEN_SOURCE}" ]] ||
    fail "BOOTSTRAP_TOKEN_SOURCE must be a non-empty regular file (not a symlink) provisioned through a protected channel"
  bootstrap_mode="$(stat -c '%a' -- "${BOOTSTRAP_TOKEN_SOURCE}")" ||
    fail "cannot inspect BOOTSTRAP_TOKEN_SOURCE permissions"
  (( (8#${bootstrap_mode} & 8#077) == 0 )) ||
    fail "BOOTSTRAP_TOKEN_SOURCE must not be accessible by group or other users (found mode ${bootstrap_mode})"
}

create_launcher_staging() {
  STAGING_DIR="$(mktemp -d "${TMPDIR:-/tmp}/ocservia-managed-node.XXXXXX")"
  trap '[[ -z "${PACKAGE_STAGING_DIR:-}" ]] || priv rm -rf -- "${PACKAGE_STAGING_DIR}"; [[ -z "${STAGING_DIR:-}" ]] || rm -rf -- "${STAGING_DIR}"' EXIT INT TERM
}

create_package_staging() {
  # A fixed root-owned sticky parent prevents the launcher from replacing
  # the protected staging directory through an operator-controlled TMPDIR.
  PACKAGE_STAGING_DIR="$(priv mktemp -d /var/tmp/ocservia-managed-node-pkg.XXXXXX)"
}

download_release_artifacts() {
  local name
  for name in "$@"; do
    echo "downloading ${DOWNLOAD_BASE}/${VERSION}/${name}"
    curl -fsSL --proto '=https' --tlsv1.2 \
      --output "${STAGING_DIR}/${name}" "${DOWNLOAD_BASE}/${VERSION}/${name}"
  done
}

download_native_package() {
  local input actual_digest
  download_release_artifacts "${PACKAGE_FILE}"
  input="${STAGING_DIR}/${PACKAGE_FILE}"
  [[ -f "${input}" && ! -L "${input}" && -s "${input}" ]] ||
    fail "downloaded package must be a nonempty regular file"
  EXPECTED_PACKAGE_DIGEST="$(sha256sum -- "${input}" | awk '{print $1}')"
  priv install -o root -g root -m 0644 -- "${input}" "${PACKAGE_STAGING_DIR}/${PACKAGE_FILE}"
  actual_digest="$(priv sha256sum -- "${PACKAGE_STAGING_DIR}/${PACKAGE_FILE}" | awk '{print $1}')"
  [[ "${actual_digest}" == "${EXPECTED_PACKAGE_DIGEST}" ]] ||
    fail "the package changed while entering root-owned staging"
  echo "HTTPS package staged: ${PACKAGE_FILE}"
}

installed_package_version() {
  local status
  case "${PACKAGE_FAMILY}" in
    deb)
      status="$(dpkg-query -W -f='${Status}' ocservia-agent 2>/dev/null || true)"
      [[ "${status}" == "install ok installed" ]] || return 0
      dpkg-query -W -f='${Version}' ocservia-agent 2>/dev/null || true
      ;;
    rpm)
      rpm -q --qf '%{VERSION}' ocservia-agent 2>/dev/null || true
      ;;
  esac
}

expected_installed_version() {
  case "${PACKAGE_FAMILY}" in
    # nFPM encodes the supported RC suffix with ~; RPM stores Release separately.
    deb) echo "${RELEASE_VERSION/-rc./~rc.}-1" ;;
    rpm) echo "${RELEASE_VERSION/-rc./~rc.}" ;;
  esac
}

native_package_satisfied() {
  # A converged rerun must not re-download or reinstall the native package.
  # A satisfied package skips downloads and package manager mutation.
  # Installed version and production relay checks still fail closed.
  local installed_version
  installed_version="$(installed_package_version)"
  [[ -n "${installed_version}" ]] || return 1
  [[ "${installed_version}" == "$(expected_installed_version)" ]] ||
    fail "ocservia-agent ${installed_version} is installed but this bootstrap targets release ${RELEASE_VERSION} (native ${PACKAGE_FAMILY} version $(expected_installed_version)); the bootstrap neither upgrades nor downgrades an installed package — use the Agent release lifecycle (docs/operations/agent-lifecycle.md)"
  [[ -f "${RELAY_DROPIN}" && ! -L "${RELAY_DROPIN}" ]] ||
    fail "the installed ocservia-agent ${installed_version} lacks the production relay drop-in ${RELAY_DROPIN}; it was installed without the production request — reinstall it through the release lifecycle with /etc/ocservia/agent-install-production-relays present"
  return 0
}

install_native_package() {
  local actual_digest
  # Re-check the digest on the frozen root-owned copy immediately before the
  # package manager, so the exact bytes about to be installed are still the
  # downloaded HTTPS bytes; every read since the freeze
  # step targets the root-owned staging the launcher cannot reach.
  actual_digest="$(priv sha256sum -- "${PACKAGE_STAGING_DIR}/${PACKAGE_FILE}" | awk '{print $1}')"
  [[ "${actual_digest}" == "${EXPECTED_PACKAGE_DIGEST}" ]] ||
    fail "the package digest changed after verification; refusing to invoke the package manager"
  priv install -d -o root -g root -m 0755 -- "$(dirname -- "${REQUEST_MARKER}")"
  priv touch -- "${REQUEST_MARKER}"
  case "${PACKAGE_FAMILY}" in
    deb) priv dpkg -i "${PACKAGE_STAGING_DIR}/${PACKAGE_FILE}" ;;
    rpm) priv rpm -ivh "${PACKAGE_STAGING_DIR}/${PACKAGE_FILE}" ;;
  esac
  [[ -f "${RELAY_DROPIN}" && ! -L "${RELAY_DROPIN}" ]] ||
    fail "the native package install did not produce the production relay drop-in ${RELAY_DROPIN}"
  [[ -x "${AGENT_BINARY}" ]] ||
    fail "the native package install did not install ${AGENT_BINARY}"
  echo "native package installed: ${PACKAGE_FILE} (production relay contract requested)"
}

resolve_agent_group() {
  AGENT_GID="$(priv id -g ocserv-agent 2>/dev/null)" ||
    fail "the ocserv-agent group does not exist; the native package lifecycle must create it"
  [[ "${AGENT_GID}" =~ ^[0-9]+$ ]] ||
    fail "cannot resolve the ocserv-agent group id"
}

ensure_sealing_key() {
  local path="$1" purpose="$2" metadata
  if path_exists "${path}"; then
    metadata="$(stat_string "${path}")"
    if ! priv test -f "${path}" || priv test -L "${path}"; then
      fail "the existing ${purpose} sealing key ${path} is not a regular file; refusing to overwrite or reuse it"
    fi
    [[ "${metadata}" == "0:0:600:1" ]] ||
      fail "the existing ${purpose} sealing key ${path} has unsafe metadata (${metadata}); expected root:root mode 0600 with a single link — resolve it deliberately, the installer never overwrites sealing material"
    echo "preserved existing ${purpose} sealing key"
    return
  fi
  priv install -o root -g root -m 0600 -- /dev/null "${path}"
  priv openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "${path}"
  echo "generated ${purpose} sealing key (${path})"
}

sealing_descriptor() {
  priv openssl rsa -in "$1" -pubout -outform DER 2>/dev/null | sha256sum | awk '{print $1}'
}

write_config_atomic() {
  local source="$1" target="$2" mode="$3"
  priv install -o root -g ocserv-agent -m "${mode}" -- "${source}" "${target}.new"
  priv mv -f -- "${target}.new" "${target}"
}

ensure_relays_env() {
  local metadata existing_a staging
  if path_exists "${RELAYS_ENV_FILE}"; then
    metadata="$(stat_string "${RELAYS_ENV_FILE}")"
    if ! priv test -f "${RELAYS_ENV_FILE}" || priv test -L "${RELAYS_ENV_FILE}"; then
      fail "the existing ${RELAYS_ENV_FILE} is not a regular file"
    fi
    [[ "${metadata}" == "0:${AGENT_GID}:640:1" ]] ||
      fail "the existing ${RELAYS_ENV_FILE} has unsafe metadata (${metadata}); expected root:ocserv-agent mode 0640"
    existing_a="$(priv sed -n 's/^RELAY_URL_A=//p' "${RELAYS_ENV_FILE}" | tail -n 1)"
    if [[ "${existing_a}" == "${RELAY_URL_A}" ]]; then
      echo "preserved existing relay configuration (${RELAYS_ENV_FILE})"
      return
    fi
    [[ "${existing_a}" == "${PLACEHOLDER_RELAY_URL_A}" ]] ||
      fail "the existing ${RELAYS_ENV_FILE} requests ${existing_a}, not the configured dedicated relay; resolve the mismatch deliberately instead of overwriting node configuration"
  fi
  staging="$(mktemp "${STAGING_DIR}/relays-env.XXXXXX")"
  printf 'RELAY_URL_A=%s\n' "${RELAY_URL_A}" >"${staging}"
  write_config_atomic "${staging}" "${RELAYS_ENV_FILE}" 0640
  echo "wrote dedicated relay URL to ${RELAYS_ENV_FILE}"
}

ensure_protected_install() {
  local source="$1" target="$2" purpose="$3" metadata
  if path_exists "${target}"; then
    metadata="$(stat_string "${target}")"
    if ! priv test -f "${target}" || priv test -L "${target}"; then
      fail "the existing ${purpose} ${target} is not a regular file"
    fi
    [[ "${metadata}" == "0:${AGENT_GID}:640:1" || "${metadata}" == "0:${AGENT_GID}:440:1" ]] ||
      fail "the existing ${purpose} ${target} has unsafe metadata (${metadata}); expected root:ocserv-agent mode 0640 or 0440"
    echo "preserved existing ${purpose} (${target})"
    return
  fi
  priv install -o root -g ocserv-agent -m 0640 -- "${source}" "${target}"
  echo "installed ${purpose} (${target})"
}

prepare_bootstrap_token() {
  local metadata source_digest target_digest
  [[ -z "${ENROLLED_NODE_ID}" ]] || return 0
  [[ -n "${BOOTSTRAP_TOKEN_SOURCE}" ]] || return 0
  if path_exists "${ENROLLMENT_TOKEN_FILE}"; then
    metadata="$(stat_string "${ENROLLMENT_TOKEN_FILE}")"
    if ! priv test -f "${ENROLLMENT_TOKEN_FILE}" || priv test -L "${ENROLLMENT_TOKEN_FILE}"; then
      fail "the existing bootstrap token ${ENROLLMENT_TOKEN_FILE} is not a regular file"
    fi
    [[ "${metadata}" == "0:${AGENT_GID}:640:1" ]] ||
      fail "the existing bootstrap token ${ENROLLMENT_TOKEN_FILE} must be root:ocserv-agent mode 0640 (found ${metadata})"
    source_digest="$(priv sha256sum -- "${BOOTSTRAP_TOKEN_SOURCE}" | awk '{print $1}')" ||
      fail "cannot hash the protected bootstrap token source"
    target_digest="$(priv sha256sum -- "${ENROLLMENT_TOKEN_FILE}" | awk '{print $1}')" ||
      fail "cannot hash the already staged bootstrap token"
    [[ "${source_digest}" == "${target_digest}" ]] ||
      fail "the protected bootstrap token source does not match the already staged token; resolve the conflicting secret deliberately"
    return
  fi
  priv install -o root -g ocserv-agent -m 0640 -- "${BOOTSTRAP_TOKEN_SOURCE}" "${ENROLLMENT_TOKEN_FILE}"
  echo "protected node bootstrap token staged for enrollment"
}

detect_enrolled_node() {
  # Establishes whether this node already completed enrollment, before any
  # step that could create node state. The enrolled state is a validation
  # boundary: from here on the bootstrap must not regenerate identity or
  # sealing material the Controller-side enrollment binding depends on.
  local metadata node_id
  ENROLLED_NODE_ID=""
  path_exists "${AGENT_ENV_FILE}" || return 0
  metadata="$(stat_string "${AGENT_ENV_FILE}")"
  if ! priv test -f "${AGENT_ENV_FILE}" || priv test -L "${AGENT_ENV_FILE}"; then
    fail "the existing ${AGENT_ENV_FILE} is not a regular file"
  fi
  [[ "${metadata}" == "0:${AGENT_GID}:640:1" ]] ||
    fail "the existing ${AGENT_ENV_FILE} has unsafe metadata (${metadata}); expected root:ocserv-agent mode 0640"
  node_id="$(priv sed -n 's/^NODE_ID=//p' "${AGENT_ENV_FILE}" | tail -n 1)"
  [[ -n "${node_id}" ]] ||
    fail "the existing ${AGENT_ENV_FILE} has no NODE_ID line; resolve it deliberately instead of overwriting node configuration"
  if is_final_node_id "${node_id}"; then
    ENROLLED_NODE_ID="${node_id}"
  fi
}

validate_enrolled_sealing_keys() {
  # The enrolled agent.env pins the exact sealing public-key fingerprints the
  # Controller bound at enrollment. On an enrolled node both private keys
  # must exist with safe metadata and re-derive exactly those fingerprints; a
  # regenerated key would no longer match the enrolled binding — the same
  # mismatch the upgrade preflight fails closed on — so the bootstrap never
  # generates replacements here.
  local path purpose metadata expected
  for path in "${USER_SEAL_KEY_FILE}" "${P12_SEAL_KEY_FILE}"; do
    case "${path}" in
      "${USER_SEAL_KEY_FILE}") purpose=user-password ;;
      *) purpose=p12-password ;;
    esac
    if ! path_exists "${path}"; then
      fail "the enrolled node is missing its ${purpose} sealing private key ${path}; regenerating it would no longer match the enrolled Controller binding — restore the key from protected backups or re-enroll deliberately (docs/operations/agent-lifecycle.md)"
    fi
    if ! priv test -f "${path}" || priv test -L "${path}"; then
      fail "the existing ${purpose} sealing key ${path} is not a regular file; refusing to reuse it"
    fi
    metadata="$(stat_string "${path}")"
    [[ "${metadata}" == "0:0:600:1" ]] ||
      fail "the enrolled ${purpose} sealing key ${path} has unsafe metadata (${metadata}); expected root:root mode 0600 with a single link — resolve it deliberately, the installer never overwrites sealing material"
  done
  USER_SEAL_DESCRIPTOR="$(sealing_descriptor "${USER_SEAL_KEY_FILE}")"
  P12_SEAL_DESCRIPTOR="$(sealing_descriptor "${P12_SEAL_KEY_FILE}")"
  [[ "${USER_SEAL_DESCRIPTOR}" =~ ^[0-9a-f]{64}$ && "${P12_SEAL_DESCRIPTOR}" =~ ^[0-9a-f]{64}$ ]] ||
    fail "an enrolled sealing key is not a usable RSA private key; resolve it deliberately instead of regenerating it"
  expected="$(priv sed -n 's/^USER_PASSWORD_SEAL_PUBLIC_KEY_SHA256=//p' "${AGENT_ENV_FILE}" | tail -n 1)"
  [[ "${expected}" =~ ^[0-9a-f]{64}$ ]] ||
    fail "the enrolled ${AGENT_ENV_FILE} has no valid USER_PASSWORD_SEAL_PUBLIC_KEY_SHA256 fingerprint; resolve it deliberately"
  [[ "${USER_SEAL_DESCRIPTOR}" == "${expected}" ]] ||
    fail "the user-password sealing key re-derives ${USER_SEAL_DESCRIPTOR} but the enrolled agent.env pins ${expected}; a regenerated or replaced key cannot silently take the enrolled binding — restore the enrolled key or re-enroll deliberately (docs/operations/agent-lifecycle.md)"
  expected="$(priv sed -n 's/^P12_PASSWORD_SEAL_PUBLIC_KEY_SHA256=//p' "${AGENT_ENV_FILE}" | tail -n 1)"
  [[ "${expected}" =~ ^[0-9a-f]{64}$ ]] ||
    fail "the enrolled ${AGENT_ENV_FILE} has no valid P12_PASSWORD_SEAL_PUBLIC_KEY_SHA256 fingerprint; resolve it deliberately"
  [[ "${P12_SEAL_DESCRIPTOR}" == "${expected}" ]] ||
    fail "the p12-password sealing key re-derives ${P12_SEAL_DESCRIPTOR} but the enrolled agent.env pins ${expected}; a regenerated or replaced key cannot silently take the enrolled binding — restore the enrolled key or re-enroll deliberately (docs/operations/agent-lifecycle.md)"
  # Replays use the identifiers bound with these keys, even when the launcher
  # no longer exports the first attempt's optional public key names.
  USER_PASSWORD_SEAL_KEY_ID="$(priv sed -n 's/^USER_PASSWORD_SEAL_KEY_ID=//p' "${AGENT_ENV_FILE}" | tail -n 1)"
  P12_PASSWORD_SEAL_KEY_ID="$(priv sed -n 's/^P12_PASSWORD_SEAL_KEY_ID=//p' "${AGENT_ENV_FILE}" | tail -n 1)"
  [[ "${USER_PASSWORD_SEAL_KEY_ID}" =~ ^[A-Za-z0-9._-]{1,128}$ &&
     "${P12_PASSWORD_SEAL_KEY_ID}" =~ ^[A-Za-z0-9._-]{1,128}$ &&
     "${USER_PASSWORD_SEAL_KEY_ID}" != "${P12_PASSWORD_SEAL_KEY_ID}" ]] ||
    fail "the enrolled sealing key identifiers are missing or invalid; restore the original configuration"

}

validate_enrolled_identity() {
  # The persistent identity is the Controller enrollment binding. On an
  # enrolled node missing identity files are a fail-closed integrity failure,
  # never something to provision again. With both files present, the Agent's
  # own identity loader validates the material instead: Identity::provision
  # regenerates nothing when the files exist — it loads and enforces the
  # exact startup rules (agent-owned owner-only directory and files, the
  # 32-byte endpoint secret key, the 64-hex controller pin, and that the pin
  # still matches the configured Controller EndpointID), rejecting partial
  # or substituted identity material. Structural validity alone cannot
  # detect a replaced-but-valid endpoint key, so the enrollment also
  # persists the enrolled Agent EndpointID in agent.env and the rerun fails
  # closed unless the loaded identity derives exactly that EndpointID — the
  # same key the Controller bound at enrollment.
  local file output endpoint_id enrolled_endpoint_id
  for file in "${IDENTITY_DIR}/endpoint.key" "${IDENTITY_DIR}/controller.endpoint"; do
    if ! path_exists "${file}" || ! priv test -f "${file}" || priv test -L "${file}"; then
      fail "the enrolled node is missing its persistent identity file ${file}; regenerating the identity would break the Controller enrollment binding — restore ${IDENTITY_DIR} from protected backups or re-enroll as a new node deliberately (docs/operations/agent-lifecycle.md#enroll-the-node)"
    fi
  done
  enrolled_endpoint_id="$(priv sed -n 's/^AGENT_ENDPOINT_ID=//p' "${AGENT_ENV_FILE}" | tail -n 1)"
  [[ "${enrolled_endpoint_id}" =~ ^[0-9a-f]{64}$ ]] ||
    fail "the enrolled ${AGENT_ENV_FILE} has no valid AGENT_ENDPOINT_ID enrollment binding; resolve it deliberately instead of trusting an unbound enrolled identity (docs/operations/agent-lifecycle.md#enroll-the-node)"
  output="$(as_agent "${AGENT_BINARY}" \
    --identity-dir "${IDENTITY_DIR}" \
    --controller "${CONTROLLER_ENDPOINT_ID}" \
    --prepare-enrollment)" ||
    fail "the enrolled persistent identity in ${IDENTITY_DIR} did not pass the Agent identity validation; the bootstrap never regenerates an enrolled identity — restore the identity material or re-enroll deliberately (docs/operations/agent-lifecycle.md#enroll-the-node)"
  endpoint_id="$(printf '%s\n' "${output}" | awk 'NF {last=$0} END {print last}')"
  [[ "${endpoint_id}" =~ ^[0-9a-f]{64}$ ]] ||
    fail "the Agent did not print a valid EndpointID while validating the enrolled identity"
  [[ "${endpoint_id}" == "${enrolled_endpoint_id}" ]] ||
    fail "the enrolled identity now presents EndpointID ${endpoint_id} but the enrolled ${AGENT_ENV_FILE} pins ${enrolled_endpoint_id}; a replaced endpoint secret key cannot silently take the Controller enrollment binding — restore the identity material or re-enroll as a new node deliberately (docs/operations/agent-lifecycle.md#enroll-the-node)"
  ENDPOINT_ID="${endpoint_id}"
  echo "enrolled persistent identity validated; EndpointID: ${endpoint_id}"
}

validate_enrolled_relays() {
  # On an enrolled node relays.env is the running relay configuration. It
  # must already exist, be safe, and name the configured dedicated relay;
  # a validation-only rerun never writes it.
  local metadata existing_a
  if ! path_exists "${RELAYS_ENV_FILE}"; then
    fail "the enrolled node is missing its relay configuration ${RELAYS_ENV_FILE}; restoring it is a deliberate recovery operation, not a bootstrap rerun (docs/operations/agent-lifecycle.md#install-a-managed-node)"
  fi
  if ! priv test -f "${RELAYS_ENV_FILE}" || priv test -L "${RELAYS_ENV_FILE}"; then
    fail "the enrolled relay configuration ${RELAYS_ENV_FILE} is not a regular file"
  fi
  metadata="$(stat_string "${RELAYS_ENV_FILE}")"
  [[ "${metadata}" == "0:${AGENT_GID}:640:1" ]] ||
    fail "the enrolled relay configuration ${RELAYS_ENV_FILE} has unsafe metadata (${metadata}); expected root:ocserv-agent mode 0640"
  existing_a="$(priv sed -n 's/^RELAY_URL_A=//p' "${RELAYS_ENV_FILE}" | tail -n 1)"
  [[ "${existing_a}" == "${RELAY_URL_A}" ]] ||
    fail "the enrolled ${RELAYS_ENV_FILE} names ${existing_a:-<none>}, not the configured dedicated relay; a rerun never rewrites the running relay configuration — resolve the mismatch deliberately"
}

validate_enrolled_relay_token() {
  # The relay access token authenticates the node to the production relays.
  # On an enrolled node it must already exist as non-empty protected
  # material; a validation-only rerun never reinstalls it from the operator
  # source.
  local metadata
  if ! path_exists "${RELAY_TOKEN_FILE}"; then
    fail "the enrolled node is missing its relay access token ${RELAY_TOKEN_FILE}; restoring it is a deliberate recovery operation, not a bootstrap rerun"
  fi
  if ! priv test -f "${RELAY_TOKEN_FILE}" || priv test -L "${RELAY_TOKEN_FILE}"; then
    fail "the enrolled relay access token ${RELAY_TOKEN_FILE} is not a regular file"
  fi
  metadata="$(stat_string "${RELAY_TOKEN_FILE}")"
  [[ "${metadata}" == "0:${AGENT_GID}:640:1" || "${metadata}" == "0:${AGENT_GID}:440:1" ]] ||
    fail "the enrolled relay access token ${RELAY_TOKEN_FILE} has unsafe metadata (${metadata}); expected root:ocserv-agent mode 0640 or 0440"
  priv test -s "${RELAY_TOKEN_FILE}" ||
    fail "the enrolled relay access token ${RELAY_TOKEN_FILE} is empty"
}

validate_enrolled_key_ancestry() {
  # Mirror of the upgrade preflight ancestry rule for the fixed anchor path:
  # every ancestor must be a root-owned real directory that is not
  # group/world-writable, so Agent-writable ancestry cannot silently swap
  # the trust anchor the running services load.
  local ancestor uid mode
  local -a ancestors=(/etc /etc/ocservia-agent)
  if [[ -z "${SYSROOT}" ]]; then
    ancestors=(/ /etc /etc/ocservia-agent)
  fi
  for ancestor in "${ancestors[@]}"; do
    if ! priv test -d "${SYSROOT}${ancestor}" || priv test -L "${SYSROOT}${ancestor}"; then
      fail "the enrolled command key ancestor ${ancestor} must be a real directory"
    fi
    uid="$(priv stat -c '%u' -- "${SYSROOT}${ancestor}")"
    mode="$(priv stat -c '%a' -- "${SYSROOT}${ancestor}")"
    if [[ "${uid}" != 0 ]] || (( (8#${mode} & 8#022) != 0 )); then
      fail "the enrolled command key ancestor ${ancestor} must stay root-owned and not group/world-writable (found uid=${uid} mode=${mode})"
    fi
  done
}

validate_enrolled_command_key() {
  # The Controller command verification key is the shared Agent/privd
  # command authorization anchor. On an enrolled node the rerun validates it
  # exactly like the services load it and like the upgrade preflight does:
  # agent.env must still pin exactly this installed anchor, its ancestry
  # must stay root-controlled, and the file must be a one-link
  # root:ocserv-agent 0440/0640 regular file containing an Ed25519
  # SubjectPublicKeyInfo public key. A validation-only rerun never installs
  # or replaces it — rotating the command trust anchor is a deliberate
  # operation with old/new key overlap (docs/operations/agent-lifecycle.md).
  local configured matches metadata uid gid mode links size description
  matches="$(priv grep -c '^CONTROLLER_COMMAND_VERIFICATION_KEY_FILE=' "${AGENT_ENV_FILE}" || true)"
  [[ "${matches}" == 1 ]] ||
    fail "the enrolled ${AGENT_ENV_FILE} must contain exactly one CONTROLLER_COMMAND_VERIFICATION_KEY_FILE assignment"
  configured="$(priv sed -n 's/^CONTROLLER_COMMAND_VERIFICATION_KEY_FILE=//p' "${AGENT_ENV_FILE}" | tail -n 1)"
  [[ "${configured}" == "${COMMAND_KEY_FILE}" ]] ||
    fail "the enrolled agent.env pins the command verification key ${configured:-<none>}, not the installed ${COMMAND_KEY_FILE}; resolve the mismatch deliberately"
  validate_enrolled_key_ancestry
  if ! path_exists "${COMMAND_KEY_FILE}"; then
    fail "the enrolled node is missing its Controller command verification key ${COMMAND_KEY_FILE}; restoring the command trust anchor is a deliberate recovery operation, not a bootstrap rerun (docs/operations/agent-lifecycle.md)"
  fi
  if ! priv test -f "${COMMAND_KEY_FILE}" || priv test -L "${COMMAND_KEY_FILE}"; then
    fail "the enrolled Controller command verification key ${COMMAND_KEY_FILE} is not a regular file"
  fi
  metadata="$(stat_string "${COMMAND_KEY_FILE}")"
  IFS=: read -r uid gid mode links <<<"${metadata}"
  size="$(priv stat -c '%s' -- "${COMMAND_KEY_FILE}")"
  [[ "${links}" == 1 && "${size}" -ge 1 && "${size}" -le 4096 ]] ||
    fail "the enrolled Controller command verification key must be a one-link regular file containing 1..4096 bytes"
  [[ "${uid}" == 0 && "${gid}" == "${AGENT_GID}" && ( "${mode}" == 440 || "${mode}" == 640 ) ]] ||
    fail "the enrolled Controller command verification key must be root:ocserv-agent mode 0440 or 0640 so Agent and privd can both load it (found ${metadata})"
  description="$(priv openssl pkey -pubin -in "${COMMAND_KEY_FILE}" -noout -text 2>/dev/null)" ||
    fail "the enrolled Controller command verification key ${COMMAND_KEY_FILE} is not a readable public key PEM"
  [[ "${description}" == "ED25519 Public-Key:"* ]] ||
    fail "the enrolled Controller command verification key must contain an Ed25519 SubjectPublicKeyInfo public key"
}

prepare_production_node() {
  if [[ -n "${ENROLLED_NODE_ID}" ]]; then
    # Enrolled reruns are validation-only for everything the running services
    # load: no ensure_* function may create or replace the relay
    # configuration, the relay access token, or the Controller command
    # verification key.
    validate_enrolled_sealing_keys
    validate_enrolled_relays
    validate_enrolled_relay_token
    validate_enrolled_command_key
    return
  fi
  ensure_sealing_key "${USER_SEAL_KEY_FILE}" user-password
  ensure_sealing_key "${P12_SEAL_KEY_FILE}" p12-password
  USER_SEAL_DESCRIPTOR="$(sealing_descriptor "${USER_SEAL_KEY_FILE}")" ||
    fail "cannot derive the user-password sealing key descriptor; the key is not a usable RSA private key"
  P12_SEAL_DESCRIPTOR="$(sealing_descriptor "${P12_SEAL_KEY_FILE}")" ||
    fail "cannot derive the p12-password sealing key descriptor; the key is not a usable RSA private key"
  [[ "${USER_SEAL_DESCRIPTOR}" =~ ^[0-9a-f]{64}$ && "${P12_SEAL_DESCRIPTOR}" =~ ^[0-9a-f]{64}$ ]] ||
    fail "a sealing key descriptor is malformed"
  [[ "${USER_SEAL_DESCRIPTOR}" != "${P12_SEAL_DESCRIPTOR}" ]] ||
    fail "the two sealing private keys are not distinct; provision two different keys and rerun"
  ensure_relays_env
  ensure_protected_install "${RELAY_ACCESS_TOKEN_SOURCE}" "${RELAY_TOKEN_FILE}" "relay access token"
  ensure_protected_install "${CONTROLLER_COMMAND_VERIFICATION_KEY_SOURCE}" "${COMMAND_KEY_FILE}" "Controller command verification key"
}

prepare_identity() {
  local output
  output="$(as_agent "${AGENT_BINARY}" \
    --identity-dir "${IDENTITY_DIR}" \
    --controller "${CONTROLLER_ENDPOINT_ID}" \
    --prepare-enrollment)" ||
    fail "the Agent could not prepare the persistent identity; a changed controller pin or replaced identity directory is a new trust decision (see docs/operations/agent-lifecycle.md#enroll-the-node)"
  ENDPOINT_ID="$(printf '%s\n' "${output}" | awk 'NF {last=$0} END {print last}')"
  [[ "${ENDPOINT_ID}" =~ ^[0-9a-f]{64}$ ]] ||
    fail "the Agent did not print a valid EndpointID"
  echo "persistent identity prepared; EndpointID: ${ENDPOINT_ID}"
}

is_final_node_id() {
  [[ "$1" =~ ^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$ ]] &&
    [[ "$1" != "${PLACEHOLDER_NODE_ID}" ]]
}

cleanup_enrollment_copy() {
  if ! path_exists "${ENROLLMENT_TOKEN_FILE}"; then return; fi
  if ! priv test -f "${ENROLLMENT_TOKEN_FILE}" || priv test -L "${ENROLLMENT_TOKEN_FILE}" \
    || [[ "$(stat_string "${ENROLLMENT_TOKEN_FILE}")" != "0:${AGENT_GID}:640:1" ]]; then
    fail "registered node has an unsafe enrollment token copy; inspect ${ENROLLMENT_TOKEN_FILE} deliberately (expected one-link root:ocserv-agent 0640)"
  fi
  priv rm -f -- "${ENROLLMENT_TOKEN_FILE}"
  echo "removed the known stale token copy after confirming the local enrollment binding"
}

converge_enrollment() {
  local -a relay_args=(--relay-mode custom --relay-url "${RELAY_URL_A}")
  # Additional public trust is provisioned by the operator, never downloaded
  # from the Relay or replaced by an installer rerun.
  if path_exists "${RELAY_CA_FILE}"; then
    validate_enrolled_key_ancestry
    if ! priv test -f "${RELAY_CA_FILE}" || priv test -L "${RELAY_CA_FILE}" \
      || ! priv test -s "${RELAY_CA_FILE}" \
      || [[ "$(stat_string "${RELAY_CA_FILE}")" != 0:0:444:1 ]]; then
      fail "the Relay CA must be a nonempty one-link root:root regular file with mode 0444"
    fi
    priv openssl x509 -in "${RELAY_CA_FILE}" -noout >/dev/null 2>&1 ||
      fail "the Relay CA must contain a readable PEM certificate"
    relay_args+=(--relay-ca-file "${RELAY_CA_FILE}")
  fi
  local node_id staging metadata
  if [[ -n "${ENROLLED_NODE_ID}" ]]; then
    cleanup_enrollment_copy
    node_bootstrap_complete "${ENROLLED_NODE_ID}"
    return
  fi
  path_exists "${ENROLLMENT_TOKEN_FILE}" ||
    fail "derived enrollment credential is missing; preserve identity and rerun --resume-install"
  metadata="$(stat_string "${ENROLLMENT_TOKEN_FILE}")"
  if ! priv test -f "${ENROLLMENT_TOKEN_FILE}" || priv test -L "${ENROLLMENT_TOKEN_FILE}"; then
    fail "the enrollment token ${ENROLLMENT_TOKEN_FILE} is not a regular file"
  fi
  [[ "${metadata}" == "0:${AGENT_GID}:640:1" ]] ||
    fail "the enrollment token ${ENROLLMENT_TOKEN_FILE} must be root:ocserv-agent mode 0640 (found ${metadata})"
  node_id="$(as_agent "${AGENT_BINARY}" \
    --identity-dir "${IDENTITY_DIR}" \
    --controller "${CONTROLLER_ENDPOINT_ID}" \
    --enrollment-token-file "${ENROLLMENT_TOKEN_FILE}" \
    --enrollment-environment "${ENROLLMENT_ENVIRONMENT}" \
    --user-password-seal-key-id "${USER_PASSWORD_SEAL_KEY_ID}" \
    --user-password-seal-public-key-sha256 "${USER_SEAL_DESCRIPTOR}" \
    --p12-password-seal-key-id "${P12_PASSWORD_SEAL_KEY_ID}" \
    --p12-password-seal-public-key-sha256 "${P12_SEAL_DESCRIPTOR}" \
    "${relay_args[@]}" \
    --relay-token-file "${RELAY_TOKEN_FILE}")" ||
    {
      fail "enrollment failed or its response is unknown; preserve the original EndpointID, identity and bootstrap binding; rerun --resume-install to recover the committed result before requesting new authority"
    }
  node_id="$(printf '%s\n' "${node_id}" | awk 'NF {last=$0} END {print last}')"
  is_final_node_id "${node_id}" ||
    fail "enrollment did not return a valid UUIDv7 node ID"
  staging="$(mktemp "${STAGING_DIR}/agent-env.XXXXXX")"
  printf 'CONTROLLER_ENDPOINT_ID=%s\nNODE_ID=%s\nAGENT_ENDPOINT_ID=%s\nCONTROLLER_COMMAND_VERIFICATION_KEY_FILE=%s\nUSER_PASSWORD_SEAL_KEY_ID=%s\nUSER_PASSWORD_SEAL_PUBLIC_KEY_SHA256=%s\nP12_PASSWORD_SEAL_KEY_ID=%s\nP12_PASSWORD_SEAL_PUBLIC_KEY_SHA256=%s\n' \
    "${CONTROLLER_ENDPOINT_ID}" "${node_id}" "${ENDPOINT_ID}" "${COMMAND_KEY_FILE}" \
    "${USER_PASSWORD_SEAL_KEY_ID}" "${USER_SEAL_DESCRIPTOR}" \
    "${P12_PASSWORD_SEAL_KEY_ID}" "${P12_SEAL_DESCRIPTOR}" >"${staging}"
  write_config_atomic "${staging}" "${AGENT_ENV_FILE}" 0640
  cleanup_enrollment_copy
  if [[ -n "${BOOTSTRAP_TOKEN_SOURCE}" ]]; then
    priv rm -f -- "${BOOTSTRAP_TOKEN_SOURCE}"
  fi
  echo "enrollment complete; final ${AGENT_ENV_FILE} written and the one-time token file consumed"
  node_bootstrap_complete "${node_id}"
}

# Every source enters the same root lifecycle before reading token state.
((EUID == 0)) || forward_root_lifecycle
unset SUDO_USER
validate_sysroot
echo "release identity: ${VERSION}"
resolve_architecture
detect_platform
require_commands
node_bootstrap_prepare
validate_operator_inputs

create_launcher_staging
if native_package_satisfied; then
  echo "native package ocservia-agent $(expected_installed_version) already installed; skipping the release download and package installation"
else
  create_package_staging
  download_native_package
  install_native_package
fi
# The verified native payload owns host validation. Run as root, before any
# registration identity, sealing material, final configuration or token use.
[[ -x "${PRIVD_BINARY}" ]] || fail "the native package did not install ${PRIVD_BINARY}"
priv "${PRIVD_BINARY}" --host-preflight ||
  fail "host preflight failed before enrollment; follow the resource diagnostic, correct the host deliberately, and rerun"
resolve_agent_group
detect_enrolled_node
if [[ -z "${ENROLLED_NODE_ID}" ]]; then
  validate_bootstrap_token_source
fi
prepare_production_node
if [[ -n "${ENROLLED_NODE_ID}" ]]; then
  validate_enrolled_identity
else
  prepare_identity
fi
prepare_bootstrap_token
converge_enrollment
