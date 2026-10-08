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
# - No deployment activation, no docker compose, no Docker installation,
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
#   ./controller-bootstrap.sh --version vX.Y.Z
#   ./controller-bootstrap.sh --version vX.Y.Z --root-lifecycle
#   ./controller-bootstrap.sh --version vX.Y.Z --check
#   ./controller-bootstrap.sh --version vX.Y.Z --quick --controller-domain NAME \
#     --relay-domain NAME --acme-email ADDRESS [--root-ca-passphrase-file PATH]
#     [--root-ca-export-dir PATH] [--check]
#
# --quick ignores ./install.env, implies --root-lifecycle, checks DNS and the
# public ports read-only, and hands off to the release's
# deploy/production/quick-install.sh, which generates the configuration.
#
# The version must be an explicit exact vX.Y.Z release tag; latest,
# branches, commits, and pre-releases are not accepted.
set -euo pipefail
umask 077

REPOSITORY_URL="https://github.com/GentleKingson/ocservia"
CONFIG_ROOT="${PWD}"
VERSION=""
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

# Embedded install.env loader: the Stage-1 contract copy of
# deploy/lib/install-env.sh, kept functionally identical to it. This bootstrap
# is a single self-contained Release asset, so it cannot source repository
# siblings before it clones the requested release; any loader contract change
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
[[ "${version_seen}" == true ]] || usage
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
  install_env_load "${CONFIG_ROOT}/install.env" "${INSTALL_ENV_NAMES[@]}"
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
    [[ "${tag}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-rc[.][1-9][0-9]*)?$ ]] && matching+=("${tag}")
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
  # Public checkout files are bind-mounted into non-root containers. Keep the
  # claimed root private and limit the readable umask to the clone itself.
  if ! (umask 022; git clone --quiet --branch "${VERSION}" --single-branch --depth 1 \
    "${REPOSITORY_URL}" "${TARGET}"); then
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
  [[ "${QUICK}" == false || ( -x "${TARGET}/deploy/production/quick-install.sh" && ! -L "${TARGET}/deploy/production/quick-install.sh" ) ]] ||
    fail "release ${VERSION} at ${TARGET} does not support --quick (deploy/production/quick-install.sh is missing); use a release that does"
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
  local url path=deploy/production/install.sh
  [[ "${QUICK}" == false ]] || path=deploy/production/quick-install.sh
  [[ "${REPOSITORY_URL}" == https://github.com/* ]] ||
    fail "cannot probe release content for repository ${REPOSITORY_URL}"
  url="https://raw.githubusercontent.com/${REPOSITORY_URL#https://github.com/}/${VERSION}/${path}"
  if ! curl -fsSLI -o /dev/null --proto '=https' --tlsv1.2 "${url}" 2>/dev/null; then
    fail "release ${VERSION} does not appear to ship ${path}; this bootstrap hands off only to releases that do"
  fi
  echo "release installer: ${path} is published for ${VERSION}"
}

run_check() {
  local tag_ref
  command -v git >/dev/null 2>&1 || fail "git is required to inspect the release tag"
  command -v curl >/dev/null 2>&1 ||
    fail "curl is required to check the release and download the release bundle"
  if (( EUID != 0 )); then
    command -v sudo >/dev/null 2>&1 ||
      fail "sudo is required for a non-root launcher (the production installer invokes it for the host bootstrap and the root lifecycle)"
  fi
  load_config
  walk_source_root false
  echo "version: ${VERSION} (exact vX.Y.Z release tag)"
  echo "configuration directory: ${CONFIG_ROOT}"
  echo "source root: ${SOURCE_ROOT}"
  select_lifecycle
  [[ "${QUICK}" == false ]] || quick_preflight
  tag_ref="$(git ls-remote "${REPOSITORY_URL}" "refs/tags/${VERSION}")"
  [[ -n "${tag_ref}" ]] ||
    fail "release tag ${VERSION} was not found on ${REPOSITORY_URL}; check the published releases"
  echo "release tag: ${VERSION} is reachable on ${REPOSITORY_URL}"
  check_release_installer_published
  echo "check passed: no host state was modified; rerun without --check to bootstrap"
}

hand_off() {
  local installer="${TARGET}/deploy/production/install.sh"
  if [[ "${QUICK}" == true ]]; then
    echo "handing off to the Quick installer: ${TARGET}/deploy/production/quick-install.sh"
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
[[ "${QUICK}" == false ]] || quick_preflight
walk_source_root true
prepare_checkout
check_release_installer
hand_off
