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
BACKUP_DIR="${BACKUP_DIR:-${DESTDIR}${PRIVD_STATE_DIR}/upgrade-backup}"
ENROLLMENT_TOKEN_FILE="${ENROLLMENT_TOKEN_FILE:-}"
ENROLLMENT_ENVIRONMENT="${ENROLLMENT_ENVIRONMENT:-}"
ENROLLMENT_MIGRATION_CONFIRMED="${ENROLLMENT_MIGRATION_CONFIRMED:-false}"

validate_verified_package_source() {
  local trusted_root="${DESTDIR}/var/lib/ocservia-privd/package-staging"
  local current="" relative component uid mode marker archive_hash package_name source
  local -a components=()

  case "${ROOT}" in
    "${trusted_root}"/ocservia-agent-package.*/extracted/ocservia-agent-*) ;;
    *) installed_pair_preflight_error "upgrade must run from the root-owned verified package staging hierarchy" ;;
  esac
  if [[ -n "${DESTDIR}" ]]; then
    if [[ "${DESTDIR}" != /* || "${DESTDIR}" == "/" || "${DESTDIR}" == */ || ! -d "${DESTDIR}" || -L "${DESTDIR}" || \
          "$(stat -c '%u:%g:%a' -- "${DESTDIR}")" != "0:0:700" ]]; then
      installed_pair_preflight_error "DESTDIR must be an absolute root:root mode 0700 staging root"
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
      installed_pair_preflight_error "verified package ancestry contains a non-directory or symlink"
    fi
    read -r uid mode < <(stat -c '%u %a' -- "${current}")
    if [[ "${uid}" != 0 ]] || (( (8#${mode} & 8#022) != 0 )); then
      installed_pair_preflight_error "verified package ancestry must be root-owned and not group/world writable"
    fi
  done
  marker="${ROOT}/.ocservia-package-verified"
  if [[ ! -f "${marker}" || -L "${marker}" || "$(stat -c '%u:%g:%a:%h' -- "${marker}")" != "0:0:600:1" ]]; then
    installed_pair_preflight_error "verified package marker is missing or unsafe"
  fi
  archive_hash="$(sed -n 's/^archive_sha256=//p' "${marker}")"
  package_name="$(sed -n 's/^package=//p' "${marker}")"
  if [[ "$(sed -n 's/^version=//p' "${marker}")" != 1 || ! "${archive_hash}" =~ ^[0-9a-f]{64}$ || \
        "${package_name}" != "$(basename -- "${ROOT}")" || "$(wc -l <"${marker}")" -ne 3 ]]; then
    installed_pair_preflight_error "verified package marker is malformed"
  fi
  for source in \
    "${ROOT}/rust/target/release/ocservia-agent" \
    "${ROOT}/rust/target/release/ocservia-privd" \
    "${ROOT}/scripts/install-agent.sh" \
    "${ROOT}/scripts/upgrade-agent.sh" \
    "${ROOT}/scripts/rollback-agent.sh" \
    "${ROOT}/scripts/uninstall-agent.sh" \
    "${ROOT}/deploy/systemd/agent.env.example" \
    "${ROOT}/deploy/systemd/ocservia-agent.service" \
    "${ROOT}/deploy/systemd/ocservia-privd.service" \
    "${ROOT}/deploy/production/systemd/ocservia-agent-relays.conf" \
    "${ROOT}/deploy/production/systemd/relays.env.example"; do
    if [[ ! -f "${source}" || -L "${source}" || "$(stat -c '%u:%g:%h' -- "${source}")" != "0:0:1" ]] || \
      (( (8#$(stat -c '%a' -- "${source}") & 8#022) != 0 )); then
      installed_pair_preflight_error "verified package source has unsafe type, ownership, mode, or link count"
    fi
  done
}

upgrade_preflight_error() {
  echo "Agent upgrade blocked before modification: $1" >&2
  echo "Provision the Controller verification key and both purpose-separated password sealing keys through a trusted channel," >&2
  echo "set their paths, IDs, and public-key fingerprints in ${SYSCONFDIR}/ocservia-agent/agent.env, then rerun the upgrade." >&2
  exit 1
}

installed_pair_preflight_error() {
  echo "Agent upgrade blocked before modification: $1" >&2
  exit 1
}

validate_root_ancestry() {
  local path="$1" current="" relative component uid mode
  local -a components=()
  if [[ -n "${DESTDIR}" ]]; then
    case "${path}" in
      "${DESTDIR}"|"${DESTDIR}"/*) ;;
      *) installed_pair_preflight_error "trusted filesystem path escapes DESTDIR" ;;
    esac
    current="${DESTDIR}"
    relative="${path#"${DESTDIR}"}"
    read -r uid mode < <(stat -c '%u %a' -- "${current}")
    if [[ "${uid}" != 0 ]] || (( (8#${mode} & 8#022) != 0 )); then
      installed_pair_preflight_error "DESTDIR must remain root-owned and not group/world writable"
    fi
  else
    relative="${path}"
  fi
  IFS='/' read -r -a components <<<"${relative#/}"
  for component in "${components[@]}"; do
    [[ -n "${component}" ]] || continue
    current="${current}/${component}"
    if [[ ! -d "${current}" || -L "${current}" ]]; then
      installed_pair_preflight_error "trusted filesystem ancestry contains a non-directory or symlink: ${current}"
    fi
    read -r uid mode < <(stat -c '%u %a' -- "${current}")
    if [[ "${uid}" != 0 ]] || (( (8#${mode} & 8#022) != 0 )); then
      installed_pair_preflight_error "trusted filesystem ancestry must be root-owned and not group/world writable: ${current}"
    fi
  done
}

validate_installed_snapshot_source() {
  local path="$1" expected_mode="$2" uid gid mode links
  validate_root_ancestry "$(dirname -- "${path}")"
  if [[ ! -f "${path}" || -L "${path}" ]]; then
    installed_pair_preflight_error "installed rollback source must be a regular file: ${path}"
  fi
  read -r uid gid mode links < <(stat -c '%u %g %a %h' -- "${path}")
  if [[ "${uid}:${gid}:${mode}:${links}" != "0:0:${expected_mode}:1" ]]; then
    installed_pair_preflight_error "installed rollback source has unsafe owner, group, mode, or link count: ${path}"
  fi
}

write_snapshot_manifest() {
  local directory="$1" manifest name digest
  manifest="${directory}/MANIFEST.sha256"
  : >"${manifest}"
  chmod 0600 -- "${manifest}"
  chown root:root -- "${manifest}"
  for name in \
    ocservia-agent.previous \
    ocservia-privd.previous \
    ocservia-agent.service.previous \
    ocservia-privd.service.previous; do
    digest="$(sha256sum -- "${directory}/${name}" | awk '{print $1}')"
    printf '%s  %s\n' "${digest}" "${name}" >>"${manifest}"
  done
  if [[ -f "${directory}/ocservia-agent-relays.conf.previous" ]]; then
    name=ocservia-agent-relays.conf.previous
  else
    name=ocservia-agent-relays.conf.absent
  fi
  digest="$(sha256sum -- "${directory}/${name}" | awk '{print $1}')"
  printf '%s  %s\n' "${digest}" "${name}" >>"${manifest}"
  sync -f "${manifest}"
  sync -f "${directory}"
}

ensure_root_private_directory() {
  local path="$1" uid mode
  if [[ -e "${path}" || -L "${path}" ]]; then
    if [[ ! -d "${path}" || -L "${path}" ]]; then
      installed_pair_preflight_error "trusted state path must be a real directory: ${path}"
    fi
    validate_root_ancestry "$(dirname -- "${path}")"
    read -r uid mode < <(stat -c '%u %a' -- "${path}")
    if [[ "${uid}" != 0 ]] || (( (8#${mode} & 8#022) != 0 )); then
      installed_pair_preflight_error "trusted state directory must already be root-owned and not group/world writable: ${path}"
    fi
    chown root:root -- "${path}"
    chmod 0700 -- "${path}"
  else
    validate_root_ancestry "$(dirname -- "${path}")"
    install -d -o root -g root -m 0700 -- "${path}"
  fi
  validate_root_ancestry "${path}"
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

validate_password_sealing_keys() {
  local agent_env="${DESTDIR}${SYSCONFDIR}/ocservia-agent/agent.env"
  local user_id p12_id user_hash p12_hash purpose key_path key_file key_id expected_hash
  local uid gid mode links size actual_hash
  user_id="$(sed -n 's/^USER_PASSWORD_SEAL_KEY_ID=//p' "${agent_env}")"
  p12_id="$(sed -n 's/^P12_PASSWORD_SEAL_KEY_ID=//p' "${agent_env}")"
  user_hash="$(sed -n 's/^USER_PASSWORD_SEAL_PUBLIC_KEY_SHA256=//p' "${agent_env}")"
  p12_hash="$(sed -n 's/^P12_PASSWORD_SEAL_PUBLIC_KEY_SHA256=//p' "${agent_env}")"
  if ! [[ "${user_id}" =~ ^[A-Za-z0-9_.-]{1,128}$ && "${p12_id}" =~ ^[A-Za-z0-9_.-]{1,128}$ ]] || [[ "${user_id}" == "${p12_id}" ]]; then
    upgrade_preflight_error "agent.env must configure distinct valid user and P12 password sealing key IDs"
  fi
  if ! [[ "${user_hash}" =~ ^[0-9a-f]{64}$ && "${p12_hash}" =~ ^[0-9a-f]{64}$ ]] || [[ "${user_hash}" == "${p12_hash}" ]]; then
    upgrade_preflight_error "agent.env must configure distinct lowercase SHA-256 fingerprints for both password sealing public keys"
  fi
  for purpose in user p12; do
    if [[ "${purpose}" == user ]]; then
      key_path="${SYSCONFDIR}/ocservia-agent/user-password-seal-private.pem"
      key_id="${user_id}"
      expected_hash="${user_hash}"
    else
      key_path="${SYSCONFDIR}/ocservia-agent/p12-password-seal-private.pem"
      key_id="${p12_id}"
      expected_hash="${p12_hash}"
    fi
    validate_safe_ancestry "$(dirname -- "${key_path}")"
    key_file="${DESTDIR}${key_path}"
    if [[ ! -f "${key_file}" || -L "${key_file}" ]]; then
      upgrade_preflight_error "${purpose} password sealing private key must be provisioned through a trusted channel before upgrade"
    fi
    read -r uid gid mode links size < <(stat -c '%u %g %a %h %s' -- "${key_file}")
    if [[ "${uid}" != 0 || "${gid}" != 0 || "${links}" != 1 || "${size}" -lt 256 || "${size}" -gt 32768 ]] || ! { [[ "${mode}" == 400 ]] || [[ "${mode}" == 600 ]]; }; then
      upgrade_preflight_error "${key_path} must be a one-link root:root regular file mode 0400 or 0600"
    fi
    if ! actual_hash="$(openssl rsa -in "${key_file}" -pubout -outform DER 2>/dev/null | sha256sum | cut -d' ' -f1)" || [[ "${actual_hash}" != "${expected_hash}" || -z "${key_id}" ]]; then
      upgrade_preflight_error "${purpose} password sealing private key does not match its enrolled public-key fingerprint"
    fi
  done
}

bind_legacy_password_sealing_keys() {
  local agent_env="${DESTDIR}${SYSCONFDIR}/ocservia-agent/agent.env"
  local installed_unit="$1"
  local marker="${DESTDIR}${SYSCONFDIR}/ocservia-agent/sealing-keys-bound"
  local controller node_id user_id user_hash p12_id p12_hash expected actual response
  local marker_uid marker_gid marker_mode marker_links
  if grep -Fq -- '--user-password-seal-key-id' "${installed_unit}" && \
    grep -Fq -- '--p12-password-seal-key-id' "${installed_unit}"; then
    return
  fi
  controller="$(sed -n 's/^CONTROLLER_ENDPOINT_ID=//p' "${agent_env}")"
  node_id="$(sed -n 's/^NODE_ID=//p' "${agent_env}")"
  user_id="$(sed -n 's/^USER_PASSWORD_SEAL_KEY_ID=//p' "${agent_env}")"
  user_hash="$(sed -n 's/^USER_PASSWORD_SEAL_PUBLIC_KEY_SHA256=//p' "${agent_env}")"
  p12_id="$(sed -n 's/^P12_PASSWORD_SEAL_KEY_ID=//p' "${agent_env}")"
  p12_hash="$(sed -n 's/^P12_PASSWORD_SEAL_PUBLIC_KEY_SHA256=//p' "${agent_env}")"
  if ! [[ "${controller}" =~ ^[0-9a-f]{64}$ && "${node_id}" =~ ^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$ ]]; then
    upgrade_preflight_error "legacy sealing-key enrollment requires valid Controller EndpointID and node UUIDv7"
  fi
  expected="$(printf 'node_id=%s\nuser_key_id=%s\nuser_sha256=%s\np12_key_id=%s\np12_sha256=%s' "${node_id}" "${user_id}" "${user_hash}" "${p12_id}" "${p12_hash}")"
  if [[ -f "${marker}" && ! -L "${marker}" ]]; then
    read -r marker_uid marker_gid marker_mode marker_links < <(stat -c '%u %g %a %h' -- "${marker}")
    actual="$(cat -- "${marker}")"
    if [[ "${marker_uid}" == 0 && "${marker_gid}" == 0 && "${marker_mode}" == 600 && "${marker_links}" == 1 && "${actual}" == "${expected}" ]]; then
      return
    fi
    upgrade_preflight_error "the existing sealing-key enrollment marker is unsafe or does not match agent.env"
  fi
  if [[ -n "${DESTDIR}" ]]; then
    if [[ "${ENROLLMENT_MIGRATION_CONFIRMED}" != true ]]; then
      upgrade_preflight_error "legacy package migration must exercise or explicitly simulate one-time sealing-key enrollment"
    fi
  else
    if [[ "${ENROLLMENT_TOKEN_FILE}" != /* || ! "${ENROLLMENT_ENVIRONMENT}" =~ ^[A-Za-z0-9_.-]{1,64}$ ]]; then
      upgrade_preflight_error "legacy package migration requires absolute ENROLLMENT_TOKEN_FILE and ENROLLMENT_ENVIRONMENT"
    fi
    response="$(runuser -u ocserv-agent -- "${ROOT}/rust/target/release/ocservia-agent" \
      --identity-dir "${STATE_DIR}/identity" \
      --controller "${controller}" \
      --enrollment-token-file "${ENROLLMENT_TOKEN_FILE}" \
      --enrollment-environment "${ENROLLMENT_ENVIRONMENT}" \
      --user-password-seal-key-id "${user_id}" \
      --user-password-seal-public-key-sha256 "${user_hash}" \
      --p12-password-seal-key-id "${p12_id}" \
      --p12-password-seal-public-key-sha256 "${p12_hash}")" || \
      upgrade_preflight_error "Controller rejected the existing node sealing-key enrollment"
    if [[ "${response}" != "${node_id}" ]]; then
      upgrade_preflight_error "sealing-key enrollment returned a different node identity"
    fi
  fi
  install -o root -g root -m 0600 -- /dev/null "${marker}"
  printf '%s\n' "${expected}" >"${marker}"
}

harden_legacy_artifact_spool() {
  local installed_unit="$1"
  local directory="${DESTDIR}${PRIVD_STATE_DIR}/certificates/artifacts"
  local uid mode entry name ancestor ancestor_uid ancestor_mode
  if grep -Fq -- '--p12-password-seal-key-id' "${installed_unit}"; then
    return
  fi
  if [[ ! -e "${directory}" && ! -L "${directory}" ]]; then
    return
  fi
  if [[ ! -d "${directory}" || -L "${directory}" ]]; then
    installed_pair_preflight_error "legacy P12 artifact spool must be a real directory"
  fi
  for ancestor in \
    "${DESTDIR}${PRIVD_STATE_DIR}" \
    "${DESTDIR}${PRIVD_STATE_DIR}/certificates" \
    "${directory}"; do
    if [[ ! -d "${ancestor}" || -L "${ancestor}" ]]; then
      installed_pair_preflight_error "legacy P12 artifact ancestry must contain only real directories"
    fi
    read -r ancestor_uid ancestor_mode < <(stat -c '%u %a' -- "${ancestor}")
    if [[ "${ancestor_uid}" != 0 ]] || (( (8#${ancestor_mode} & 8#022) != 0 )); then
      installed_pair_preflight_error "legacy P12 artifact ancestry has unsafe ownership or mode"
    fi
  done
  read -r uid mode < <(stat -c '%u %a' -- "${directory}")
  if [[ "${uid}" != 0 ]]; then
    installed_pair_preflight_error "legacy P12 artifact spool must be root-owned"
  fi
  # Remove group traversal before examining any legacy file. Pre-P1-06
  # packages have no authenticated root ledger, so every recognized artifact
  # is untrusted and must be removed without an mtime grace period.
  chmod 0700 -- "${directory}"
  for entry in "${directory}"/* "${directory}"/.*; do
    [[ -e "${entry}" || -L "${entry}" ]] || continue
    name="$(basename -- "${entry}")"
    [[ "${name}" != . && "${name}" != .. ]] || continue
    if [[ "${name}" =~ ^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}\.p12$ || \
          "${name}" =~ ^\.[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}\.(p12|chain\.pem)$ ]]; then
      if [[ ! -f "${entry}" || -L "${entry}" ]] || [[ "$(stat -c '%u:%h' -- "${entry}")" != "0:1" ]]; then
        installed_pair_preflight_error "legacy P12 artifact entry ${name} has unsafe type, owner, or link count"
      fi
      rm -f -- "${entry}"
    fi
  done
  mode="$(stat -c '%a' -- "${directory}")"
  [[ "${mode}" == 700 ]] || installed_pair_preflight_error "legacy P12 artifact spool could not be hardened"
  sync -f "${directory}"
}

if [[ ${EUID} -ne 0 ]]; then
  echo "upgrade-agent.sh must run as root" >&2
  exit 1
fi
validate_verified_package_source

if [[ -n "${DESTDIR}" && ( "${DESTDIR}" != /* || "${DESTDIR}" == "/" || "${DESTDIR}" == */ ) ]] || \
  [[ "${PREFIX}" != /* || "${SYSCONFDIR}" != /* || "${STATE_DIR}" != /* || "${PRIVD_STATE_DIR}" != /* || "${BACKUP_DIR}" != /* ]]; then
  echo "DESTDIR, PREFIX, SYSCONFDIR, STATE_DIR, PRIVD_STATE_DIR, and BACKUP_DIR must identify absolute paths" >&2
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
validate_password_sealing_keys

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

validate_installed_snapshot_source "${installed_agent}" 755
validate_installed_snapshot_source "${installed_privd}" 755
validate_installed_snapshot_source "${installed_agent_unit}" 644
validate_installed_snapshot_source "${installed_privd_unit}" 644
if [[ -f "${installed_relay_dropin}" ]]; then
  validate_installed_snapshot_source "${installed_relay_dropin}" 644
fi

# A pre-P1-06 node has no Controller-side sealing-key binding. Use the verified
# new Agent binary to bind both descriptors through the existing EndpointID and
# a fresh operator-issued token only after every rollback source is known-safe.
# A strict root-owned marker makes a later rollback/re-upgrade deterministic.
bind_legacy_password_sealing_keys "${installed_agent_unit}"
harden_legacy_artifact_spool "${installed_privd_unit}"

ensure_root_private_directory "${DESTDIR}${PRIVD_STATE_DIR}"
ensure_root_private_directory "${BACKUP_DIR}"
validate_root_ancestry "${BACKUP_DIR}"
if [[ "$(stat -c '%u:%g:%a' -- "${DESTDIR}${PRIVD_STATE_DIR}")" != "0:0:700" || \
      "$(stat -c '%u:%g:%a' -- "${BACKUP_DIR}")" != "0:0:700" ]]; then
  installed_pair_preflight_error "privd state and upgrade backup directories must be root:root mode 0700"
fi
rm -f -- "${BACKUP_DIR}/MANIFEST.sha256"
install -o root -g root -m 0755 -- "${installed_agent}" "${BACKUP_DIR}/ocservia-agent.previous"
install -o root -g root -m 0755 -- "${installed_privd}" "${BACKUP_DIR}/ocservia-privd.previous"
install -o root -g root -m 0644 -- "${installed_agent_unit}" "${BACKUP_DIR}/ocservia-agent.service.previous"
install -o root -g root -m 0644 -- "${installed_privd_unit}" "${BACKUP_DIR}/ocservia-privd.service.previous"
rm -f -- "${BACKUP_DIR}/ocservia-agent-relays.conf.previous" \
  "${BACKUP_DIR}/ocservia-agent-relays.conf.absent"
if [[ -f "${installed_relay_dropin}" ]]; then
  install -o root -g root -m 0644 -- "${installed_relay_dropin}" \
    "${BACKUP_DIR}/ocservia-agent-relays.conf.previous"
else
  install -o root -g root -m 0600 -- /dev/null "${BACKUP_DIR}/ocservia-agent-relays.conf.absent"
fi
write_snapshot_manifest "${BACKUP_DIR}"

"${ROOT}/scripts/install-agent.sh"
if [[ -z "${DESTDIR}" ]]; then
  systemctl try-restart ocservia-privd.service ocservia-agent.service
fi
