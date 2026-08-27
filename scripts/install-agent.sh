#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DESTDIR="${DESTDIR:-}"
PREFIX="${PREFIX:-/usr}"
SYSCONFDIR="${SYSCONFDIR:-/etc}"
STATE_DIR="${STATE_DIR:-/var/lib/ocservia-agent}"
PRIVD_STATE_DIR="${PRIVD_STATE_DIR:-/var/lib/ocservia-privd}"
AGENT_UID="${AGENT_UID:-997}"
AGENT_GID="${AGENT_GID:-997}"
INSTALL_PRODUCTION_RELAYS="${INSTALL_PRODUCTION_RELAYS:-false}"

validate_verified_package_source() {
  local trusted_root="${DESTDIR}/var/lib/ocservia-upgrade/package-staging"
  local current="" relative component uid mode marker archive_hash package_name source
  local -a components=()

  case "${ROOT}" in
    "${trusted_root}"/ocservia-agent-package.*/extracted/ocservia-agent-*) ;;
    *)
      echo "install-agent.sh must run from the root-owned verified package staging hierarchy" >&2
      exit 1
      ;;
  esac
  if [[ -n "${DESTDIR}" ]]; then
    if [[ "${DESTDIR}" != /* || "${DESTDIR}" == "/" || "${DESTDIR}" == */ || ! -d "${DESTDIR}" || -L "${DESTDIR}" || \
          "$(stat -c '%u:%g:%a' -- "${DESTDIR}")" != "0:0:700" ]]; then
      echo "DESTDIR must be an absolute root:root mode 0700 staging root" >&2
      exit 2
    fi
    current="${DESTDIR}"
    relative="${ROOT#"${DESTDIR}"}"
  else
    relative="${ROOT}"
  fi
  IFS='/' read -r -a components <<<"${relative#/}"
  for component in "${components[@]}"; do
    [[ -n "${component}" ]] || continue
    current="${current}/${component}"
    if [[ ! -d "${current}" || -L "${current}" ]]; then
      echo "verified package ancestry contains a non-directory or symlink" >&2
      exit 1
    fi
    read -r uid mode < <(stat -c '%u %a' -- "${current}")
    if [[ "${uid}" != 0 ]] || (( (8#${mode} & 8#022) != 0 )); then
      echo "verified package ancestry must be root-owned and not group/world writable" >&2
      exit 1
    fi
  done
  marker="${ROOT}/.ocservia-package-verified"
  if [[ ! -f "${marker}" || -L "${marker}" || "$(stat -c '%u:%g:%a:%h' -- "${marker}")" != "0:0:600:1" ]]; then
    echo "verified package marker is missing or unsafe" >&2
    exit 1
  fi
  archive_hash="$(sed -n 's/^archive_sha256=//p' "${marker}")"
  package_name="$(sed -n 's/^package=//p' "${marker}")"
  if [[ "$(sed -n 's/^version=//p' "${marker}")" != 1 || ! "${archive_hash}" =~ ^[0-9a-f]{64}$ || \
        "${package_name}" != "$(basename -- "${ROOT}")" || "$(wc -l <"${marker}")" -ne 3 ]]; then
    echo "verified package marker is malformed" >&2
    exit 1
  fi
  for source in \
    "${ROOT}/rust/target/release/ocservia-agent" \
    "${ROOT}/rust/target/release/ocservia-privd" \
    "${ROOT}/rust/target/release/ocservia-upgrader" \
    "${ROOT}/scripts/install-agent.sh" \
    "${ROOT}/scripts/upgrade-agent.sh" \
    "${ROOT}/scripts/rollback-agent.sh" \
    "${ROOT}/scripts/uninstall-agent.sh" \
    "${ROOT}/scripts/verify-agent-package.sh" \
    "${ROOT}/deploy/systemd/agent.env.example" \
    "${ROOT}/deploy/systemd/ocservia-agent.service" \
    "${ROOT}/deploy/systemd/ocservia-privd.service" \
    "${ROOT}/deploy/systemd/ocservia-upgrader@.service" \
    "${ROOT}/deploy/production/systemd/ocservia-agent-relays.conf" \
    "${ROOT}/deploy/production/systemd/relays.env.example"; do
    if [[ ! -f "${source}" || -L "${source}" || "$(stat -c '%u:%g:%h' -- "${source}")" != "0:0:1" ]] || \
      (( (8#$(stat -c '%a' -- "${source}") & 8#022) != 0 )); then
      echo "verified package source has unsafe type, ownership, mode, or link count" >&2
      exit 1
    fi
  done
}

ensure_root_ancestry() {
  local path="$1" current="" relative component uid mode
  local -a components=()
  if [[ -n "${DESTDIR}" ]]; then
    case "${path}" in
      "${DESTDIR}"|"${DESTDIR}"/*) ;;
      *) echo "install destination escapes DESTDIR" >&2; exit 1 ;;
    esac
    current="${DESTDIR}"
    relative="${path#"${DESTDIR}"}"
    read -r uid mode < <(stat -c '%u %a' -- "${current}")
    if [[ "${uid}" != 0 ]] || (( (8#${mode} & 8#022) != 0 )); then
      echo "DESTDIR must remain root-owned and not group/world writable" >&2
      exit 1
    fi
  else
    relative="${path}"
  fi
  IFS='/' read -r -a components <<<"${relative#/}"
  for component in "${components[@]}"; do
    [[ -n "${component}" ]] || continue
    current="${current}/${component}"
    if [[ -e "${current}" || -L "${current}" ]]; then
      if [[ ! -d "${current}" || -L "${current}" ]]; then
        echo "install destination ancestry contains a non-directory or symlink: ${current}" >&2
        exit 1
      fi
      read -r uid mode < <(stat -c '%u %a' -- "${current}")
      if [[ "${uid}" != 0 ]] || (( (8#${mode} & 8#022) != 0 )); then
        echo "install destination ancestry must be root-owned and not group/world writable: ${current}" >&2
        exit 1
      fi
    else
      install -d -o root -g root -m 0755 -- "${current}"
    fi
  done
}

ensure_root_directory() {
  local path="$1" group="$2" expected_gid="$3" mode="$4" uid actual_group actual_mode
  ensure_root_ancestry "$(dirname -- "${path}")"
  if [[ -e "${path}" || -L "${path}" ]]; then
    if [[ ! -d "${path}" || -L "${path}" ]]; then
      echo "install destination must be a real directory: ${path}" >&2
      exit 1
    fi
    read -r uid actual_group actual_mode < <(stat -c '%u %g %a' -- "${path}")
    if [[ "${uid}" != 0 || "${actual_group}" != "${expected_gid}" ]] || \
      (( (8#${actual_mode} & 8#022) != 0 )); then
      echo "install destination directory has unsafe ownership or mode: ${path}" >&2
      exit 1
    fi
  else
    install -d -o root -g "${group}" -m "${mode}" -- "${path}"
  fi
  chown root:"${group}" -- "${path}"
  chmod "${mode}" -- "${path}"
}

ensure_agent_state() {
  local state="${DESTDIR}${STATE_DIR}" identity="${DESTDIR}${STATE_DIR}/identity"
  local uid gid mode
  ensure_root_ancestry "$(dirname -- "${state}")"
  if [[ -e "${state}" || -L "${state}" ]]; then
    if [[ ! -d "${state}" || -L "${state}" || ! -d "${identity}" || -L "${identity}" ]]; then
      echo "existing Agent state and identity must be real directories" >&2
      exit 1
    fi
    for path in "${state}" "${identity}"; do
      read -r uid gid mode < <(stat -c '%u %g %a' -- "${path}")
      if [[ "${uid}:${gid}:${mode}" != "${AGENT_UID}:${AGENT_GID}:700" ]]; then
        echo "existing Agent state has unsafe owner, group, or mode: ${path}" >&2
        exit 1
      fi
    done
    return
  fi
  install -d -o root -g root -m 0700 -- "${state}" "${identity}"
  chown "${agent_owner}:${agent_group}" -- "${identity}"
  chown "${agent_owner}:${agent_group}" -- "${state}"
}

validate_absolute_target() {
  local name="$1" path="$2"
  if [[ "${path}" != /* || "${path}" == / || "${path}" == */ ]]; then
    echo "${name} must be a non-root absolute path without a trailing slash" >&2
    exit 2
  fi
  case "/${path#/}/" in
    *//*|*/./*|*/../*)
      echo "${name} must not contain empty, dot, or parent path components" >&2
      exit 2
      ;;
  esac
}

if [[ ${EUID} -ne 0 ]]; then
  echo "install-agent.sh must run as root" >&2
  exit 1
fi
validate_verified_package_source

validate_absolute_target PREFIX "${PREFIX}"
validate_absolute_target SYSCONFDIR "${SYSCONFDIR}"
validate_absolute_target STATE_DIR "${STATE_DIR}"
validate_absolute_target PRIVD_STATE_DIR "${PRIVD_STATE_DIR}"

if [[ "${PRIVD_STATE_DIR}" != "/var/lib/ocservia-privd" ]]; then
  echo "PRIVD_STATE_DIR must use the fixed systemd-managed /var/lib/ocservia-privd hierarchy" >&2
  exit 2
fi

if [[ -z "${DESTDIR}" ]]; then
  if ! getent group ocserv-agent >/dev/null; then
    groupadd --system ocserv-agent
  fi
  if ! getent passwd ocserv-agent >/dev/null; then
    useradd --system --gid ocserv-agent --home-dir "${STATE_DIR}" --shell /usr/sbin/nologin ocserv-agent
  fi
  agent_owner=ocserv-agent
  agent_group=ocserv-agent
  AGENT_UID="$(id -u ocserv-agent)"
  AGENT_GID="$(id -g ocserv-agent)"
else
  if [[ "${DESTDIR}" != /* || "${DESTDIR}" == "/" || "${DESTDIR}" == */ ]] || ! [[ "${AGENT_UID}" =~ ^[0-9]+$ && "${AGENT_GID}" =~ ^[0-9]+$ ]]; then
    echo "DESTDIR must be an absolute staging root and numeric agent IDs are required" >&2
    exit 2
  fi
  agent_owner="${AGENT_UID}"
  agent_group="${AGENT_GID}"
fi

ensure_root_directory "${DESTDIR}${PREFIX}/libexec/ocservia" root 0 0755
ensure_root_directory "${DESTDIR}${PREFIX}/lib/systemd/system" root 0 0755
ensure_agent_state
ensure_root_directory "${DESTDIR}${PRIVD_STATE_DIR}" "${agent_group}" "${AGENT_GID}" 0700
ensure_root_directory "${DESTDIR}${SYSCONFDIR}/ocservia-agent" "${agent_group}" "${AGENT_GID}" 0750
install -m 0755 -- "${ROOT}/rust/target/release/ocservia-agent" "${DESTDIR}${PREFIX}/libexec/ocservia/ocservia-agent"
install -m 0755 -- "${ROOT}/rust/target/release/ocservia-privd" "${DESTDIR}${PREFIX}/libexec/ocservia/ocservia-privd"
install -m 0755 -- "${ROOT}/rust/target/release/ocservia-upgrader" "${DESTDIR}${PREFIX}/libexec/ocservia/ocservia-upgrader"
install -m 0755 -- "${ROOT}/scripts/rollback-agent.sh" "${DESTDIR}${PREFIX}/libexec/ocservia/ocservia-agent-rollback"
install -m 0755 -- "${ROOT}/scripts/verify-agent-package.sh" "${DESTDIR}${PREFIX}/libexec/ocservia/ocservia-agent-verify"
install -m 0644 -- "${ROOT}/deploy/systemd/ocservia-agent.service" "${DESTDIR}${PREFIX}/lib/systemd/system/ocservia-agent.service"
install -m 0644 -- "${ROOT}/deploy/systemd/ocservia-privd.service" "${DESTDIR}${PREFIX}/lib/systemd/system/ocservia-privd.service"
install -m 0644 -- "${ROOT}/deploy/systemd/ocservia-upgrader@.service" "${DESTDIR}${PREFIX}/lib/systemd/system/ocservia-upgrader@.service"

printf 'AGENT_UID=%s\nPRIVD_ATTESTATION_KEY_FILE=/var/lib/ocservia-privd/attestation.key\nUSER_PASSWORD_SEAL_PRIVATE_KEY_FILE=/etc/ocservia-agent/user-password-seal-private.pem\nP12_PASSWORD_SEAL_PRIVATE_KEY_FILE=/etc/ocservia-agent/p12-password-seal-private.pem\n' "${AGENT_UID}" >"${DESTDIR}${SYSCONFDIR}/ocservia-agent/privd.env"
chmod 0640 -- "${DESTDIR}${SYSCONFDIR}/ocservia-agent/privd.env"
chown root:"${agent_group}" -- "${DESTDIR}${SYSCONFDIR}/ocservia-agent/privd.env"
if [[ ! -e "${DESTDIR}${SYSCONFDIR}/ocservia-agent/agent.env" ]]; then
  install -o root -g "${agent_group}" -m 0640 -- "${ROOT}/deploy/systemd/agent.env.example" "${DESTDIR}${SYSCONFDIR}/ocservia-agent/agent.env"
fi

if [[ "${INSTALL_PRODUCTION_RELAYS}" == true ]]; then
  ensure_root_directory "${DESTDIR}${PREFIX}/lib/systemd/system/ocservia-agent.service.d" root 0 0755
  install -m 0644 -- "${ROOT}/deploy/production/systemd/ocservia-agent-relays.conf" \
    "${DESTDIR}${PREFIX}/lib/systemd/system/ocservia-agent.service.d/10-production-relays.conf"
  if [[ ! -e "${DESTDIR}${SYSCONFDIR}/ocservia-agent/relays.env" ]]; then
    install -o root -g "${agent_group}" -m 0640 -- "${ROOT}/deploy/production/systemd/relays.env.example" \
      "${DESTDIR}${SYSCONFDIR}/ocservia-agent/relays.env"
  fi
fi

if [[ -z "${DESTDIR}" ]]; then
  systemctl daemon-reload
fi
