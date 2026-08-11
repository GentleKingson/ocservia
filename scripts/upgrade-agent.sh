#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DESTDIR="${DESTDIR:-}"
PREFIX="${PREFIX:-/usr}"
SYSCONFDIR="${SYSCONFDIR:-/etc}"
STATE_DIR="${STATE_DIR:-/var/lib/ocservia-agent}"
AGENT_UID="${AGENT_UID:-997}"
AGENT_GID="${AGENT_GID:-997}"
BACKUP_DIR="${BACKUP_DIR:-${DESTDIR}${STATE_DIR}/upgrade-backup}"

upgrade_preflight_error() {
  echo "Agent upgrade blocked before modification: $1" >&2
  echo "Install an Ed25519 Controller verification public key as root:ocserv-agent mode 0440 or 0640," >&2
  echo "set CONTROLLER_COMMAND_VERIFICATION_KEY_FILE to its absolute path in ${SYSCONFDIR}/ocservia-agent/agent.env, then rerun the upgrade." >&2
  exit 1
}

installed_pair_preflight_error() {
  echo "Agent upgrade blocked before modification: $1" >&2
  exit 1
}

validate_safe_ancestry() {
  local runtime_parent="$1"
  local runtime_path=""
  local actual_path uid mode
  local -a components=()

  if [[ -z "${DESTDIR}" ]]; then
    actual_path="/"
    uid="$(stat -c '%u' -- "${actual_path}")"
    mode="$(stat -c '%a' -- "${actual_path}")"
    if [[ ! -d "${actual_path}" || -L "${actual_path}" || "${uid}" != 0 ]] || (( (8#${mode} & 8#022) != 0 )); then
      upgrade_preflight_error "Controller command key ancestry has an unsafe root directory"
    fi
  fi

  IFS='/' read -r -a components <<<"${runtime_parent#/}"
  for component in "${components[@]}"; do
    [[ -n "${component}" ]] || continue
    runtime_path="${runtime_path}/${component}"
    actual_path="${DESTDIR}${runtime_path}"
    if [[ ! -d "${actual_path}" || -L "${actual_path}" ]]; then
      upgrade_preflight_error "Controller command key ancestor ${runtime_path} must be a real directory"
    fi
    uid="$(stat -c '%u' -- "${actual_path}")"
    mode="$(stat -c '%a' -- "${actual_path}")"
    # Agent and privd load this same trust anchor independently. Their safe
    # ancestry intersection is root-owned and not group/world writable.
    if [[ "${uid}" != 0 ]] || (( (8#${mode} & 8#022) != 0 )); then
      upgrade_preflight_error "Controller command key ancestor ${runtime_path} has unsafe ownership or mode"
    fi
  done
}

validate_controller_command_key() {
  local agent_env="${DESTDIR}${SYSCONFDIR}/ocservia-agent/agent.env"
  local env_uid env_gid env_mode env_links
  local key_path key_file key_uid key_gid key_mode key_links key_size key_description
  local -a configured_paths=()

  validate_safe_ancestry "${SYSCONFDIR}/ocservia-agent"
  if [[ ! -f "${agent_env}" || -L "${agent_env}" ]]; then
    upgrade_preflight_error "${SYSCONFDIR}/ocservia-agent/agent.env must be a regular file"
  fi
  read -r env_uid env_gid env_mode env_links < <(stat -c '%u %g %a %h' -- "${agent_env}")
  if [[ "${env_uid}" != 0 || "${env_links}" != 1 ]] || \
    ! { [[ "${env_mode}" == 600 ]] || [[ "${env_gid}" == "${AGENT_GID}" && "${env_mode}" == 640 ]]; }; then
    upgrade_preflight_error "${SYSCONFDIR}/ocservia-agent/agent.env has unsafe ownership, mode, or link count"
  fi

  mapfile -t configured_paths < <(sed -n 's/^CONTROLLER_COMMAND_VERIFICATION_KEY_FILE=//p' "${agent_env}")
  if [[ ${#configured_paths[@]} -ne 1 ]]; then
    upgrade_preflight_error "agent.env must contain exactly one CONTROLLER_COMMAND_VERIFICATION_KEY_FILE assignment"
  fi
  key_path="${configured_paths[0]}"
  if [[ "${key_path}" != /* || "${key_path}" =~ [[:space:]] || ${#key_path} -gt 4096 ]]; then
    upgrade_preflight_error "CONTROLLER_COMMAND_VERIFICATION_KEY_FILE must be one clean absolute path without whitespace"
  fi
  case "/${key_path#/}/" in
    *//*|*/./*|*/../*)
      upgrade_preflight_error "CONTROLLER_COMMAND_VERIFICATION_KEY_FILE must not contain empty, dot, or parent components"
      ;;
  esac

  validate_safe_ancestry "$(dirname -- "${key_path}")"
  key_file="${DESTDIR}${key_path}"
  if [[ ! -f "${key_file}" || -L "${key_file}" ]]; then
    upgrade_preflight_error "${key_path} must be an existing regular file"
  fi
  read -r key_uid key_gid key_mode key_links key_size < <(stat -c '%u %g %a %h %s' -- "${key_file}")
  if [[ "${key_links}" != 1 || "${key_size}" -lt 1 || "${key_size}" -gt 4096 ]]; then
    upgrade_preflight_error "${key_path} must be a one-link regular file containing 1..4096 bytes"
  fi
  if [[ "${key_uid}" != 0 || "${key_gid}" != "${AGENT_GID}" ]] || \
    ! { [[ "${key_mode}" == 440 ]] || [[ "${key_mode}" == 640 ]]; }; then
    upgrade_preflight_error "${key_path} must be root:ocserv-agent mode 0440 or 0640 so Agent and privd can both load it"
  fi
  if ! key_description="$(openssl pkey -pubin -in "${key_file}" -noout -text 2>/dev/null)" || \
    [[ "${key_description}" != ED25519\ Public-Key:* ]]; then
    upgrade_preflight_error "${key_path} must contain an Ed25519 SubjectPublicKeyInfo public key"
  fi
}

if [[ ${EUID} -ne 0 ]]; then
  echo "upgrade-agent.sh must run as root" >&2
  exit 1
fi

if [[ -n "${DESTDIR}" && ( "${DESTDIR}" != /* || "${DESTDIR}" == "/" ) ]] || \
  [[ "${PREFIX}" != /* || "${SYSCONFDIR}" != /* || "${STATE_DIR}" != /* || "${BACKUP_DIR}" != /* ]]; then
  echo "DESTDIR, PREFIX, SYSCONFDIR, STATE_DIR, and BACKUP_DIR must identify absolute paths" >&2
  exit 2
fi
if [[ -z "${DESTDIR}" ]]; then
  if ! getent passwd ocserv-agent >/dev/null || ! getent group ocserv-agent >/dev/null; then
    upgrade_preflight_error "the existing ocserv-agent account and group are required"
  fi
  AGENT_UID="$(id -u ocserv-agent)"
  AGENT_GID="$(id -g ocserv-agent)"
elif ! [[ "${AGENT_UID}" =~ ^[0-9]+$ && "${AGENT_GID}" =~ ^[0-9]+$ ]]; then
  echo "numeric AGENT_UID and AGENT_GID are required with DESTDIR" >&2
  exit 2
fi

# Protocol 1.1 cannot execute mutations without this key. Complete this
# preflight before creating backups, replacing binaries or units, or restarting
# either service so a legacy two-line agent.env remains fully untouched.
validate_controller_command_key

installed_agent="${DESTDIR}${PREFIX}/libexec/ocservia/ocservia-agent"
installed_privd="${DESTDIR}${PREFIX}/libexec/ocservia/ocservia-privd"
installed_agent_unit="${DESTDIR}${PREFIX}/lib/systemd/system/ocservia-agent.service"
installed_privd_unit="${DESTDIR}${PREFIX}/lib/systemd/system/ocservia-privd.service"
installed_relay_dropin="${DESTDIR}${PREFIX}/lib/systemd/system/ocservia-agent.service.d/10-production-relays.conf"
for installed_file in \
  "${installed_agent}" \
  "${installed_privd}" \
  "${installed_agent_unit}" \
  "${installed_privd_unit}"; do
  if [[ ! -f "${installed_file}" || -L "${installed_file}" ]]; then
    installed_pair_preflight_error "the installed Agent/privd pair and base units must be regular files"
  fi
done
if [[ -e "${installed_relay_dropin}" || -L "${installed_relay_dropin}" ]] && \
  [[ ! -f "${installed_relay_dropin}" || -L "${installed_relay_dropin}" ]]; then
  installed_pair_preflight_error "the installed production relay drop-in must be a regular file"
fi

install -d -o root -g root -m 0700 "${BACKUP_DIR}"
install -m 0755 "${installed_agent}" "${BACKUP_DIR}/ocservia-agent.previous"
install -m 0755 "${installed_privd}" "${BACKUP_DIR}/ocservia-privd.previous"
install -m 0644 "${installed_agent_unit}" "${BACKUP_DIR}/ocservia-agent.service.previous"
install -m 0644 "${installed_privd_unit}" "${BACKUP_DIR}/ocservia-privd.service.previous"
rm -f -- "${BACKUP_DIR}/ocservia-agent-relays.conf.previous" \
  "${BACKUP_DIR}/ocservia-agent-relays.conf.absent"
if [[ -f "${installed_relay_dropin}" ]]; then
  install -m 0644 "${installed_relay_dropin}" \
    "${BACKUP_DIR}/ocservia-agent-relays.conf.previous"
else
  install -m 0600 /dev/null "${BACKUP_DIR}/ocservia-agent-relays.conf.absent"
fi

"${ROOT}/scripts/install-agent.sh"
if [[ -z "${DESTDIR}" ]]; then
  systemctl try-restart ocservia-privd.service ocservia-agent.service
fi
