#!/usr/bin/env bash
set -euo pipefail

DESTDIR="${DESTDIR:-}"
PREFIX="${PREFIX:-/usr}"
UPGRADE_STATE_DIR="${UPGRADE_STATE_DIR:-/var/lib/ocservia-upgrade}"
BACKUP_DIR="${BACKUP_DIR:-${DESTDIR}${UPGRADE_STATE_DIR}/upgrade-backup}"
verify_only=false
if [[ "$#" == 1 && "$1" == --verify-only ]]; then
  verify_only=true
elif [[ "$#" != 0 ]]; then
  echo 'usage: rollback-agent.sh [--verify-only]' >&2
  exit 2
fi

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
  [[ "${PREFIX}" != /* || "${UPGRADE_STATE_DIR}" != /* || "${BACKUP_DIR}" != /* ]]; then
  echo "DESTDIR, PREFIX, UPGRADE_STATE_DIR, and BACKUP_DIR must identify absolute paths" >&2
  exit 2
fi
if [[ "${BACKUP_DIR}" != "${DESTDIR}${UPGRADE_STATE_DIR}/upgrade-backup" ]]; then
  echo "BACKUP_DIR must use the fixed root-only upgrade-backup location" >&2
  exit 2
fi
if [[ "${UPGRADE_STATE_DIR}" != "/var/lib/ocservia-upgrade" ]]; then
  echo "UPGRADE_STATE_DIR must use the fixed root-only /var/lib/ocservia-upgrade hierarchy" >&2
  exit 2
fi

validate_root_ancestry "${BACKUP_DIR}"
if [[ "$(stat -c '%u:%g:%a' -- "${DESTDIR}${UPGRADE_STATE_DIR}")" != "0:0:700" || \
      "$(stat -c '%u:%g:%a' -- "${BACKUP_DIR}")" != "0:0:700" ]]; then
  rollback_error "upgrade state and rollback snapshot directories must be root:root mode 0700"
fi

manifest="${BACKUP_DIR}/MANIFEST.sha256"
if [[ "${verify_only}" != true ]]; then
  [[ ! -L "${DESTDIR}${UPGRADE_STATE_DIR}/.binding-lifecycle.lock" ]] || rollback_error "unsafe lifecycle lock"
  exec 9>"${DESTDIR}${UPGRADE_STATE_DIR}/.binding-lifecycle.lock"
  flock -n 9 || rollback_error "another binding/package lifecycle operation is active"
fi
validate_file "${manifest}" 600
snapshot_entries="$(wc -l <"${manifest}")"
if [[ "${snapshot_entries}" -ne 8 && "${snapshot_entries}" -ne 9 && "${snapshot_entries}" -ne 13 ]] || \
  awk 'length($1) != 64 || $1 !~ /^[0-9a-f]+$/ || $2 !~ /^(ocservia-agent\.previous|ocservia-privd\.previous|ocservia-agent\.service\.previous|ocservia-privd\.service\.previous|ocservia-agent-relays\.conf\.(previous|absent)|ocservia-agent-relays\.(previous|absent)|ocservia-upgrader\.(previous|absent)|ocservia-upgrader@\.service\.(previous|absent)|ocservia-agent-verify\.(previous|absent)|ocservia-agent-rebind\.(previous|absent)|ocservia-agent-retention(\.service|\.timer)?\.(previous|absent))$/ || NF != 2 || seen[$2]++ { bad=1 } END { exit bad ? 0 : 1 }' "${manifest}"; then
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

# Each optional package artifact is either restored from .previous or
# removed per .absent, so a rollback cannot leave a mixed-generation runner.
resolve_optional_backup() {
  local base="$1" expected_mode="$2" label="${3:-$1}"
  local backup="${BACKUP_DIR}/${base}.previous" absent="${BACKUP_DIR}/${base}.absent"
  if [[ -f "${backup}" && ! -L "${backup}" && ! -e "${absent}" && ! -L "${absent}" ]]; then
    validate_file "${backup}" "${expected_mode}"
    validate_digest "${base}.previous" "${manifest}"
    resolved_backup="${backup}"
  elif [[ -f "${absent}" && ! -L "${absent}" && ! -e "${backup}" && ! -L "${backup}" ]]; then
    validate_file "${absent}" 600
    validate_digest "${base}.absent" "${manifest}"
    resolved_backup=""
  else
    rollback_error "rollback snapshot has ambiguous or unsafe ${label} state"
  fi
}

resolve_optional_backup ocservia-agent-relays.conf 644 'relay drop-in'
relay_backup="${resolved_backup}"
restore_relay=false
[[ -z "${relay_backup}" ]] || restore_relay=true
resolve_optional_backup ocservia-upgrader 755
upgrader_backup="${resolved_backup}"
resolve_optional_backup 'ocservia-upgrader@.service' 644
upgrader_unit_backup="${resolved_backup}"
resolve_optional_backup ocservia-agent-verify 755
verifier_backup="${resolved_backup}"

# Eight-entry snapshots predate the optional-Relay launcher. Do not infer
# single-Relay support from a version number or silently change relays.env.
relay_launcher_backup=""
if [[ "${snapshot_entries}" -ne 8 ]]; then
  resolve_optional_backup ocservia-agent-relays 755
  relay_launcher_backup="${resolved_backup}"
fi
if [[ "${restore_relay}" == true && -z "${relay_launcher_backup}" ]] &&
  grep -Fq '/usr/libexec/ocservia/ocservia-agent-relays' "${relay_backup}"; then
  rollback_error "rollback snapshot is missing the production Relay launcher required by its service"
fi

libexec="${DESTDIR}${PREFIX}/libexec/ocservia"
systemd="${DESTDIR}${PREFIX}/lib/systemd/system"
lifecycle_restores=()
retention_timer_backup=""
if [[ "${snapshot_entries}" -eq 13 ]]; then
  resolve_optional_backup ocservia-agent-rebind 755
  rebind_backup="${resolved_backup}"
  resolve_optional_backup ocservia-agent-retention 755
  retention_backup="${resolved_backup}"
  resolve_optional_backup ocservia-agent-retention.service 644
  retention_unit_backup="${resolved_backup}"
  resolve_optional_backup ocservia-agent-retention.timer 644
  retention_timer_backup="${resolved_backup}"
  if [[ -n "${retention_backup}" && -z "${rebind_backup}" ]]; then
    rollback_error "rollback snapshot retention executable requires rebind helper"
  fi
  if [[ -n "${retention_unit_backup}" && -z "${retention_backup}" ]]; then
    rollback_error "rollback snapshot retention service requires retention executable"
  fi
  if [[ -n "${retention_timer_backup}" && -z "${retention_unit_backup}" ]]; then
    rollback_error "rollback snapshot retention timer requires retention service"
  fi
  lifecycle_restores=(
    "${rebind_backup}" "${libexec}/ocservia-agent-rebind" 755
    "${retention_backup}" "${libexec}/ocservia-agent-retention" 755
    "${retention_unit_backup}" "${systemd}/ocservia-agent-retention.service" 644
    "${retention_timer_backup}" "${systemd}/ocservia-agent-retention.timer" 644
  )
fi
pending_record="${DESTDIR}${UPGRADE_STATE_DIR}/installing-package"
if [[ -e "${pending_record}" || -L "${pending_record}" ]]; then
  if [[ ! -f "${pending_record}" || -L "${pending_record}" ]] ||
    [[ "$(stat -c '%u:%g:%a:%h' -- "${pending_record}")" != "0:0:600:1" || "$(wc -l <"${pending_record}")" -ne 1 ]] ||
    ! [[ "$(cat -- "${pending_record}")" =~ ^archive_sha256=[0-9a-f]{64}$ ]]; then
    rollback_error "pending installation record is malformed or unsafe"
  fi
fi

binding_node=""
active_binding="${DESTDIR}/etc/ocservia-agent/active-binding"
if [[ -e "${active_binding}" || -L "${active_binding}" ]]; then
  validate_root_ancestry "$(dirname -- "${active_binding}")"
  [[ -f "${active_binding}" && ! -L "${active_binding}" ]] || rollback_error "unsafe Controller binding"
  read -r binding_uid binding_mode binding_links < <(stat -c '%u %a %h' -- "${active_binding}")
  [[ "${binding_uid}" == 0 && "${binding_links}" == 1 && ( "${binding_mode}" == 640 || "${binding_mode}" == 440 ) ]] || rollback_error "unsafe Controller binding metadata"
  binding_node="$(sed -n '2p' "${active_binding}")"
  [[ "${binding_node}" =~ ^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$ ]] || rollback_error "invalid Controller binding node"
  for binary in ocservia-agent ocservia-privd ocservia-upgrader; do
    [[ "$("${BACKUP_DIR}/${binary}.previous" --binding-version)" == 1 ]] ||
      rollback_error "snapshot ${binary} cannot enforce the committed Controller binding"
  done
fi


if [[ "${verify_only}" == true ]]; then
  echo "Matched rollback snapshot verified without modification"
  exit 0
fi

# Missing legacy records mean unknown prior state, never an .absent claim.
if [[ "${snapshot_entries}" -lt 13 ]]; then
  rollback_error "rollback snapshot does not record prior rebind and retention state; mutation requires a complete matched snapshot"
fi
relay_directory="${systemd}/ocservia-agent.service.d"
relay_directory_missing=false
validate_destination "${libexec}/ocservia-agent" 755
validate_destination "${libexec}/ocservia-privd" 755
validate_destination "${libexec}/ocservia-agent-relays" 755
validate_destination "${systemd}/ocservia-agent.service" 644
validate_destination "${systemd}/ocservia-privd.service" 644
validate_root_ancestry "${libexec}"
validate_root_ancestry "${systemd}"
for ((entry = 0; entry < ${#lifecycle_restores[@]}; entry += 3)); do
  validate_destination "${lifecycle_restores[entry + 1]}" "${lifecycle_restores[entry + 2]}"
done
# A present runner artifact must be a safe restore destination; an absent one
# matches the .absent snapshot branch.
if [[ -n "${upgrader_backup}" ]]; then
  validate_destination "${libexec}/ocservia-upgrader" 755
fi
if [[ -n "${upgrader_unit_backup}" ]]; then
  validate_destination "${systemd}/ocservia-upgrader@.service" 644
fi
if [[ -n "${verifier_backup}" ]]; then
  validate_destination "${libexec}/ocservia-agent-verify" 755
fi
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

# A rollback invalidates every durable upgrade intent that has not reached a
# terminal state, so a restarted upgrader cannot re-apply the rolled-back
# release afterwards.
mark_operations_rolled_back() {
  local operations="${DESTDIR}${UPGRADE_STATE_DIR}/operations" entry state staging
  if [[ -n "${binding_node:-}" ]]; then
    operations="${DESTDIR}${UPGRADE_STATE_DIR}/bindings/${binding_node}/operations"
  fi
  [[ -d "${operations}" ]] || return 0
  for entry in "${operations}"/*; do
    [[ -d "${entry}" ]] || continue
    [[ "$(basename -- "${entry}")" =~ ^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$ ]] \
      || continue
    state="${entry}/state"
    [[ -f "${state}" && ! -L "${state}" ]] || continue
    case "$(cat -- "${state}")" in
      accepted | running*) ;;
      *) continue ;;
    esac
    staging="${entry}/.state.rollback.$$"
    install -o root -g root -m 0600 -- /dev/null "${staging}"
    printf 'rolled_back\n' >"${staging}"
    mv -fT -- "${staging}" "${state}"
    sync -f "${state}"
    sync -f "${entry}"
  done
}


if [[ -z "${DESTDIR}" ]]; then
  if [[ -f "${systemd}/ocservia-agent-retention.timer" ]]; then
    if [[ -n "${retention_timer_backup}" ]]; then
      systemctl stop ocservia-agent-retention.timer
    else
      systemctl disable --now ocservia-agent-retention.timer
    fi
  fi
  [[ ! -f "${systemd}/ocservia-agent-retention.service" ]] || systemctl stop ocservia-agent-retention.service
  systemctl stop 'ocservia-upgrader@*.service' 2>/dev/null || true
  systemctl stop ocservia-agent.service ocservia-privd.service
fi
mark_operations_rolled_back
if [[ "${relay_directory_missing}" == true ]]; then
  install -d -o root -g root -m 0755 -- "${relay_directory}"
  validate_root_ancestry "${relay_directory}"
fi

restore_file "${BACKUP_DIR}/ocservia-agent.previous" "${libexec}/ocservia-agent" 755
restore_file "${BACKUP_DIR}/ocservia-privd.previous" "${libexec}/ocservia-privd" 755
if [[ -n "${relay_launcher_backup}" ]]; then
  restore_file "${relay_launcher_backup}" "${libexec}/ocservia-agent-relays" 755
else
  rm -f -- "${libexec}/ocservia-agent-relays"
fi
restore_file "${BACKUP_DIR}/ocservia-agent.service.previous" "${systemd}/ocservia-agent.service" 644
restore_file "${BACKUP_DIR}/ocservia-privd.service.previous" "${systemd}/ocservia-privd.service" 644
optional_restores=(
  "${upgrader_backup}" "${libexec}/ocservia-upgrader" 755
  "${upgrader_unit_backup}" "${systemd}/ocservia-upgrader@.service" 644
  "${verifier_backup}" "${libexec}/ocservia-agent-verify" 755
  "${lifecycle_restores[@]}"
)
for ((entry = 0; entry < ${#optional_restores[@]}; entry += 3)); do
  backup="${optional_restores[entry]}"
  destination="${optional_restores[entry + 1]}"
  if [[ -n "${backup}" ]]; then
    restore_file "${backup}" "${destination}" "${optional_restores[entry + 2]}"
  else
    rm -f -- "${destination}"
  fi
done
if [[ "${restore_relay}" == true ]]; then
  restore_file "${relay_backup}" "${relay_directory}/10-production-relays.conf" 644
else
  if [[ -d "${relay_directory}" && ! -L "${relay_directory}" ]]; then
    rm -f -- "${relay_directory}/10-production-relays.conf"
    sync -f "${relay_directory}"
    rmdir -- "${relay_directory}" 2>/dev/null || true
  fi
fi

# ponytail: Keep this standalone rollback driver while it is executing. Its
# bounded snapshot reader must be extended explicitly for any new file set.
# Flush restores and removals on every filesystem before clearing recovery state.
sync
if [[ -f "${pending_record}" ]]; then
  rm -- "${pending_record}"
  sync -f "${DESTDIR}${UPGRADE_STATE_DIR}"
fi
if [[ -z "${DESTDIR}" ]]; then
  systemctl daemon-reload
  systemctl start ocservia-privd.service
  systemctl start ocservia-agent.service
  flock -u 9
  [[ -z "${retention_timer_backup}" ]] || systemctl start ocservia-agent-retention.timer
fi
echo "Agent, privd, and systemd units restored from one verified matched rollback snapshot"
