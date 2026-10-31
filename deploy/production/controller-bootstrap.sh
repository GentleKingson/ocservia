#!/usr/bin/env bash
# Self-contained Release entrypoint. Collect inputs, retain a minimal verified
# runtime bundle, then delegate to the existing Quick/Manual installer.
set -euo pipefail
umask 077

readonly DEFAULT_VERSION=""
SCRIPT_LIB="$(dirname "${BASH_SOURCE[0]}")/../lib"
# shellcheck source=deploy/lib/controller-runtime.sh
source "${SCRIPT_LIB}/controller-runtime.sh" # release-embed
# shellcheck source=deploy/lib/install-ui.sh
source "${SCRIPT_LIB}/install-ui.sh" # release-embed
INTERACTIVE=false
NON_INTERACTIVE=false
(($# != 0)) || INTERACTIVE=true
CONFIG_FILE="${PWD}/install.env"
CONFIG_ROOT="${PWD}"
VERSION="${DEFAULT_VERSION}"
ROOT_LIFECYCLE=false
CHECK_ONLY=false
QUICK=false
CONTROLLER_DOMAIN=""
RELAY_DOMAIN=""
ACME_EMAIL=""
QUICK_ARGS=()
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
  OCSERV_DATABASE_BACKEND
  OCSERV_DATABASE_DEPLOYMENT
  OCSERV_DATABASE_BACKUP_HOST
  OCSERV_DATABASE_BACKUP_PORT
  OCSERV_DATABASE_BACKUP_NAME
  OCSERV_DATABASE_BACKUP_USER
  OCSERV_DATABASE_BACKUP_IMAGE
  OCSERV_DEPLOYMENT_MODE
  OCSERV_RELAY_PUBLIC_HOST
  OCSERV_RELAY_SECRET_DIR
  OCSERV_TLS_MODE
  OCSERV_ACME_EMAIL
  OCSERV_ACME_DIRECTORY_URL
  OCSERV_ACME_CA_FILE
  OCSERV_SIGNER_SECRET_DIR
  OCSERV_SIGNER_STATE_DIR
  OCSERV_EDGE_GATEWAY_IP
  OCSERV_EDGE_GATEWAY_SUBNET
  OCSERV_EDGE_GATEWAY_IP_RANGE
  OCSERV_CERTIFICATE_SIGNER_URL
  OCSERV_CONTROLLER_ENDPOINT_ID
  OCSERV_CONTROLLER_PUBLIC_URL
  OCSERV_CONTROLLER_STATE_DIR
  OCSERV_CONTROLLER_STATE_ROOT
  OCSERV_HTTPS_ADDRESS
  OCSERV_AUTH_TRUSTED_PROXY_CIDRS
  OCSERV_APPLICATION_SUBNET
  OCSERV_APPLICATION_IP_RANGE
  OCSERV_GATEWAY_APPLICATION_IP
  OCSERV_LOCAL_AUTH_ENABLED
  OCSERV_PUBLIC_ORIGIN
  OCSERV_SESSION_TTL
  OCSERV_RECOMMENDED_AGENT_VERSION
  OCSERV_OIDC_REDIRECT_URL
  OCSERV_OIDC_CLIENT_ID
  OCSERV_OIDC_ISSUER
  OCSERV_OTEL_BACKEND_ENDPOINT
  OCSERV_PUBLIC_HOST
  OCSERV_RELAY_URL_A
  OCSERV_RELAY_URL_B
  OCSERV_SECRET_DIR
)

# Embedded install.env loader: the versioned installer contract copy of
# deploy/lib/install-env.sh, kept functionally identical to it. This bootstrap
# is a single self-contained Release asset, so it cannot source repository
# siblings before it downloads the requested runtime; any loader contract change
# is made in all embedded and shared copies.
install_env_die() {
  echo "install.env: $1" >&2
  exit 1
}

# install_env_load <file> <allowlisted-key>...
install_env_load() {
  local file="$1"
  shift
  local -a allowlist=("$@")
  local -a seen=()
  local -a preset=()
  local allowed key line value mode first last lineno=0 known

  if [[ -L "${file}" ]]; then
    install_env_die "refusing the configuration symlink ${file}; install.env must be a regular file"
  fi
  if [[ ! -e "${file}" ]]; then
    return 0
  fi
  if [[ ! -f "${file}" ]]; then
    install_env_die "${file} is not a regular file"
  fi
  if [[ ! -r "${file}" ]]; then
    install_env_die "${file} is not readable by the invoking user"
  fi
  mode="$(stat -c '%a' -- "${file}")" ||
    install_env_die "cannot inspect the permissions of ${file}"
  if (( (8#${mode} & 8#022) != 0 )); then
    install_env_die "refusing the group/world-writable configuration file ${file} (mode ${mode})"
  fi

  for allowed in "${allowlist[@]}"; do
    if [[ -n "${!allowed+x}" ]]; then
      preset+=("${allowed}")
    fi
  done

  while IFS= read -r line || [[ -n "${line}" ]]; do
    lineno=$((lineno + 1))
    if [[ -z "${line}" || "${line}" == \#* ]]; then
      continue
    fi
    if [[ ! "${line}" =~ ^([A-Z][A-Z0-9_]*)=(.*)$ ]]; then
      install_env_die "${file}:${lineno}: expected KEY=VALUE, a # comment, or a blank line"
    fi
    key="${BASH_REMATCH[1]}"
    value="${BASH_REMATCH[2]}"
    case "${value}" in
      *'$'* | *'`'*)
        install_env_die "${file}:${lineno}: ${key} must be a literal value; shell expansion syntax is never evaluated"
        ;;
    esac
    if [[ "${value}" =~ [[:cntrl:]] ]]; then
      install_env_die "${file}:${lineno}: ${key} contains a control character"
    fi
    if (( ${#value} >= 2 )); then
      first="${value:0:1}"
      last="${value:${#value}-1:1}"
      if [[ ( "${first}" == "'" && "${last}" == "'" ) || ( "${first}" == '"' && "${last}" == '"' ) ]]; then
        value="${value:1:${#value}-2}"
      fi
    fi
    known=false
    for allowed in "${allowlist[@]}"; do
      if [[ "${key}" == "${allowed}" ]]; then
        known=true
        break
      fi
    done
    if [[ "${known}" != true ]]; then
      install_env_die "${file}:${lineno}: unknown configuration variable ${key}"
    fi
    for allowed in ${seen[@]+"${seen[@]}"}; do
      if [[ "${key}" == "${allowed}" ]]; then
        install_env_die "${file}:${lineno}: duplicate configuration variable ${key}"
      fi
    done
    seen+=("${key}")
    for allowed in ${preset[@]+"${preset[@]}"}; do
      if [[ "${key}" == "${allowed}" ]]; then
        continue 2
      fi
    done
    printf -v "${key}" '%s' "${value}"
    # shellcheck disable=SC2163 # key is a validated allowlisted identifier
    export "${key}"
  done <"${file}"

  if (( ${#seen[@]} > 0 )); then
    echo "loaded configuration from ${file}"
  fi
}

fail() {
  echo "controller bootstrap: $1" >&2
  exit 1
}

usage() {
  echo "No arguments: interactive wizard (TTY required). --non-interactive: unattended, no prompts." >&2
  echo "Options: --config PATH (default ./install.env), --version vX.Y.Z (Release default embedded)." >&2
  echo "usage: controller-bootstrap.sh --version vX.Y.Z [--root-lifecycle] [--check]" >&2
  echo "       controller-bootstrap.sh --version vX.Y.Z --quick --controller-domain NAME --relay-domain NAME" >&2
  echo "         --acme-email ADDRESS [--root-ca-passphrase-file PATH] [--root-ca-export-dir PATH] [--root-lifecycle] [--check]" >&2
  exit 2
}

valid_domain() {
  local name="$1" label
  [[ ${#name} -le 253 && "${name}" == *.* && "${name}" =~ ^[a-z0-9.-]+$ ]] || return 1
  local IFS=.
  for label in ${name}; do
    [[ "${label}" =~ ^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$ ]] || return 1
  done
  [[ "${name}" != .* && "${name}" != *. && "${name}" != *..* ]]
}

version_seen=false
while (($# > 0)); do
  case "$1" in
    --interactive) INTERACTIVE=true; shift ;;
    --non-interactive) NON_INTERACTIVE=true; shift ;;
    --config)
      (($# >= 2)) || fail "--config requires an existing file"
      [[ -f "$2" && ! -L "$2" ]] || fail "--config requires a regular file, not a symlink"
      CONFIG_FILE="$(realpath -- "$2")"; shift 2 ;;
    --help|-h) usage ;;
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
    --quick)
      [[ "${QUICK}" == false ]] || usage
      QUICK=true
      shift
      ;;
    --controller-domain|--relay-domain|--acme-email|--root-ca-passphrase-file|--root-ca-export-dir)
      (($# >= 2)) || usage
      for quick_argument in ${QUICK_ARGS[@]+"${QUICK_ARGS[@]}"}; do
        [[ "${quick_argument}" != "$1" ]] || usage
      done
      case "$1" in
        --controller-domain) CONTROLLER_DOMAIN="$2" ;;
        --relay-domain) RELAY_DOMAIN="$2" ;;
        --acme-email) ACME_EMAIL="$2" ;;
        --root-ca-passphrase-file|--root-ca-export-dir)
          [[ "$2" =~ ^/[A-Za-z0-9._/-]+$ ]] ||
            fail "root CA paths must be absolute and contain only letters, digits, '.', '_', '-' and '/'"
          ;;
      esac
      QUICK_ARGS+=("$1" "$2")
      shift 2
      ;;
    *)
      usage
      ;;
  esac
done
[[ "${NON_INTERACTIVE}" == false || "${INTERACTIVE}" == false ]] || fail "conflicting installation modes"
[[ "${INTERACTIVE}" == false ]] || install_ui_require_tty

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
  # Quick generates its own configuration and never reads ./install.env.
  [[ "${QUICK}" == false ]] || return 0
  install_env_load "${CONFIG_FILE}" "${INSTALL_ENV_NAMES[@]}"
}

# Read-only Quick preflight. Both names must have the same IPv4 addresses;
# an AAAA record that is not on this host fails, because ACME validation
# prefers IPv6. A NAT or cloud public address cannot be proven locally, so an
# IPv4 address missing from this host only warns. TCP443 and UDP7842 must be
# free unless a Quick installation is being retried.
quick_preflight() {
  local name address tool v4="" first_v4="" ipv6 local_v4 local_v6
  for tool in getent ip ss; do
    command -v "${tool}" >/dev/null 2>&1 || fail "${tool} is required for the Quick preflight"
  done
  local_v4="$(ip -o -4 addr show | awk '{print $4}' | cut -d/ -f1)"
  local_v6="$(ip -o -6 addr show | awk '{print $4}' | cut -d/ -f1)"
  for name in "${CONTROLLER_DOMAIN}" "${RELAY_DOMAIN}"; do
    v4="$(getent ahostsv4 "${name}" | awk '{print $1}' | sort -u | tr '\n' ' ')" || true
    [[ -n "${v4}" ]] || fail "${name} has no IPv4 address; create its A record first"
    [[ -z "${first_v4}" || "${v4}" == "${first_v4}" ]] ||
      fail "${CONTROLLER_DOMAIN} (${first_v4% }) and ${name} (${v4% }) must resolve to the same IPv4 addresses"
    first_v4="${v4}"
    for address in ${v4}; do
      grep -qxF -- "${address}" <<<"${local_v4}" ||
        echo "warning: ${name} resolves to ${address}, which is not on this host; make sure it forwards TCP443 and UDP7842 here"
    done
    ipv6="$(getent ahostsv6 "${name}" | awk '{print $1}' | grep -v '^::ffff:' | sort -u)" || true
    for address in ${ipv6}; do
      grep -qxF -- "${address}" <<<"${local_v6}" ||
        fail "${name} has the AAAA address ${address}, which is not on this host; remove the AAAA record or point it here"
    done
  done
  echo "DNS: ${CONTROLLER_DOMAIN} and ${RELAY_DOMAIN} resolve to ${first_v4% }"
  if [[ -e /etc/ocservia/install.env ]]; then
    echo "ports: not checked; retrying the existing Quick installation"
  else
    [[ -z "$(ss -Hltn 'sport = :443')" ]] || fail "TCP port 443 is already in use"
    [[ -z "$(ss -Hlun 'sport = :7842')" ]] || fail "UDP port 7842 is already in use"
    echo "ports: TCP443 and UDP7842 are free; allow both in any cloud firewall"
  fi
}

# Walk the source root path top-down. Existing components are validated
# (canonical, root/launcher-owned, not group/world-writable); with
# create=true the missing tail is created component by component with
# mode 0700, so the durable runtime root is launcher-owned and private
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
  if [[ "${OCSERV_DEPLOYMENT_MODE:-standalone}" == integrated ]] && (( EUID != 0 )); then
    fail "Integrated requires explicit --root-lifecycle for private Signer custody"
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

prepare_runtime() {
  runtime_fetch "${TARGET}" "${VERSION}"
  echo "verified runtime: ${TARGET}"
}

check_release_installer() {
  [[ -x "${TARGET}/deploy/production/install.sh" && ! -L "${TARGET}/deploy/production/install.sh" ]] ||
    fail "release ${VERSION} at ${TARGET} does not ship the production installer (deploy/production/install.sh is missing or not executable); use a release that does"
  [[ "${QUICK}" == false || ( -x "${TARGET}/deploy/production/quick-install.sh" && ! -L "${TARGET}/deploy/production/quick-install.sh" ) ]] ||
    fail "release ${VERSION} at ${TARGET} does not support --quick (deploy/production/quick-install.sh is missing); use a release that does"
}

require_run_tools() {
  local tool VERSION ID VERSION_ID
  for tool in tar gzip sha256sum find diff sort uniq awk sed mktemp; do
    command -v "${tool}" >/dev/null 2>&1 || fail "${tool} is required"
  done
  command -v curl >/dev/null 2>&1 || command -v wget >/dev/null 2>&1 || fail "curl or wget is required"
  if (( EUID != 0 )); then
    command -v sudo >/dev/null 2>&1 || fail "sudo is required for host preparation"
  fi
  [[ "$(uname -s)" == Linux ]] || fail "Linux is required"
  case "$(uname -m)" in x86_64|aarch64) ;; *) fail "unsupported architecture: $(uname -m)" ;; esac
  # Read OS metadata before any installation or downloads.
  . /etc/os-release
  case "${ID}:${VERSION_ID}" in
    ubuntu:20.04|ubuntu:22.04|ubuntu:24.04|ubuntu:26.04|debian:11|debian:12|debian:13) ;;
    *) fail "unsupported Controller OS: ${ID} ${VERSION_ID}" ;;
  esac
  echo "platform: ${ID} ${VERSION_ID}, $(uname -m)"
}

collect_inputs() {
  local mode=quick auth=local passphrase_file="" export_dir=""
  [[ ! -e "${CONFIG_FILE}" ]] || mode=manual
  install_ui_prompt mode 'Installation preset (quick/manual)' "${mode}"
  case "${mode}" in
    quick)
      QUICK=true
      echo 'Quick: Integrated, bundled PostgreSQL, ACME TLS, Local authentication, root lifecycle.'
      echo 'Manual supports external databases, Standalone and OIDC with operator-provisioned secrets.'
      install_ui_prompt CONTROLLER_DOMAIN 'Controller DNS name'
      install_ui_prompt RELAY_DOMAIN 'Relay DNS name (distinct from Controller)'
      install_ui_prompt ACME_EMAIL 'ACME contact email'
      install_ui_prompt passphrase_file 'Root CA passphrase file (empty: secure prompt during CA generation)'
      install_ui_prompt export_dir 'Root CA encrypted export directory' /root/ocservia-root-ca
      QUICK_ARGS=(--controller-domain "${CONTROLLER_DOMAIN}" --relay-domain "${RELAY_DOMAIN}" --acme-email "${ACME_EMAIL}" --root-ca-export-dir "${export_dir}")
      [[ -z "${passphrase_file}" ]] || QUICK_ARGS+=(--root-ca-passphrase-file "${passphrase_file}")
      ;;
    manual)
      load_config
      install_ui_prompt OCSERV_DEPLOYMENT_MODE 'Deployment (standalone/integrated)' standalone
      install_ui_prompt OCSERV_PUBLIC_HOST 'Controller DNS name'
      install_ui_prompt OCSERV_CONTROLLER_PUBLIC_URL 'Controller HTTPS URL' "https://${OCSERV_PUBLIC_HOST}"
      install_ui_prompt OCSERV_PUBLIC_ORIGIN 'Authentication public origin' "https://${OCSERV_PUBLIC_HOST}"
      install_ui_prompt OCSERV_DATABASE_BACKEND 'Database (postgres/mysql)' postgres
      install_ui_prompt OCSERV_DATABASE_DEPLOYMENT 'Database location (bundled/external)' "$([[ "${OCSERV_DATABASE_BACKEND}" == mysql ]] && echo external || echo bundled)"
      if [[ "${OCSERV_DATABASE_DEPLOYMENT}" == external ]]; then
        install_ui_prompt OCSERV_DATABASE_BACKUP_HOST 'Database backup host'
        install_ui_prompt OCSERV_DATABASE_BACKUP_PORT 'Database backup port' "$([[ "${OCSERV_DATABASE_BACKEND}" == mysql ]] && echo 3306 || echo 5432)"
        install_ui_prompt OCSERV_DATABASE_BACKUP_NAME 'Database name' ocservia
        install_ui_prompt OCSERV_DATABASE_BACKUP_USER 'Database backup user' ocservia_backup
      fi
      if [[ "${OCSERV_DEPLOYMENT_MODE}" == integrated ]]; then
        ROOT_LIFECYCLE=true
        install_ui_prompt OCSERV_RELAY_PUBLIC_HOST 'Relay DNS name'
        install_ui_prompt OCSERV_TLS_MODE 'TLS (manual/acme)' manual
        [[ "${OCSERV_TLS_MODE}" != acme ]] || install_ui_prompt OCSERV_ACME_EMAIL 'ACME contact email'
        install_ui_prompt OCSERV_RELAY_SECRET_DIR 'Protected Relay secret directory'
        install_ui_prompt OCSERV_SIGNER_SECRET_DIR 'Protected Signer secret directory'
        install_ui_prompt OCSERV_SIGNER_STATE_DIR 'Signer state directory'
      else
        install_ui_prompt OCSERV_RELAY_URL_A 'Dedicated Relay HTTPS URL'
        install_ui_prompt OCSERV_CERTIFICATE_SIGNER_URL 'Signer HTTPS URL'
      fi
      [[ -z "${OCSERV_OIDC_ISSUER:-}" ]] || auth=oidc
      [[ -z "${OCSERV_OIDC_ISSUER:-}" || "${OCSERV_LOCAL_AUTH_ENABLED:-false}" != true ]] || auth=both
      install_ui_prompt auth 'Authentication (local/oidc/both)' "${auth}"
      case "${auth}" in
        local) export OCSERV_LOCAL_AUTH_ENABLED=true OCSERV_OIDC_ISSUER="" OCSERV_OIDC_CLIENT_ID="" OCSERV_OIDC_REDIRECT_URL="" ;;
        oidc|both)
          export OCSERV_LOCAL_AUTH_ENABLED=false
          [[ "${auth}" != both ]] || export OCSERV_LOCAL_AUTH_ENABLED=true
          install_ui_prompt OCSERV_OIDC_ISSUER 'OIDC issuer HTTPS URL'
          install_ui_prompt OCSERV_OIDC_CLIENT_ID 'OIDC client ID'
          install_ui_prompt OCSERV_OIDC_REDIRECT_URL 'OIDC redirect URL' "https://${OCSERV_PUBLIC_HOST}/api/v1/auth/callback"
          ;;
        *) fail "authentication must be local, oidc or both" ;;
      esac
      install_ui_prompt OCSERV_CONTROLLER_ENDPOINT_ID 'Controller EndpointID (64 lowercase hex)'
      install_ui_prompt OCSERV_AUDIT_EVENT_KEY_ID 'Audit event key ID' audit-event-v1
      install_ui_prompt OCSERV_SECRET_DIR 'Prepared production secret directory'
      install_ui_prompt OCSERV_BACKUP_DIR 'Protected backup directory' /var/backups/ocservia
      install_ui_prompt OCSERV_CONTROLLER_STATE_ROOT 'Controller persistent state directory' /var/lib/ocservia-controller
      echo 'Manual uses existing protected files; it never generates credentials or identities.'
      ;;
    *) fail "preset must be quick or manual" ;;
  esac
}

validate_manual_inputs() {
  local variable
  [[ "${QUICK}" == false ]] || return 0
  for variable in OCSERV_PUBLIC_HOST OCSERV_CONTROLLER_ENDPOINT_ID OCSERV_AUDIT_EVENT_KEY_ID OCSERV_SECRET_DIR OCSERV_BACKUP_DIR; do
    [[ -n "${!variable:-}" ]] || fail "${variable} is required; provide install.env or export it"
  done
  valid_domain "${OCSERV_PUBLIC_HOST}" || fail "OCSERV_PUBLIC_HOST must be a lowercase DNS name"
  [[ "${OCSERV_CONTROLLER_ENDPOINT_ID}" =~ ^[0-9a-f]{64}$ ]] || fail "Controller EndpointID must be 64 lowercase hex characters"
  [[ -z "${OCSERV_RELAY_URL_B:-}" ]] || fail "only one dedicated Relay is supported"
  case "${OCSERV_DATABASE_BACKEND:-postgres}:${OCSERV_DATABASE_DEPLOYMENT:-bundled}" in
    postgres:bundled|postgres:external|mysql:external) ;; *) fail "unsupported database topology" ;;
  esac
  if [[ "${OCSERV_DATABASE_DEPLOYMENT:-bundled}" == external ]]; then
    [[ -n "${OCSERV_DATABASE_BACKUP_HOST:-}" ]] || fail "OCSERV_DATABASE_BACKUP_HOST is required"
  fi
  case "${OCSERV_DEPLOYMENT_MODE:-standalone}" in
    standalone)
      [[ "${OCSERV_RELAY_URL_A:-}" == https://* && "${OCSERV_CERTIFICATE_SIGNER_URL:-}" == https://* ]] || fail "HTTPS Relay and Signer URLs are required" ;;
    integrated)
      valid_domain "${OCSERV_RELAY_PUBLIC_HOST:-}" || fail "Integrated requires a Relay DNS name"
      [[ "${OCSERV_RELAY_PUBLIC_HOST}" != "${OCSERV_PUBLIC_HOST}" ]] || fail "Integrated requires distinct Controller and Relay DNS names"
      for variable in OCSERV_RELAY_SECRET_DIR OCSERV_SIGNER_SECRET_DIR OCSERV_SIGNER_STATE_DIR; do
        [[ "${!variable:-}" == /* ]] || fail "${variable} must be an absolute directory"
      done ;;
    *) fail "deployment must be standalone or integrated" ;;
  esac
  if [[ -n "${OCSERV_OIDC_ISSUER:-}${OCSERV_OIDC_CLIENT_ID:-}${OCSERV_OIDC_REDIRECT_URL:-}" ]]; then
    [[ "${OCSERV_OIDC_ISSUER:-}" == https://* && -n "${OCSERV_OIDC_CLIENT_ID:-}" && "${OCSERV_OIDC_REDIRECT_URL:-}" == https://* ]] || fail "complete OIDC configuration is required"
  else
    [[ "${OCSERV_LOCAL_AUTH_ENABLED:-false}" == true ]] || fail "enable Local authentication or configure OIDC"
  fi
  [[ "${OCSERV_SECRET_DIR}" == /* && -d "${OCSERV_SECRET_DIR}" && ! -L "${OCSERV_SECRET_DIR}" && "${OCSERV_BACKUP_DIR}" == /* ]] || fail "prepare an absolute secret directory and backup path"
}

hand_off() {
  local installer="${TARGET}/deploy/production/install.sh"
  local -a install_args=()
  [[ "${NON_INTERACTIVE}" == false ]] || install_args+=(--non-interactive)
  # Values already loaded under the invoking user's privileges.
  install_args+=(--config "${CONFIG_FILE}")
  if [[ "${QUICK}" == true ]]; then
    echo "handing off to the Quick installer: ${TARGET}/deploy/production/quick-install.sh"
    [[ "${NON_INTERACTIVE}" == false ]] || QUICK_ARGS+=(--non-interactive)
    exec "${TARGET}/deploy/production/quick-install.sh" "${QUICK_ARGS[@]}"
  fi
  if [[ "${ROOT_LIFECYCLE}" == true ]]; then
    # The installer performs its own controlled sudo re-exec for the root
    # lifecycle and resolves install.env from the invoking directory,
    # which is still ${CONFIG_ROOT}: this script never changed PWD.
    echo "handing off to the production installer: ${installer} --root-lifecycle (configuration stays in ${CONFIG_ROOT})"
    # An operator shell must never pre-set the installer's internal
    # install.env resolution marker: it would suppress the launcher-side
    # install.env parsing this bootstrap hands off to.
    unset OCSERV_INSTALL_ENV_RESOLVED
    exec "${installer}" "${install_args[@]}" --root-lifecycle
  fi
  echo "handing off to the production installer: ${installer} (configuration stays in ${CONFIG_ROOT})"
  unset OCSERV_INSTALL_ENV_RESOLVED
  exec "${installer}" "${install_args[@]}"
}

require_run_tools
[[ "${INTERACTIVE}" == false ]] || collect_inputs

if [[ "${QUICK}" == true ]]; then
  [[ -n "${CONTROLLER_DOMAIN}" && -n "${RELAY_DOMAIN}" && -n "${ACME_EMAIL}" ]] || usage
  valid_domain "${CONTROLLER_DOMAIN}" || fail "--controller-domain must be a lowercase DNS name"
  valid_domain "${RELAY_DOMAIN}" || fail "--relay-domain must be a lowercase DNS name"
  [[ "${CONTROLLER_DOMAIN}" != "${RELAY_DOMAIN}" ]] || fail "the Controller and Relay domains must differ"
  if [[ ${#ACME_EMAIL} -gt 254 || ! "${ACME_EMAIL}" =~ ^[A-Za-z0-9._%+-]+@(.+)$ ]] || ! valid_domain "${BASH_REMATCH[1]}"; then
    fail "--acme-email must be a plain email address"
  fi
  # Integrated deployment requires the explicit root lifecycle.
  ROOT_LIFECYCLE=true
elif ((${#QUICK_ARGS[@]} > 0)); then
  usage
fi
[[ "${VERSION}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-rc[.][1-9][0-9]*)?$ ]] ||
  fail "unsupported version '${VERSION}': an exact vX.Y.Z or vX.Y.Z-rc.N release tag is required (latest, branches, commits, and other pre-releases are not accepted)"

load_config
validate_manual_inputs
if [[ "${QUICK}" == true && "${NON_INTERACTIVE}" == true && " ${QUICK_ARGS[*]} " != *" --root-ca-passphrase-file "* ]]; then
  fail "--non-interactive Quick requires --root-ca-passphrase-file"
fi
resolve_source_root
select_lifecycle
[[ "${QUICK}" == false ]] || quick_preflight
walk_source_root false
if [[ "${CHECK_ONLY}" == true ]]; then
  check_root="$(mktemp -d)"
  trap 'rm -rf -- "${check_root}"' EXIT
  runtime_fetch "${check_root}/runtime" "${VERSION}"
  TARGET="${check_root}/runtime"
  check_release_installer
  echo "check passed: inputs, platform and published runtime verified; no host state modified"
  exit 0
fi
if [[ "${INTERACTIVE}" == true ]]; then
  echo "Release: ${VERSION}; runtime: ${TARGET}; root lifecycle: ${ROOT_LIFECYCLE}; Quick: ${QUICK}"
  if [[ "${QUICK}" == false ]]; then
    echo "Controller: ${OCSERV_PUBLIC_HOST}; deployment: ${OCSERV_DEPLOYMENT_MODE:-standalone}; database: ${OCSERV_DATABASE_BACKEND:-postgres}/${OCSERV_DATABASE_DEPLOYMENT:-bundled}"
    echo "Local login: ${OCSERV_LOCAL_AUTH_ENABLED:-false}; OIDC: ${OCSERV_OIDC_ISSUER:-disabled}"
    echo "Configuration will be saved to ${CONFIG_FILE} (protected file paths only)."
  fi
  echo 'Existing deployments are never upgraded by this installer.'
  install_ui_confirm
  [[ "${QUICK}" == true ]] || install_ui_save "${CONFIG_FILE}" "${INSTALL_ENV_NAMES[@]}"
fi
walk_source_root true
prepare_runtime
check_release_installer
hand_off
