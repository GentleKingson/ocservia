#!/usr/bin/env bash
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
# shellcheck source=deploy/lib/install-env.sh
source "${ROOT}/deploy/lib/install-env.sh"

fail() {
  echo "controller install: $1" >&2
  exit 1
}

resolve_release_identity() {
  local tag matching=()
  if [[ -e "${ROOT}/RELEASE" ]]; then
    # shellcheck source=deploy/lib/controller-runtime.sh
    source "${ROOT}/deploy/lib/controller-runtime.sh"
    read -r RELEASE_TAG RELEASE_COMMIT <"${ROOT}/RELEASE"
    runtime_identity "${ROOT}" "${RELEASE_TAG}" "${RELEASE_COMMIT}"
    echo "release identity: ${RELEASE_TAG} (${RELEASE_COMMIT})"
    return
  fi
  command -v git >/dev/null 2>&1 ||
    fail "git is required to identify the release checkout"
  RELEASE_COMMIT="$(git -C "${ROOT}" rev-parse HEAD 2>/dev/null)" ||
    fail "${ROOT} is not a Git checkout; install from a clean checkout of an exact vX.Y.Z release tag"
  while IFS= read -r tag; do
    [[ "${tag}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-rc[.][1-9][0-9]*)?$ ]] && matching+=("${tag}")
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
  if (( EUID != 0 )) && ! command -v docker >/dev/null 2>&1; then
    fail "no Docker client is installed, so the host bootstrap would install Docker from scratch; a fresh Docker installation grants no non-root daemon access and this installer never modifies the Docker permission model — run 'deploy/production/install.sh --root-lifecycle' for a deliberate root Controller lifecycle on this fresh host, or install Docker separately and deliberately grant this launcher Docker daemon access per Docker's official post-install steps, then rerun install.sh as the launcher"
  fi
}

run_host_bootstrap() {
  local sudo_env=() args=(install)
  if [[ -n "${OCSERV_CONTROLLER_STATE_ROOT:-}" ]]; then
    sudo_env+=("OCSERV_CONTROLLER_STATE_ROOT=${OCSERV_CONTROLLER_STATE_ROOT}")
  elif [[ -n "${OCSERV_CONTROLLER_STATE_DIR:-}" ]]; then
    sudo_env+=("OCSERV_CONTROLLER_STATE_DIR=${OCSERV_CONTROLLER_STATE_DIR}")
  fi
  if [[ -n "${OCSERV_BACKUP_DIR:-}" ]]; then
    args+=(--backup-dir "${OCSERV_BACKUP_DIR}")
  fi
  if (( EUID == 0 )); then
    env "${sudo_env[@]+"${sudo_env[@]}"}" "${BOOTSTRAP}" "${args[@]}"
  else
    sudo "${sudo_args[@]}" env "${sudo_env[@]+"${sudo_env[@]}"}" "${BOOTSTRAP}" "${args[@]}"
  fi
}

download_release_bundle() {
  BUNDLE_DIR="${STATE_ROOT}/release-bundles/${RELEASE_TAG}"
  [[ ! -L "${STATE_ROOT}/release-bundles" && ! -L "${BUNDLE_DIR}" ]] ||
    fail "release bundle directory must not be a symlink"
  mkdir -p -- "${BUNDLE_DIR}"
  release_file="${BUNDLE_DIR}/controller-release-${ARCH_WORD}.json"
  local temporary
  temporary="$(mktemp "${BUNDLE_DIR}/.manifest.XXXXXX")"
  curl -fsSL --proto '=https' --tlsv1.2 --output "${temporary}" \
    "${DOWNLOAD_BASE}/${RELEASE_TAG}/controller-release-${ARCH_WORD}.json" || {
    rm -f -- "${temporary}"
    fail "cannot download release manifest"
  }
  mv -fT -- "${temporary}" "${release_file}"
}

quick_install() {
  CONFIG_DIR=/etc/ocservia
  CONFIG="${CONFIG_DIR}/install.env"
  CREDENTIALS_DIR=/root/ocservia-initial-credentials
  STATE_ROOT=/var/lib/ocservia-controller
  HEADER="# ocservia Quick install configuration (generated; keep unchanged)"

  note() { echo "quick install: $*"; }

  controller_domain="" relay_domain="" acme_email="" release_file=""
  materials_args=()
  non_interactive=false
  sudo_args=()
  original_args=("$@")
  while (($#)); do
    if [[ "$1" == --non-interactive ]]; then
      non_interactive=true
      sudo_args=(-n)
      materials_args+=(--non-interactive)
      shift
      continue
    fi
    (($# >= 2)) || fail "missing value for $1"
    case "$1" in
      --controller-domain) controller_domain="$2" ;;
      --relay-domain) relay_domain="$2" ;;
      --acme-email) acme_email="$2" ;;
      --root-ca-passphrase-file) materials_args+=("$1" "$2" --non-interactive) ;;
      --root-ca-export-dir) materials_args+=("$1" "$2") ;;
      --release-file) release_file="$2" ;;
      *) fail "unknown argument: $1" ;;
    esac
    shift 2
  done
  [[ -n "${controller_domain}" && -n "${acme_email}" ]] ||
    fail "--controller-domain and --acme-email are required"
  [[ -n "${relay_domain}" ]] || relay_domain="${controller_domain}"
  [[ -z "${release_file}" || ("${release_file}" == /* && -f "${release_file}") ]] ||
    fail "--release-file must be an absolute path to a release manifest"

  if [[ "${non_interactive}" == true && " ${materials_args[*]} " != *" --root-ca-passphrase-file "* ]]; then
    fail "--non-interactive requires --root-ca-passphrase-file"
  fi
  if ((EUID != 0)); then
    command -v sudo >/dev/null 2>&1 || fail "run as root or install sudo"
    exec sudo "${sudo_args[@]}" env ${OCSERV_ACME_DIRECTORY_URL:+"OCSERV_ACME_DIRECTORY_URL=${OCSERV_ACME_DIRECTORY_URL}"} \
      ${OCSERV_ACME_CA_FILE:+"OCSERV_ACME_CA_FILE=${OCSERV_ACME_CA_FILE}"} "$0" --quick "${original_args[@]}"
  fi

  unset SUDO_USER
  acme_directory="${OCSERV_ACME_DIRECTORY_URL:-}" acme_ca="${OCSERV_ACME_CA_FILE:-}"
  while IFS= read -r variable; do
    [[ "${variable}" != OCSERV_* ]] || unset "${variable}"
  done < <(compgen -e)

  config_keys=(OCSERV_DEPLOYMENT_MODE OCSERV_DATABASE_BACKEND OCSERV_DATABASE_DEPLOYMENT OCSERV_TLS_MODE
    OCSERV_ACME_EMAIL OCSERV_ACME_DIRECTORY_URL OCSERV_ACME_CA_FILE OCSERV_PUBLIC_HOST OCSERV_RELAY_PUBLIC_HOST
    OCSERV_LOCAL_AUTH_ENABLED OCSERV_SECRET_DIR OCSERV_RELAY_SECRET_DIR OCSERV_SIGNER_SECRET_DIR
    OCSERV_SIGNER_STATE_DIR OCSERV_BACKUP_DIR OCSERV_AUDIT_EVENT_KEY_ID OCSERV_CONTROLLER_ENDPOINT_ID)

  if [[ -e "${CONFIG}" || -L "${CONFIG}" ]]; then
    [[ -f "${CONFIG}" && ! -L "${CONFIG}" && "$(head -n 1 -- "${CONFIG}")" == "${HEADER}" ]] ||
      fail "${CONFIG} was not generated by the Quick install; use the manual installation"
    for line in "OCSERV_PUBLIC_HOST=${controller_domain}" "OCSERV_RELAY_PUBLIC_HOST=${relay_domain}" \
      "OCSERV_ACME_EMAIL=${acme_email}"; do
      grep -qxF -- "${line}" "${CONFIG}" || fail "${CONFIG} records different domains or email; rerun with the original values"
    done
    note "continuing the Quick installation recorded in ${CONFIG}"
  elif [[ -e "${STATE_ROOT}/release-history.json" || -e "${STATE_ROOT}/deployment-profile.json" || -e "${STATE_ROOT}/current-release.json" ]]; then
    fail "a Controller deployment already exists in ${STATE_ROOT}; Quick only installs a new host"
  fi

  secret_dir="${CONFIG_DIR}/secrets"
  relay_dir="${CONFIG_DIR}/relay"
  signer_dir="${CONFIG_DIR}/signer"
  signer_state=/var/lib/ocservia-signer
  backup_dir=/var/backups/ocservia

  "${ROOT}/deploy/production/bootstrap-host.sh" install --backup-dir "${backup_dir}"

  install -d -o 0 -g 0 -m 0755 -- "${CONFIG_DIR}"
  [[ -d "${relay_dir}" ]] || install -d -o 0 -g 0 -m 0700 -- "${relay_dir}"
  OCSERV_SECRET_DIR="${secret_dir}" OCSERV_SIGNER_SECRET_DIR="${signer_dir}" OCSERV_SIGNER_STATE_DIR="${signer_state}" \
    generate_materials ${materials_args[@]+"${materials_args[@]}"}

  if [[ ! -e "${CONFIG}" ]]; then
    endpoint_id="$(jq -er '.controller_endpoint_id' "${secret_dir}/quick-install.json")"
    staged="$(mktemp "${CONFIG_DIR}/.install.env.XXXXXX")"
    {
      printf '%s\n' "${HEADER}"
      printf '%s\n' OCSERV_DEPLOYMENT_MODE=integrated \
        OCSERV_DATABASE_DEPLOYMENT=bundled OCSERV_TLS_MODE=acme "OCSERV_ACME_EMAIL=${acme_email}"
      [[ -z "${acme_directory}" ]] || printf 'OCSERV_ACME_DIRECTORY_URL=%s\n' "${acme_directory}"
      [[ -z "${acme_ca}" ]] || printf 'OCSERV_ACME_CA_FILE=%s\n' "${acme_ca}"
      printf '%s\n' "OCSERV_PUBLIC_HOST=${controller_domain}" "OCSERV_RELAY_PUBLIC_HOST=${relay_domain}" \
        OCSERV_LOCAL_AUTH_ENABLED=true "OCSERV_SECRET_DIR=${secret_dir}" "OCSERV_RELAY_SECRET_DIR=${relay_dir}" \
        "OCSERV_SIGNER_SECRET_DIR=${signer_dir}" "OCSERV_SIGNER_STATE_DIR=${signer_state}" \
        "OCSERV_BACKUP_DIR=${backup_dir}" "OCSERV_AUDIT_EVENT_KEY_ID=quick-$(date -u +%Y%m%d)" \
        "OCSERV_CONTROLLER_ENDPOINT_ID=${endpoint_id}"
    } >"${staged}"
    mv -fT -- "${staged}" "${CONFIG}"
    note "configuration written: ${CONFIG}"
  fi

  install_env_load "${CONFIG}" "${config_keys[@]}"

  history="${STATE_ROOT}/release-history.json"
  if [[ ! -e "${history}" && ! -e "${STATE_ROOT}/current-release.json" ]]; then
    if [[ -z "${release_file}" ]]; then
      resolve_release_identity
      resolve_architecture
      download_release_bundle
    fi
    "${CONTROLLER}" install --release-file "${release_file}"
  fi
  [[ -e "${history}" ]] || history="${STATE_ROOT}/current-release.json"

  for role in gateway control transport backup postgres otel edge relay signer; do
    variable="OCSERV_${role^^}_IMAGE"
    printf -v "${variable}" '%s' "$(jq -er --arg role "${role}" '(.current // .).images[$role]' "${history}")"
    export "${variable?}"
  done
  compose() { "${ROOT}/deploy/production/compose.sh" "$@"; }

  if [[ -e "${CREDENTIALS_DIR}/complete" ]]; then
    note "initial Local administrator already exists"
  else
    workspace_id="$(compose run --rm --no-deps -T migrate --provision-management-workspace |
      jq -Rr 'fromjson? | select(.msg == "management workspace ready") | .workspace_id')"
    [[ "${workspace_id}" =~ ^[0-9a-f-]{36}$ ]] || fail "the management workspace was not provisioned"

    [[ -d "${CREDENTIALS_DIR}" ]] || install -d -o 0 -g 0 -m 0700 -- "${CREDENTIALS_DIR}"
    file="${CREDENTIALS_DIR}/admin.txt"
    if [[ ! -e "${file}" ]]; then
      password="$(openssl rand -base64 6)"
      staged="$(mktemp "${CREDENTIALS_DIR}/.admin.XXXXXX")"
      printf 'URL: https://%s\nUsername: admin\nPassword: %s\n' \
        "${controller_domain}" "${password}" >"${staged}"
      unset password
      chmod 0400 -- "${staged}"
      mv -fT -- "${staged}" "${file}"
    fi
    username="$(sed -n 's/^Username: //p' "${file}")"
    trap 'rm -f -- "${secret_dir}"/local-bootstrap-admin-password' EXIT
    sed -n 's/^Password: //p' "${file}" |
      install -o 65534 -g 65532 -m 0400 /dev/stdin "${secret_dir}/local-bootstrap-admin-password"
    status=0
    output="$(compose run --rm --no-deps -T \
      -e "OCSERV_LOCAL_BOOTSTRAP_USERNAME=${username}" \
      -e "OCSERV_LOCAL_BOOTSTRAP_WORKSPACE_ID=${workspace_id}" \
      -e OCSERV_LOCAL_BOOTSTRAP_PASSWORD_FILE=/run/secrets/local-bootstrap-password \
      -v "${secret_dir}/local-bootstrap-admin-password:/run/secrets/local-bootstrap-password:ro" \
      control-plane --bootstrap-local-admin)" || status=$?
    printf '%s\n' "${output}"
    if ((status != 0)); then
      jq -Rr 'fromjson? | .error // empty' <<<"${output}" | grep -qxF "Local authentication is already initialized" ||
        fail "Local bootstrap was rejected"
      note "Local authentication was already initialized by an earlier run"
    fi
    : >"${CREDENTIALS_DIR}/complete"
  fi

  cat <<EOF
Quick install complete: https://${controller_domain}
Initial Local administrator credentials (root-only file):
  ${CREDENTIALS_DIR}/admin.txt
Change the initial password after the first login.
EOF
}

if [[ "${1:-}" == --quick ]]; then
  shift
  # shellcheck source=deploy/lib/quick-materials.sh
  source "${ROOT}/deploy/lib/quick-materials.sh"
  quick_install "$@"
  exit
fi

CONFIG_FILE="${PWD}/install.env"
NON_INTERACTIVE=false
while (($#)); do
  case "$1" in
    --root-lifecycle) ROOT_LIFECYCLE=true; shift ;;
    --non-interactive) NON_INTERACTIVE=true; shift ;;
    --config) (($# >= 2)) || fail "--config requires a path"; CONFIG_FILE="$2"; shift 2 ;;
    *) echo "usage: install.sh [--root-lifecycle] [--non-interactive] [--config PATH]" >&2; exit 2 ;;
  esac
done
sudo_args=()
[[ "${NON_INTERACTIVE}" == false ]] || sudo_args+=(-n)

if [[ -z "${OCSERV_INSTALL_ENV_RESOLVED:-}" ]]; then
  install_env_load "${CONFIG_FILE}" "${CONTROLLER_INSTALL_ENV_NAMES[@]}"
fi
case "${OCSERV_DATABASE_BACKEND:-postgres}:${OCSERV_DATABASE_DEPLOYMENT:-bundled}" in
  postgres:bundled|postgres:external) ;;
  *) fail 'unsupported database configuration; only OCSERV_DATABASE_BACKEND=postgres with OCSERV_DATABASE_DEPLOYMENT=bundled or external is supported' ;;
esac
STATE_ROOT="${OCSERV_CONTROLLER_STATE_ROOT:-${OCSERV_CONTROLLER_STATE_DIR:-/var/lib/ocservia-controller}}"
if [[ "${OCSERV_DEPLOYMENT_MODE:-standalone}" == integrated && "${ROOT_LIFECYCLE}" != true ]] && (( EUID != 0 )); then
  fail "Integrated requires --root-lifecycle for private Signer custody"
fi

forward_root_lifecycle() {
  local variable allowed
  local -a reexec_args=()
  [[ "${NON_INTERACTIVE}" == false ]] || reexec_args+=(--non-interactive)
  local -a forwarded_environment=()
  while IFS= read -r variable; do
    for allowed in "${CONTROLLER_INSTALL_ENV_NAMES[@]}"; do
      [[ "${variable}" == "${allowed}" ]] || continue
      forwarded_environment+=("${variable}=${!variable}")
      break
    done
  done < <(compgen -e)
  command -v sudo >/dev/null 2>&1 ||
    fail "sudo is required for --root-lifecycle when the installer is not already running as root"
  exec sudo "${sudo_args[@]}" env "${forwarded_environment[@]+"${forwarded_environment[@]}"}" \
    OCSERV_INSTALL_ENV_RESOLVED=1 \
    "${ROOT}/deploy/production/install.sh" --root-lifecycle "${reexec_args[@]}"
}

if [[ "${ROOT_LIFECYCLE}" == true ]]; then
  (( EUID == 0 )) || forward_root_lifecycle
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


resolve_release_identity
resolve_architecture
verify_fresh_host_launcher_path
run_host_bootstrap
download_release_bundle
exec "${CONTROLLER}" install --release-file "${release_file}"
