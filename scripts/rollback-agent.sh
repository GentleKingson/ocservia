#!/usr/bin/env bash
set -euo pipefail

DESTDIR="${DESTDIR:-}"
PREFIX="${PREFIX:-/usr}"
PRIVD_STATE_DIR="${PRIVD_STATE_DIR:-/var/lib/ocservia-privd}"
BACKUP_DIR="${BACKUP_DIR:-${DESTDIR}${PRIVD_STATE_DIR}/upgrade-backup}"

rollback_error() {
  echo "Agent rollback blocked before modification: $1" >&2
  exit 1
}

validate_root_ancestry() {
  local path="$1" current="" relative component uid mode
  local -a components=()
  if [[ -n "${DESTDIR}" ]]; then
    case "${path}" in
      "${DESTDIR}"|"${DESTDIR}"/*) ;;
      *) rollback_error "trusted filesystem path escapes DESTDIR" ;;
    esac
    if [[ ! -d "${DESTDIR}" || -L "${DESTDIR}" || "$(stat -c '%u:%g:%a' -- "${DESTDIR}")" != "0:0:700" ]]; then
      rollback_error "DESTDIR must remain root:root mode 0700"
    fi
    current="${DESTDIR}"
    relative="${path#"${DESTDIR}"}"
  else
    relative="${path}"
  fi
  IFS='/' read -r -a components <<<"${relative#/}"
  for component in "${components[@]}"; do
    [[ -n "${component}" ]] || continue
    current="${current}/${component}"
    if [[ ! -d "${current}" || -L "${current}" ]]; then
      rollback_error "trusted ancestry contains a non-directory or symlink: ${current}"
    fi
    read -r uid mode < <(stat -c '%u %a' -- "${current}")
    if [[ "${uid}" != 0 ]] || (( (8#${mode} & 8#022) != 0 )); then
      rollback_error "trusted ancestry must be root-owned and not group/world writable: ${current}"
    fi
  done
}

validate_file() {
  local path="$1" expected_mode="$2" uid gid mode links
  if [[ ! -f "${path}" || -L "${path}" ]]; then
    rollback_error "rollback snapshot contains a missing, non-regular, or symlinked file: ${path}"
  fi
  read -r uid gid mode links < <(stat -c '%u %g %a %h' -- "${path}")
  if [[ "${uid}:${gid}:${mode}:${links}" != "0:0:${expected_mode}:1" ]]; then
    rollback_error "rollback snapshot file has unsafe owner, group, mode, or link count: ${path}"
  fi
}

validate_digest() {
  local name="$1" manifest="$2" digest
  digest="$(sha256sum -- "${BACKUP_DIR}/${name}" | awk '{print $1}')"
  if ! grep -Fxq "${digest}  ${name}" "${manifest}"; then
    rollback_error "rollback snapshot digest does not match its trusted manifest: ${name}"
  fi
}

validate_destination() {
  local path="$1" expected_mode="$2" uid gid mode links
  validate_root_ancestry "$(dirname -- "${path}")"
  if [[ -e "${path}" || -L "${path}" ]]; then
    if [[ ! -f "${path}" || -L "${path}" ]]; then
      rollback_error "installed rollback destination is not a regular file: ${path}"
    fi
    read -r uid gid mode links < <(stat -c '%u %g %a %h' -- "${path}")
    if [[ "${uid}:${gid}:${mode}:${links}" != "0:0:${expected_mode}:1" ]]; then
      rollback_error "installed rollback destination has unsafe owner, group, mode, or link count: ${path}"
    fi
  fi
}

restore_file() {
  local source="$1" destination="$2" mode="$3" staging
  staging="$(dirname -- "${destination}")/.ocservia-rollback-$(basename -- "${destination}").$$"
  if [[ -e "${staging}" || -L "${staging}" ]]; then
    rollback_error "rollback destination staging path already exists"
  fi
  install -o root -g root -m "${mode}" -- "${source}" "${staging}"
  sync -f "${staging}"
  mv -fT -- "${staging}" "${destination}"
  if [[ "$(sha256sum -- "${source}" | awk '{print $1}')" != "$(sha256sum -- "${destination}" | awk '{print $1}')" || \
        "$(stat -c '%u:%g:%a:%h' -- "${destination}")" != "0:0:${mode}:1" ]]; then
    rollback_error "restored rollback destination failed post-publish verification"
  fi
  sync -f "${destination}"
  sync -f "$(dirname -- "${destination}")"
}

if [[ ${EUID} -ne 0 ]]; then
  echo "rollback-agent.sh must run as root" >&2
  exit 1
fi
if [[ -n "${DESTDIR}" && ( "${DESTDIR}" != /* || "${DESTDIR}" == "/" || "${DESTDIR}" == */ ) ]] || \
  [[ "${PREFIX}" != /* || "${PRIVD_STATE_DIR}" != /* || "${BACKUP_DIR}" != /* ]]; then
  echo "DESTDIR, PREFIX, PRIVD_STATE_DIR, and BACKUP_DIR must identify absolute paths" >&2
  exit 2
fi
if [[ "${BACKUP_DIR}" != "${DESTDIR}${PRIVD_STATE_DIR}/upgrade-backup" ]]; then
  echo "BACKUP_DIR must use the fixed privd-owned upgrade-backup location" >&2
  exit 2
fi
if [[ "${PRIVD_STATE_DIR}" != "/var/lib/ocservia-privd" ]]; then
  echo "PRIVD_STATE_DIR must use the fixed root-owned /var/lib/ocservia-privd hierarchy" >&2
  exit 2
fi

validate_root_ancestry "${BACKUP_DIR}"
if [[ "$(stat -c '%u:%g:%a' -- "${DESTDIR}${PRIVD_STATE_DIR}")" != "0:0:700" || \
      "$(stat -c '%u:%g:%a' -- "${BACKUP_DIR}")" != "0:0:700" ]]; then
  rollback_error "privd state and rollback snapshot directories must be root:root mode 0700"
fi

manifest="${BACKUP_DIR}/MANIFEST.sha256"
validate_file "${manifest}" 600
if [[ "$(wc -l <"${manifest}")" -ne 5 ]] || \
  awk 'length($1) != 64 || $1 !~ /^[0-9a-f]+$/ || $2 !~ /^(ocservia-agent\.previous|ocservia-privd\.previous|ocservia-agent\.service\.previous|ocservia-privd\.service\.previous|ocservia-agent-relays\.conf\.(previous|absent))$/ || NF != 2 { bad=1 } END { exit bad ? 0 : 1 }' "${manifest}"; then
  rollback_error "rollback snapshot manifest is malformed"
fi

required_backups=(
  ocservia-agent.previous
  ocservia-privd.previous
  ocservia-agent.service.previous
  ocservia-privd.service.previous
)
for backup in "${required_backups[@]}"; do
  case "${backup}" in
    *.service.previous) mode=644 ;;
    *) mode=755 ;;
  esac
  validate_file "${BACKUP_DIR}/${backup}" "${mode}"
  validate_digest "${backup}" "${manifest}"
done

relay_backup="${BACKUP_DIR}/ocservia-agent-relays.conf.previous"
relay_absent="${BACKUP_DIR}/ocservia-agent-relays.conf.absent"
if [[ -f "${relay_backup}" && ! -L "${relay_backup}" && ! -e "${relay_absent}" && ! -L "${relay_absent}" ]]; then
  validate_file "${relay_backup}" 644
  validate_digest "$(basename -- "${relay_backup}")" "${manifest}"
  restore_relay=true
elif [[ -f "${relay_absent}" && ! -L "${relay_absent}" && ! -e "${relay_backup}" && ! -L "${relay_backup}" ]]; then
  validate_file "${relay_absent}" 600
  validate_digest "$(basename -- "${relay_absent}")" "${manifest}"
  restore_relay=false
else
  rollback_error "rollback snapshot has ambiguous or unsafe relay drop-in state"
fi

libexec="${DESTDIR}${PREFIX}/libexec/ocservia"
systemd="${DESTDIR}${PREFIX}/lib/systemd/system"
relay_directory="${systemd}/ocservia-agent.service.d"
relay_directory_missing=false
validate_destination "${libexec}/ocservia-agent" 755
validate_destination "${libexec}/ocservia-privd" 755
validate_destination "${systemd}/ocservia-agent.service" 644
validate_destination "${systemd}/ocservia-privd.service" 644
validate_root_ancestry "${libexec}"
validate_root_ancestry "${systemd}"
if [[ "${restore_relay}" == true ]]; then
  if [[ ! -e "${relay_directory}" && ! -L "${relay_directory}" ]]; then
    validate_root_ancestry "$(dirname -- "${relay_directory}")"
    relay_directory_missing=true
  else
    validate_root_ancestry "${relay_directory}"
    validate_destination "${relay_directory}/10-production-relays.conf" 644
  fi
elif [[ -e "${relay_directory}/10-production-relays.conf" || -L "${relay_directory}/10-production-relays.conf" ]]; then
  validate_destination "${relay_directory}/10-production-relays.conf" 644
fi

if [[ -z "${DESTDIR}" ]]; then
  systemctl stop ocservia-agent.service ocservia-privd.service
fi
if [[ "${relay_directory_missing}" == true ]]; then
  install -d -o root -g root -m 0755 -- "${relay_directory}"
  validate_root_ancestry "${relay_directory}"
fi

restore_file "${BACKUP_DIR}/ocservia-agent.previous" "${libexec}/ocservia-agent" 755
restore_file "${BACKUP_DIR}/ocservia-privd.previous" "${libexec}/ocservia-privd" 755
restore_file "${BACKUP_DIR}/ocservia-agent.service.previous" "${systemd}/ocservia-agent.service" 644
restore_file "${BACKUP_DIR}/ocservia-privd.service.previous" "${systemd}/ocservia-privd.service" 644
if [[ "${restore_relay}" == true ]]; then
  restore_file "${relay_backup}" "${relay_directory}/10-production-relays.conf" 644
else
  if [[ -d "${relay_directory}" && ! -L "${relay_directory}" ]]; then
    rm -f -- "${relay_directory}/10-production-relays.conf"
    sync -f "${relay_directory}"
    rmdir -- "${relay_directory}" 2>/dev/null || true
  fi
fi

if [[ -z "${DESTDIR}" ]]; then
  systemctl daemon-reload
  systemctl start ocservia-privd.service
  systemctl start ocservia-agent.service
fi
echo "Agent, privd, and systemd units restored from one verified matched rollback snapshot"
