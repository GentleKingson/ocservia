#!/usr/bin/env bash
# Thin one-command orchestrator for a production managed node.
#
# Scope: starting from a clean checkout of an exact vX.Y.Z release tag, run
# the platform preflight, download the release metadata plus the matching
# native package, verify the out-of-band release trust (trusted public key
# fingerprint -> SHA256SUMS.sig -> selected package digest) BEFORE any
# package manager runs as root, install the native package with the
# production relay request marker, prepare the production node state (sealing
# keys, relay configuration, relay access token, Controller command
# verification key, persistent identity), and stop at ENROLLMENT_READY.
# When the operator later provisions the protected enrollment token file,
# rerunning this same entrypoint completes enrollment, atomically writes the
# final /etc/ocservia-agent/agent.env, consumes the one-time token file, and
# stops at PENDING_APPROVAL.
#
# Reruns converge without repeating completed work: when the native package
# of this exact release is already installed under the production relay
# contract, the release download and the out-of-band trust verification are
# skipped (they protect the package manager invocation, which no longer
# happens), and an already-enrolled node is never enrolled again. Once
# agent.env carries the final NODE_ID, the bootstrap enters a validation-only
# mode for everything the running services load: the persistent identity
# files must exist and pass the Agent's own identity validation (agent
# ownership, owner-only permissions, the 32-byte endpoint key, and the
# controller pin still matching the configured Controller), the EndpointID
# the identity loader derives must equal the AGENT_ENDPOINT_ID enrollment
# binding persisted in agent.env at enrollment (a substituted-but-valid
# endpoint key cannot silently take the Controller enrollment binding), both
# sealing private keys must re-derive exactly the fingerprints pinned in
# agent.env,
# and the relay configuration, relay access token, and Controller command
# verification key must already exist and validate (the command key as an
# Ed25519 anchor under root-controlled ancestry) — or the rerun fails closed.
# It never regenerates identity or sealing keys and never installs or
# replaces relay or command trust material; the Controller-side enrollment
# binding and the command authorization anchor only change through
# deliberate operations.
# After the independent approval, activation stays a deliberate operator
# action outside this bootstrap (systemctl enable --now
# ocservia-privd.service ocservia-agent.service); a rerun then only observes
# the enabled+active services and reports SERVICES_ACTIVE. There is no local
# approval signal a bootstrap could act on without Controller credentials:
# pending nodes are absent from relay admission, and the only in-protocol
# confirmation of approval is the Agent's accepted session, which requires
# the service the operator has not enabled yet.
#
# This script never reimplements the verified package lifecycle, the
# enrollment protocol, or the approval boundary: the native package
# scriptlets (postinst/preremove), the Agent CLI (--prepare-enrollment /
# --enrollment-token-file), and the Controller's expected_endpoint_id token
# contract remain the authorities. It never approves a node, never creates or
# auto-approves an approval request, never places Controller administrator
# credentials on the node, and never enables or starts a service.
#
# Native package first: the default install path is the release .deb on the
# Debian family and the release .rpm on Rocky Linux 9. The signed archive
# stays the trusted payload inside the package and the durable upgrade and
# manual recovery carrier; it is not the one-command install path.
#
# Usage model:
#   git clone --branch vX.Y.Z --depth 1 <ocservia repository>
#   cd ocservia
#   export CONTROLLER_ENDPOINT_ID=<64-lowercase-hex>
#   export RELAY_URL_A=https://relay-a.example.com
#   export RELAY_URL_B=https://relay-b.example.com
#   export RELAY_ACCESS_TOKEN_SOURCE=/protected/relay-access-token
#   export CONTROLLER_COMMAND_VERIFICATION_KEY_SOURCE=/protected/key.pem
#   export TRUSTED_RELEASE_KEY=/etc/ocservia/release-signing.pub.pem        # default
#   export EXPECTED_RELEASE_KEY_SHA256=<64-lowercase-hex>  # else read from
#   #   /etc/ocservia/trusted-release-key.sha256 (the durable upgrader anchor)
#   # (or keep the allowlisted node configuration in ./install.env, parsed by
#   # the strict non-executing loader in deploy/lib/install-env.sh; explicit
#   # shell variables always win over the file)
#   deploy/managed-node/install.sh   # operator launcher user -> ENROLLMENT_READY
#   # create a one-time token with expected_endpoint_id=<printed EndpointID>
#   # and install it as /etc/ocservia-agent/enrollment-token root:ocserv-agent 0640
#   deploy/managed-node/install.sh   # -> PENDING_APPROVAL
#   # approve the node (docs/how-to/enroll-node.md#approve-the-node), then:
#   sudo systemctl enable --now ocservia-privd.service ocservia-agent.service
#   deploy/managed-node/install.sh   # -> SERVICES_ACTIVE (read-only verification)
#
# Supported hosts, mirroring what this repository's installers and CI actually
# exercise: x86_64 and aarch64; Ubuntu 22.04/24.04/26.04 and Debian 12/13
# through dpkg; Rocky Linux 9 through rpm; systemd required. Ubuntu 20.04 and
# Debian 11 are deliberately excluded: they ship OpenSSL 1.1.1, whose pkeyutl
# cannot verify the Ed25519 SHA256SUMS signature this bootstrap (and the
# package's own verifier) depend on. Any other platform fails closed before
# any host mutation.
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

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
DOWNLOAD_BASE="https://github.com/GentleKingson/ocservia/releases/download"
OS_RELEASE_FILE="${OCSERV_MANAGED_NODE_OS_RELEASE:-/etc/os-release}"
SYSROOT="${OCSERV_MANAGED_NODE_SYSROOT:-}"
AGENT_BINARY="${SYSROOT}/usr/libexec/ocservia/ocservia-agent"
RELAY_DROPIN="${SYSROOT}/usr/lib/systemd/system/ocservia-agent.service.d/10-production-relays.conf"
REQUEST_MARKER="${SYSROOT}/etc/ocservia/agent-install-production-relays"
TRUSTED_FINGERPRINT_FILE="/etc/ocservia/trusted-release-key.sha256"
AGENT_CONF_DIR="${SYSROOT}/etc/ocservia-agent"
AGENT_ENV_FILE="${AGENT_CONF_DIR}/agent.env"
RELAYS_ENV_FILE="${AGENT_CONF_DIR}/relays.env"
RELAY_TOKEN_FILE="${AGENT_CONF_DIR}/relay-access-token"
COMMAND_KEY_FILE="${AGENT_CONF_DIR}/controller-command-verification-key.pem"
ENROLLMENT_TOKEN_FILE="${AGENT_CONF_DIR}/enrollment-token"
USER_SEAL_KEY_FILE="${AGENT_CONF_DIR}/user-password-seal-private.pem"
P12_SEAL_KEY_FILE="${AGENT_CONF_DIR}/p12-password-seal-private.pem"
IDENTITY_DIR="${SYSROOT}/var/lib/ocservia-agent/identity"
# The pristine placeholder relay URLs installed by the verified package from
# deploy/production/systemd/relays.env.example.
PLACEHOLDER_RELAY_URL_A="https://relay-a.example.com"
PLACEHOLDER_RELAY_URL_B="https://relay-b.example.com"
PLACEHOLDER_NODE_ID="00000000-0000-7000-8000-000000000000"
SUPPORTED_HOSTS="Ubuntu 22.04/24.04/26.04 and Debian 12/13 (dpkg), Rocky Linux 9 (rpm), x86_64/aarch64, systemd"
RELEASE_TAG=""
RELEASE_COMMIT=""
RELEASE_VERSION=""
ARCH_WORD=""
RPM_ARCH=""
PACKAGE_FAMILY=""
PACKAGE_MANAGER=""
PACKAGE_FILE=""
STAGING_DIR=""
PACKAGE_STAGING_DIR=""
FROZEN_RELEASE_KEY=""
ROOT_LIFECYCLE=false
EXPECTED_PACKAGE_DIGEST=""
AGENT_GID=""
ENDPOINT_ID=""
ENROLLED_NODE_ID=""
USER_PASSWORD_SEAL_KEY_ID="${USER_PASSWORD_SEAL_KEY_ID:-}"
P12_PASSWORD_SEAL_KEY_ID="${P12_PASSWORD_SEAL_KEY_ID:-}"
ENROLLMENT_ENVIRONMENT="${ENROLLMENT_ENVIRONMENT:-}"
USER_SEAL_DESCRIPTOR=""
P12_SEAL_DESCRIPTOR=""

# Only managed-node configuration crosses the internal sudo boundary on the
# deliberate --root-lifecycle path. Every value is a public descriptor or a
# path to operator-provisioned protected material; the fixture seams cross
# for the same reason as the Controller state root: an explicitly configured
# root must stay identical on both sides of the boundary.
ROOT_LIFECYCLE_ENV_NAMES=(
  CONTROLLER_COMMAND_VERIFICATION_KEY_SOURCE
  CONTROLLER_ENDPOINT_ID
  ENROLLMENT_ENVIRONMENT
  EXPECTED_RELEASE_KEY_SHA256
  OCSERV_MANAGED_NODE_OS_RELEASE
  OCSERV_MANAGED_NODE_SYSROOT
  P12_PASSWORD_SEAL_KEY_ID
  RELAY_ACCESS_TOKEN_SOURCE
  RELAY_URL_A
  RELAY_URL_B
  TRUSTED_RELEASE_KEY
  USER_PASSWORD_SEAL_KEY_ID
)

fail() {
  echo "managed-node install: $1" >&2
  exit 1
}

if (($# > 1)); then
  echo "usage: deploy/managed-node/install.sh [--root-lifecycle] (the node configuration comes from the operator session or ./install.env)" >&2
  exit 2
fi
case "${1:-}" in
  "") ;;
  --root-lifecycle) ROOT_LIFECYCLE=true ;;
  *)
    echo "usage: deploy/managed-node/install.sh [--root-lifecycle] (the node configuration comes from the operator session or ./install.env)" >&2
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
# $PWD/install.env as new authoritative input. The fixture seams above stay
# deliberately outside this allowlist: they are test seams, not operator
# configuration, and must not gain an install.env entry.
if [[ -z "${OCSERV_INSTALL_ENV_RESOLVED:-}" ]]; then
  # shellcheck source=../lib/install-env.sh disable=SC1091
  source "${ROOT}/deploy/lib/install-env.sh"
  install_env_load "${PWD}/install.env" \
    CONTROLLER_COMMAND_VERIFICATION_KEY_SOURCE \
    CONTROLLER_ENDPOINT_ID \
    ENROLLMENT_ENVIRONMENT \
    EXPECTED_RELEASE_KEY_SHA256 \
    P12_PASSWORD_SEAL_KEY_ID \
    RELAY_ACCESS_TOKEN_SOURCE \
    RELAY_URL_A \
    RELAY_URL_B \
    TRUSTED_RELEASE_KEY \
    USER_PASSWORD_SEAL_KEY_ID
fi

# Privileged steps run one command at a time: directly when this process is
# already the deliberate root lifecycle, otherwise through sudo with explicit
# arguments only. The node configuration directory is mode 0750
# root:ocserv-agent, so existence probes and metadata reads below the
# privileged boundary must also cross it.
priv() {
  if ((EUID == 0)); then
    "$@"
  else
    sudo "$@"
  fi
}

# The Agent binary refuses to run as root, so the offline identity
# preparation and the one-shot enrollment always run as ocserv-agent.
as_agent() {
  if ((EUID == 0)); then
    runuser -u ocserv-agent -- "$@"
  else
    sudo -u ocserv-agent "$@"
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
    "${ROOT}/deploy/managed-node/install.sh" --root-lifecycle
}

if [[ "${ROOT_LIFECYCLE}" == true ]]; then
  ((EUID == 0)) || forward_root_lifecycle
  # sudo retains SUDO_USER through env; strip it so the deliberate root
  # lifecycle is not misread as a whole-script sudo invocation below.
  unset SUDO_USER
fi

if ((EUID == 0)) && [[ "${ROOT_LIFECYCLE}" == false ]]; then
  case "${SUDO_USER:-}" in
    "" | root) ;;
    *)
      fail "run install.sh as the operator launcher user; the installer invokes sudo only for scoped privileged steps (whole-script sudo from '${SUDO_USER}' would hand the entire operator environment to root); for a deliberate root lifecycle run 'deploy/managed-node/install.sh --root-lifecycle'"
      ;;
  esac
fi

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
  RELEASE_VERSION="${RELEASE_TAG#v}"
  [[ -z "$(git -C "${ROOT}" status --porcelain --untracked-files=all)" ]] ||
    fail "the checkout at ${RELEASE_TAG} is dirty; install from a clean checkout"
  echo "release identity: ${RELEASE_TAG} (${RELEASE_COMMIT})"
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
      PACKAGE_FILE="ocservia-agent_${RELEASE_VERSION}_${ARCH_WORD}.deb"
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
  echo "platform: ${os_id} ${os_version_id} (${PACKAGE_FAMILY}, ${ARCH_WORD}; package ${PACKAGE_FILE})"
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
  for tool in git curl openssl sha256sum awk; do
    command -v "${tool}" >/dev/null 2>&1 ||
      fail "${tool} is required by the managed-node installer"
  done
}

validate_operator_inputs() {
  local variable
  for variable in CONTROLLER_ENDPOINT_ID RELAY_URL_A RELAY_URL_B \
    RELAY_ACCESS_TOKEN_SOURCE CONTROLLER_COMMAND_VERIFICATION_KEY_SOURCE; do
    [[ -n "${!variable:-}" ]] ||
      fail "${variable} is not set; export the managed-node configuration before running the installer"
  done
  [[ "${CONTROLLER_ENDPOINT_ID}" =~ ^[0-9a-f]{64}$ ]] ||
    fail "CONTROLLER_ENDPOINT_ID must be the 64-lowercase-hex Controller EndpointID"
  for variable in RELAY_URL_A RELAY_URL_B; do
    case "${!variable}" in
      https://*) ;;
      *) fail "${variable} must be an https:// dedicated relay URL" ;;
    esac
  done
  [[ "${RELAY_URL_A}" != "${RELAY_URL_B}" ]] ||
    fail "RELAY_URL_A and RELAY_URL_B must be two distinct dedicated relay URLs"
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

resolve_trust_anchor() {
  local expected_fingerprint
  TRUSTED_RELEASE_KEY="${TRUSTED_RELEASE_KEY:-/etc/ocservia/release-signing.pub.pem}"
  if [[ -n "${EXPECTED_RELEASE_KEY_SHA256:-}" ]]; then
    expected_fingerprint="${EXPECTED_RELEASE_KEY_SHA256}"
  else
    expected_fingerprint="$(priv cat -- "${TRUSTED_FINGERPRINT_FILE}")" ||
      fail "cannot read the trusted release key fingerprint ${TRUSTED_FINGERPRINT_FILE}; provision it or export EXPECTED_RELEASE_KEY_SHA256"
  fi
  [[ "${expected_fingerprint}" =~ ^[0-9a-f]{64}$ ]] ||
    fail "the trusted release key fingerprint must be 64 lowercase hexadecimal characters"
  EXPECTED_RELEASE_KEY_SHA256="${expected_fingerprint}"
  [[ -f "${TRUSTED_RELEASE_KEY}" && ! -L "${TRUSTED_RELEASE_KEY}" ]] ||
    fail "the trusted release public key ${TRUSTED_RELEASE_KEY} is missing; provision it through an independent protected channel (TRUSTED_RELEASE_KEY)"
}

create_launcher_staging() {
  STAGING_DIR="$(mktemp -d "${TMPDIR:-/tmp}/ocservia-managed-node.XXXXXX")"
  trap '[[ -z "${PACKAGE_STAGING_DIR:-}" ]] || priv rm -rf -- "${PACKAGE_STAGING_DIR}"; [[ -z "${STAGING_DIR:-}" ]] || rm -rf -- "${STAGING_DIR}"' EXIT INT TERM
}

freeze_trust_anchor() {
  # Freeze the operator-provisioned release key before trusting anything from
  # it, mirroring scripts/verify-agent-package.sh: TRUSTED_RELEASE_KEY may
  # point anywhere the operator chose, including a launcher-writable path, so
  # pathname re-reads would let a launcher-UID process swap the key between
  # the fingerprint check and the signature verification. The fingerprint and
  # every later use read the same root-owned frozen copy. The staging parent
  # is a fixed system directory, never the operator's TMPDIR: pathname trust
  # comes from the parent, and an operator-owned TMPDIR would let the
  # launcher rename or replace even this root-owned staging entry. /var/tmp
  # is a root-owned sticky system directory on every supported host.
  PACKAGE_STAGING_DIR="$(priv mktemp -d /var/tmp/ocservia-managed-node-pkg.XXXXXX)"
  FROZEN_RELEASE_KEY="${PACKAGE_STAGING_DIR}/release-signing.pub.pem"
  priv install -o root -g root -m 0644 -- "${TRUSTED_RELEASE_KEY}" "${FROZEN_RELEASE_KEY}"
}

verify_trust_anchor() {
  local actual_fingerprint
  # The fingerprint is computed from the frozen root-owned copy the signature
  # verification later uses, so the key that passed the fingerprint check is
  # byte-identical to the key that verifies the manifest. This still happens
  # before anything is downloaded.
  actual_fingerprint="$(priv openssl pkey -pubin -in "${FROZEN_RELEASE_KEY}" -outform DER | sha256sum | awk '{print $1}')" ||
    fail "the trusted release public key ${TRUSTED_RELEASE_KEY} is not a readable public key PEM"
  [[ "${actual_fingerprint}" == "${EXPECTED_RELEASE_KEY_SHA256}" ]] ||
    fail "the trusted release public key fingerprint ${actual_fingerprint} does not match the expected ${EXPECTED_RELEASE_KEY_SHA256}"
}

download_release_artifacts() {
  local name
  for name in SHA256SUMS SHA256SUMS.sig "${PACKAGE_FILE}"; do
    echo "downloading ${DOWNLOAD_BASE}/${RELEASE_TAG}/${name}"
    curl -fsSL --proto '=https' --tlsv1.2 \
      --output "${STAGING_DIR}/${name}" "${DOWNLOAD_BASE}/${RELEASE_TAG}/${name}"
  done
}

freeze_release_artifacts() {
  # Freeze the downloaded artifacts beside the frozen release key before any
  # trust verification. The launcher's staging directory stays writable by
  # the launcher, so verifying bytes there and then parsing the manifest or
  # digesting the package from there would let a second launcher-UID process
  # swap the signed manifest (and the package) after the signature check
  # succeeds — the digest that authorizes the install must come from exactly
  # the manifest that passed verification. From this point on the bootstrap
  # never reads the launcher staging again.
  local name
  for name in SHA256SUMS SHA256SUMS.sig "${PACKAGE_FILE}"; do
    priv install -o root -g root -m 0644 -- "${STAGING_DIR}/${name}" \
      "${PACKAGE_STAGING_DIR}/${name}"
  done
}

verify_release_trust() {
  local manifest_line expected_digest actual_digest matches
  # The release-signing public key is intentionally absent from the download:
  # trust comes only from the operator-provisioned anchor verified above.
  # The frozen key, the signature, the manifest parse, and the package digest
  # all read the root-owned staging, so the launcher cannot influence any of
  # them after the fact; the staging directory is mode 0700 root, so every
  # read crosses the privileged boundary.
  priv openssl pkeyutl -verify -rawin -pubin -inkey "${FROZEN_RELEASE_KEY}" \
    -in "${PACKAGE_STAGING_DIR}/SHA256SUMS" \
    -sigfile "${PACKAGE_STAGING_DIR}/SHA256SUMS.sig" >/dev/null ||
    fail "the release checksum manifest signature verification failed"
  manifest_line="$(priv grep -F -- "  ${PACKAGE_FILE}" "${PACKAGE_STAGING_DIR}/SHA256SUMS" || true)"
  matches="$(priv grep -cF -- "  ${PACKAGE_FILE}" "${PACKAGE_STAGING_DIR}/SHA256SUMS" || true)"
  [[ "${matches}" == 1 ]] ||
    fail "the signed checksum manifest must name ${PACKAGE_FILE} exactly once (found ${matches})"
  expected_digest="${manifest_line%%"  "*}"
  [[ "${expected_digest}" =~ ^[0-9a-f]{64}$ ]] ||
    fail "the signed checksum entry for ${PACKAGE_FILE} is malformed"
  actual_digest="$(priv sha256sum -- "${PACKAGE_STAGING_DIR}/${PACKAGE_FILE}" | awk '{print $1}')" ||
    fail "cannot digest the downloaded package"
  [[ "${actual_digest}" == "${expected_digest}" ]] ||
    fail "the ${PACKAGE_FILE} digest does not match the signed checksum manifest"
  EXPECTED_PACKAGE_DIGEST="${expected_digest}"
  echo "release trust verified out of band: ${PACKAGE_FILE} digest ${expected_digest}"
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
    # nfpm appends the release component to the deb Version (deploy/package/
    # nfpm.yaml sets release: 1, and scripts/release-native-package-smoke.sh
    # asserts the installed deb Version is X.Y.Z-1); rpm keeps the release in
    # %{RELEASE}, so %{VERSION} stays the bare SemVer.
    deb) echo "${RELEASE_VERSION}-1" ;;
    rpm) echo "${RELEASE_VERSION}" ;;
  esac
}

native_package_satisfied() {
  # A converged rerun must not re-download or reinstall the native package.
  # The out-of-band release trust protects the package manager invocation,
  # which an already-installed package makes unnecessary, so a satisfied
  # package skips the whole download-and-verify phase. The installed version
  # and production relay contract checks stay fail-closed either way.
  local installed_version
  installed_version="$(installed_package_version)"
  [[ -n "${installed_version}" ]] || return 1
  [[ "${installed_version}" == "$(expected_installed_version)" ]] ||
    fail "ocservia-agent ${installed_version} is installed but this checkout is release ${RELEASE_VERSION} (native ${PACKAGE_FAMILY} version $(expected_installed_version)); the bootstrap neither upgrades nor downgrades an installed package — use the Agent release lifecycle (docs/operations/agent-lifecycle.md)"
  [[ -f "${RELAY_DROPIN}" && ! -L "${RELAY_DROPIN}" ]] ||
    fail "the installed ocservia-agent ${installed_version} lacks the production relay drop-in ${RELAY_DROPIN}; it was installed without the production request — reinstall it through the release lifecycle with /etc/ocservia/agent-install-production-relays present"
  return 0
}

install_native_package() {
  local actual_digest
  # Re-check the digest on the frozen root-owned copy immediately before the
  # package manager, so the exact bytes about to be installed are still the
  # bytes verified against the signed manifest; every read since the freeze
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
  local metadata existing_a existing_b staging
  if path_exists "${RELAYS_ENV_FILE}"; then
    metadata="$(stat_string "${RELAYS_ENV_FILE}")"
    if ! priv test -f "${RELAYS_ENV_FILE}" || priv test -L "${RELAYS_ENV_FILE}"; then
      fail "the existing ${RELAYS_ENV_FILE} is not a regular file"
    fi
    [[ "${metadata}" == "0:${AGENT_GID}:640:1" ]] ||
      fail "the existing ${RELAYS_ENV_FILE} has unsafe metadata (${metadata}); expected root:ocserv-agent mode 0640"
    existing_a="$(priv sed -n 's/^RELAY_URL_A=//p' "${RELAYS_ENV_FILE}" | tail -n 1)"
    existing_b="$(priv sed -n 's/^RELAY_URL_B=//p' "${RELAYS_ENV_FILE}" | tail -n 1)"
    if [[ "${existing_a}" == "${RELAY_URL_A}" && "${existing_b}" == "${RELAY_URL_B}" ]]; then
      echo "preserved existing relay configuration (${RELAYS_ENV_FILE})"
      return
    fi
    [[ "${existing_a}" == "${PLACEHOLDER_RELAY_URL_A}" && "${existing_b}" == "${PLACEHOLDER_RELAY_URL_B}" ]] ||
      fail "the existing ${RELAYS_ENV_FILE} requests ${existing_a} / ${existing_b}, not the configured dedicated relays; resolve the mismatch deliberately instead of overwriting node configuration"
  fi
  staging="$(mktemp "${STAGING_DIR}/relays-env.XXXXXX")"
  printf 'RELAY_URL_A=%s\nRELAY_URL_B=%s\n' "${RELAY_URL_A}" "${RELAY_URL_B}" >"${staging}"
  write_config_atomic "${staging}" "${RELAYS_ENV_FILE}" 0640
  echo "wrote dedicated relay URLs to ${RELAYS_ENV_FILE}"
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
      fail "the enrolled node is missing its persistent identity file ${file}; regenerating the identity would break the Controller enrollment binding — restore ${IDENTITY_DIR} from protected backups or re-enroll as a new node deliberately (docs/how-to/enroll-node.md)"
    fi
  done
  enrolled_endpoint_id="$(priv sed -n 's/^AGENT_ENDPOINT_ID=//p' "${AGENT_ENV_FILE}" | tail -n 1)"
  [[ "${enrolled_endpoint_id}" =~ ^[0-9a-f]{64}$ ]] ||
    fail "the enrolled ${AGENT_ENV_FILE} has no valid AGENT_ENDPOINT_ID enrollment binding; resolve it deliberately instead of trusting an unbound enrolled identity (docs/how-to/enroll-node.md)"
  output="$(as_agent "${AGENT_BINARY}" \
    --identity-dir "${IDENTITY_DIR}" \
    --controller "${CONTROLLER_ENDPOINT_ID}" \
    --prepare-enrollment)" ||
    fail "the enrolled persistent identity in ${IDENTITY_DIR} did not pass the Agent identity validation; the bootstrap never regenerates an enrolled identity — restore the identity material or re-enroll deliberately (docs/how-to/enroll-node.md)"
  endpoint_id="$(printf '%s\n' "${output}" | awk 'NF {last=$0} END {print last}')"
  [[ "${endpoint_id}" =~ ^[0-9a-f]{64}$ ]] ||
    fail "the Agent did not print a valid EndpointID while validating the enrolled identity"
  [[ "${endpoint_id}" == "${enrolled_endpoint_id}" ]] ||
    fail "the enrolled identity now presents EndpointID ${endpoint_id} but the enrolled ${AGENT_ENV_FILE} pins ${enrolled_endpoint_id}; a replaced endpoint secret key cannot silently take the Controller enrollment binding — restore the identity material or re-enroll as a new node deliberately (docs/how-to/enroll-node.md)"
  echo "enrolled persistent identity validated; EndpointID: ${endpoint_id}"
}

validate_enrolled_relays() {
  # On an enrolled node relays.env is the running relay configuration. It
  # must already exist, be safe, and name exactly the configured dedicated
  # relays; a validation-only rerun never writes it.
  local metadata existing_a existing_b
  if ! path_exists "${RELAYS_ENV_FILE}"; then
    fail "the enrolled node is missing its relay configuration ${RELAYS_ENV_FILE}; restoring it is a deliberate recovery operation, not a bootstrap rerun (docs/getting-started/managed-node.md)"
  fi
  if ! priv test -f "${RELAYS_ENV_FILE}" || priv test -L "${RELAYS_ENV_FILE}"; then
    fail "the enrolled relay configuration ${RELAYS_ENV_FILE} is not a regular file"
  fi
  metadata="$(stat_string "${RELAYS_ENV_FILE}")"
  [[ "${metadata}" == "0:${AGENT_GID}:640:1" ]] ||
    fail "the enrolled relay configuration ${RELAYS_ENV_FILE} has unsafe metadata (${metadata}); expected root:ocserv-agent mode 0640"
  existing_a="$(priv sed -n 's/^RELAY_URL_A=//p' "${RELAYS_ENV_FILE}" | tail -n 1)"
  existing_b="$(priv sed -n 's/^RELAY_URL_B=//p' "${RELAYS_ENV_FILE}" | tail -n 1)"
  [[ "${existing_a}" == "${RELAY_URL_A}" && "${existing_b}" == "${RELAY_URL_B}" ]] ||
    fail "the enrolled ${RELAYS_ENV_FILE} names ${existing_a:-<none>} / ${existing_b:-<none>}, not the configured dedicated relays; a rerun never rewrites the running relay configuration — resolve the mismatch deliberately"
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
    fail "the Agent could not prepare the persistent identity; a changed controller pin or replaced identity directory is a new trust decision (see docs/how-to/enroll-node.md)"
  ENDPOINT_ID="$(printf '%s\n' "${output}" | awk 'NF {last=$0} END {print last}')"
  [[ "${ENDPOINT_ID}" =~ ^[0-9a-f]{64}$ ]] ||
    fail "the Agent did not print a valid EndpointID"
  echo "persistent identity prepared; EndpointID: ${ENDPOINT_ID}"
}

is_final_node_id() {
  [[ "$1" =~ ^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$ ]] &&
    [[ "$1" != "${PLACEHOLDER_NODE_ID}" ]]
}

services_enabled_and_active() {
  # Read-only observation, the only post-activation signal available without
  # Controller credentials: it proves the operator completed the deliberate
  # enable step, not Controller-side approval or an accepted session. The
  # queries never mutate anything; the bootstrap still never enables, starts,
  # stops, or masks a service.
  local unit
  for unit in ocservia-privd.service ocservia-agent.service; do
    [[ "$(systemctl is-enabled "${unit}" 2>/dev/null || true)" == "enabled" ]] || return 1
    [[ "$(systemctl is-active "${unit}" 2>/dev/null || true)" == "active" ]] || return 1
  done
}

print_pending_approval() {
  echo "PENDING_APPROVAL"
  echo "NODE_ID: ${1}"
  echo "next: approve the node as described in docs/how-to/enroll-node.md#approve-the-node, then enable ocservia-privd.service and ocservia-agent.service; the bootstrap never starts or enables a service"
}

print_services_active() {
  echo "SERVICES_ACTIVE"
  echo "NODE_ID: ${1}"
  echo "next: both managed-node services are enabled and active; confirm the node reports online in the Controller inventory (the bootstrap cannot observe Controller-side approval)"
}

converge_enrollment() {
  local node_id staging metadata
  if [[ -n "${ENROLLED_NODE_ID}" ]]; then
    if path_exists "${ENROLLMENT_TOKEN_FILE}"; then
      echo "an enrollment token file is present but this node is already enrolled; remove the stale token file" >&2
    fi
    if services_enabled_and_active; then
      print_services_active "${ENROLLED_NODE_ID}"
    else
      print_pending_approval "${ENROLLED_NODE_ID}"
    fi
    return
  fi
  if ! path_exists "${ENROLLMENT_TOKEN_FILE}"; then
    echo "ENROLLMENT_READY"
    echo "next: create a short-lived one-time enrollment token with expected_endpoint_id=${ENDPOINT_ID} (docs/how-to/enroll-node.md), install it as ${ENROLLMENT_TOKEN_FILE} (root:ocserv-agent 0640), and rerun deploy/managed-node/install.sh"
    return
  fi
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
    --relay-mode custom \
    --relay-url "${RELAY_URL_A}" \
    --relay-url "${RELAY_URL_B}" \
    --relay-token-file "${RELAY_TOKEN_FILE}")" ||
    fail "enrollment failed; the identity and prepared configuration are unchanged — create a fresh one-time token and rerun (a token is one-time and short-lived)"
  node_id="$(printf '%s\n' "${node_id}" | awk 'NF {last=$0} END {print last}')"
  is_final_node_id "${node_id}" ||
    fail "enrollment did not return a valid UUIDv7 node ID"
  staging="$(mktemp "${STAGING_DIR}/agent-env.XXXXXX")"
  printf 'CONTROLLER_ENDPOINT_ID=%s\nNODE_ID=%s\nAGENT_ENDPOINT_ID=%s\nCONTROLLER_COMMAND_VERIFICATION_KEY_FILE=%s\nUSER_PASSWORD_SEAL_KEY_ID=%s\nUSER_PASSWORD_SEAL_PUBLIC_KEY_SHA256=%s\nP12_PASSWORD_SEAL_KEY_ID=%s\nP12_PASSWORD_SEAL_PUBLIC_KEY_SHA256=%s\n' \
    "${CONTROLLER_ENDPOINT_ID}" "${node_id}" "${ENDPOINT_ID}" "${COMMAND_KEY_FILE}" \
    "${USER_PASSWORD_SEAL_KEY_ID}" "${USER_SEAL_DESCRIPTOR}" \
    "${P12_PASSWORD_SEAL_KEY_ID}" "${P12_SEAL_DESCRIPTOR}" >"${staging}"
  write_config_atomic "${staging}" "${AGENT_ENV_FILE}" 0640
  priv rm -f -- "${ENROLLMENT_TOKEN_FILE}"
  echo "enrollment complete; final ${AGENT_ENV_FILE} written and the one-time token file consumed"
  print_pending_approval "${node_id}"
}

validate_sysroot
resolve_release_identity
resolve_architecture
detect_platform
require_commands
validate_operator_inputs
create_launcher_staging
if native_package_satisfied; then
  echo "native package ocservia-agent $(expected_installed_version) already installed; skipping the release download and package installation"
else
  resolve_trust_anchor
  freeze_trust_anchor
  verify_trust_anchor
  download_release_artifacts
  freeze_release_artifacts
  verify_release_trust
  install_native_package
fi
resolve_agent_group
detect_enrolled_node
prepare_production_node
if [[ -n "${ENROLLED_NODE_ID}" ]]; then
  validate_enrolled_identity
else
  prepare_identity
fi
converge_enrollment
