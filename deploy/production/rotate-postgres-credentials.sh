#!/usr/bin/env bash
set -Eeuo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COMPOSE_FILE="${OCSERV_ROTATION_COMPOSE_FILE:-${ROOT}/deploy/production/compose.yaml}"
secret_dir="${OCSERV_SECRET_DIR:-}"
new_app_file="${OCSERV_NEW_POSTGRES_APP_PASSWORD_FILE:-}"
new_backup_file="${OCSERV_NEW_POSTGRES_BACKUP_PASSWORD_FILE:-}"
terminate_sessions="${OCSERV_TERMINATE_OLD_POSTGRES_SESSIONS:-false}"
wait_timeout="${OCSERV_ROTATION_WAIT_TIMEOUT_SECONDS:-180}"
lock_timeout="${OCSERV_ROTATION_LOCK_TIMEOUT_SECONDS:-300}"
launcher_uid="$(id -u)"
recovery_dir=""
rotation_lock_fd=""
database_changed=false
files_changed=false
recovering=false

fail() {
  echo "$1" >&2
  return 1
}

validate_private_directory() {
  local path="$1"
  [[ -n "${path}" && -d "${path}" && ! -L "${path}" ]] || fail "credential directory must be a real directory"
  [[ "$(stat -c '%u:%a' "${path}")" == "${launcher_uid}:700" ]] || fail "credential directory must be launcher-owned with mode 0700"
}

validate_current_secret() {
  local path="$1"
  [[ -f "${path}" && ! -L "${path}" ]] || fail "current credential secret must be a regular file"
  [[ "$(stat -c '%u:%a' "${path}")" == "${launcher_uid}:444" ]] || fail "current credential secret must be launcher-owned with mode 0444"
}

validate_new_secret() {
  local path="$1" parent
  [[ -n "${path}" && "${path}" == /* && -f "${path}" && ! -L "${path}" ]] || fail "new credential must be an absolute regular-file path"
  [[ "$(realpath -e -- "${path}")" == "${path}" ]] || fail "new credential path must be canonical and contain no symlink"
  parent="$(dirname -- "${path}")"
  validate_private_directory "${parent}"
  local metadata
  metadata="$(stat -c '%u:%a:%h' "${path}")"
  [[ "${metadata}" == "${launcher_uid}:400:1" || "${metadata}" == "${launcher_uid}:600:1" ]] || fail "new credential must be a single-link launcher-owned file with mode 0400 or 0600"
}

acquire_rotation_lock() {
  local lock_path="${secret_dir}/.postgres-credential-rotation.lock" metadata
  command -v flock >/dev/null 2>&1 || fail "flock is required for PostgreSQL credential rotation"
  if [[ -e "${lock_path}" || -L "${lock_path}" ]]; then
    [[ -f "${lock_path}" && ! -L "${lock_path}" ]] || fail "credential rotation lock must be a regular file"
    metadata="$(stat -c '%u:%a:%h' "${lock_path}")"
    [[ "${metadata}" == "${launcher_uid}:600:1" ]] || fail "credential rotation lock must be a single-link launcher-owned file with mode 0600"
  fi
  umask 077
  exec {rotation_lock_fd}>"${lock_path}"
  chmod 0600 "${lock_path}"
  metadata="$(stat -c '%u:%a:%h' "${lock_path}")"
  [[ "${metadata}" == "${launcher_uid}:600:1" ]] || fail "credential rotation lock metadata changed unexpectedly"
  flock --exclusive --timeout "${lock_timeout}" "${rotation_lock_fd}" || fail "another PostgreSQL credential rotation is still in progress"
}

read_password() {
  local path="$1" value byte_count
  value="$(cat -- "${path}")"
  byte_count="$(wc -c <"${path}" | tr -d '[:space:]')"
  [[ -n "${value}" ]] || fail "PostgreSQL password must not be empty"
  [[ "${value}" != *$'\n'* && "${value}" != *$'\r'* ]] || fail "PostgreSQL password contains a line break"
  if (( byte_count != ${#value} && byte_count != ${#value} + 1 )); then
    fail "PostgreSQL password file must contain one ASCII line"
  fi
  [[ "${value}" != *[!\ -~]* ]] || fail "PostgreSQL password must contain printable ASCII only"
  printf '%s' "${value}"
}

urlencode() {
  local value="$1" output="" character hex index
  LC_ALL=C
  for ((index = 0; index < ${#value}; index++)); do
    character="${value:index:1}"
    case "${character}" in
      [a-zA-Z0-9.~_-]) output+="${character}" ;;
      *)
        printf -v hex '%02X' "'${character}"
        output+="%${hex}"
        ;;
    esac
  done
  printf '%s' "${output}"
}

pgpass_escape() {
  local value="$1"
  value="${value//\\/\\\\}"
  value="${value//:/\\:}"
  printf '%s' "${value}"
}

compose() {
  docker compose -f "${COMPOSE_FILE}" "$@"
}

alter_roles() {
  local app_password="$1" backup_password="$2" app_encoded backup_encoded
  app_encoded="$(printf '%s' "${app_password}" | base64 | tr -d '\n')"
  backup_encoded="$(printf '%s' "${backup_password}" | base64 | tr -d '\n')"
  if ! {
    printf "\\set app_encoded '%s'\n" "${app_encoded}"
    printf "\\set backup_encoded '%s'\n" "${backup_encoded}"
    cat <<'SQL'
BEGIN;
SELECT format('ALTER ROLE ocservia_app PASSWORD %L', convert_from(decode(:'app_encoded', 'base64'), 'UTF8')) \gexec
SELECT format('ALTER ROLE ocservia_backup PASSWORD %L', convert_from(decode(:'backup_encoded', 'base64'), 'UTF8')) \gexec
COMMIT;
SQL
  } | compose exec -T postgres psql --set=ON_ERROR_STOP=1 --username ocservia_owner --dbname ocservia >/dev/null 2>&1; then
    echo "PostgreSQL role verifier update failed" >&2
    return 1
  fi
}

verify_login() {
  local role="$1" password="$2" database="$3"
  # The single-quoted program is evaluated inside the PostgreSQL container.
  # shellcheck disable=SC2016
  printf '127.0.0.1:5432:%s:%s:%s\n' "${database}" "${role}" "$(pgpass_escape "${password}")" |
    compose exec -T postgres sh -ceu '
      passfile="$(mktemp)"
      trap '\''rm -f -- "${passfile}"'\'' EXIT
      chmod 0600 "${passfile}"
      cat >"${passfile}"
      PGPASSFILE="${passfile}" psql --no-password --host 127.0.0.1 --username "$1" --dbname "$2" --tuples-only --command "SELECT 1" >/dev/null
    ' -- "${role}" "${database}"
}

verify_rejected() {
  local role="$1" password="$2" database="$3"
  if verify_login "${role}" "${password}" "${database}" >/dev/null 2>&1; then
    fail "an old PostgreSQL credential still establishes a new connection"
  fi
}

atomic_secret() {
  local destination="$1" value="$2" temporary
  temporary="$(mktemp "${secret_dir}/.rotation.XXXXXX")"
  printf '%s\n' "${value}" >"${temporary}"
  chmod 0444 "${temporary}"
  mv -f -- "${temporary}" "${destination}"
}

restart_clients() {
  compose up -d --force-recreate --no-deps --wait --wait-timeout "${wait_timeout}" control-plane backup >/dev/null
}

restore_files() {
  local name
  for name in postgres-app-password postgres-backup-password database-app-url postgres.pgpass; do
    atomic_secret "${secret_dir}/${name}" "$(cat -- "${recovery_dir}/${name}")"
  done
}

credential_pair_active() {
  local app_password="$1" backup_password="$2"
  verify_login ocservia_app "${app_password}" ocservia >/dev/null 2>&1 &&
    verify_login ocservia_backup "${backup_password}" postgres >/dev/null 2>&1
}

classify_database_state() {
  if credential_pair_active "${new_app_password}" "${new_backup_password}" &&
    ! verify_login ocservia_app "${old_app_password}" ocservia >/dev/null 2>&1 &&
    ! verify_login ocservia_backup "${old_backup_password}" postgres >/dev/null 2>&1; then
    printf '%s' new
    return 0
  fi
  if credential_pair_active "${old_app_password}" "${old_backup_password}" &&
    ! verify_login ocservia_app "${new_app_password}" ocservia >/dev/null 2>&1 &&
    ! verify_login ocservia_backup "${new_backup_password}" postgres >/dev/null 2>&1; then
    printf '%s' old
    return 0
  fi
  return 1
}

file_matches_either() {
  local path="$1" first="$2" second="$3" value
  validate_current_secret "${path}" >/dev/null 2>&1 || return 1
  value="$(cat -- "${path}")"
  [[ "${value}" == "${first}" || "${value}" == "${second}" ]]
}

files_match_rotation_state() {
  file_matches_either "${secret_dir}/postgres-app-password" "${old_app_password}" "${new_app_password}" &&
    file_matches_either "${secret_dir}/postgres-backup-password" "${old_backup_password}" "${new_backup_password}" &&
    file_matches_either "${secret_dir}/database-app-url" "${current_app_url}" "${new_app_url}" &&
    file_matches_either "${secret_dir}/postgres.pgpass" "${current_pgpass}" "${new_pgpass}"
}

recover() {
  local status=$? database_state="old" recovery_safe=true
  trap - ERR
  [[ "${recovering}" == false ]] || exit "${status}"
  recovering=true
  set +e
  if [[ "${database_changed}" == true ]]; then
    if ! database_state="$(classify_database_state)"; then
      recovery_safe=false
    fi
  fi
  if [[ "${files_changed}" == true ]] && ! files_match_rotation_state; then
    recovery_safe=false
  fi
  if [[ "${recovery_safe}" == true && "${database_state}" == new ]]; then
    alter_roles "${old_app_password}" "${old_backup_password}"
    database_recovered=$?
  elif [[ "${recovery_safe}" == true ]]; then
    database_recovered=0
  else
    database_recovered=1
  fi
  if [[ "${recovery_safe}" == true && "${files_changed}" == true ]]; then
    restore_files
    files_recovered=$?
  elif [[ "${recovery_safe}" == true ]]; then
    files_recovered=0
  else
    files_recovered=1
  fi
  if (( database_recovered == 0 && files_recovered == 0 )); then
    restart_clients
    clients_recovered=$?
    verify_login ocservia_app "${old_app_password}" ocservia
    app_recovered=$?
    verify_login ocservia_backup "${old_backup_password}" postgres
    backup_recovered=$?
  else
    clients_recovered=1
    app_recovered=1
    backup_recovered=1
  fi
  if (( database_recovered == 0 && files_recovered == 0 && clients_recovered == 0 && app_recovered == 0 && backup_recovered == 0 )); then
    rm -rf -- "${recovery_dir}"
    echo "PostgreSQL credential rotation failed; the previous verifier, secrets, and clients were restored" >&2
  else
    echo "PostgreSQL credential rotation failed and automatic recovery is incomplete; keep services stopped and recover from the protected snapshot at ${recovery_dir}" >&2
  fi
  exit "${status}"
}

[[ "${terminate_sessions}" == "true" || "${terminate_sessions}" == "false" ]] || fail "OCSERV_TERMINATE_OLD_POSTGRES_SESSIONS must be true or false"
[[ "${wait_timeout}" =~ ^[1-9][0-9]*$ ]] || fail "OCSERV_ROTATION_WAIT_TIMEOUT_SECONDS must be a positive integer"
[[ "${lock_timeout}" =~ ^[1-9][0-9]*$ ]] || fail "OCSERV_ROTATION_LOCK_TIMEOUT_SECONDS must be a positive integer"
[[ -f "${COMPOSE_FILE}" && ! -L "${COMPOSE_FILE}" ]] || fail "rotation compose file must be a regular file"
validate_private_directory "${secret_dir}"
acquire_rotation_lock
for name in postgres-app-password postgres-backup-password database-app-url postgres.pgpass; do
  validate_current_secret "${secret_dir}/${name}"
done
validate_new_secret "${new_app_file}"
validate_new_secret "${new_backup_file}"

old_app_password="$(read_password "${secret_dir}/postgres-app-password")"
old_backup_password="$(read_password "${secret_dir}/postgres-backup-password")"
new_app_password="$(read_password "${new_app_file}")"
new_backup_password="$(read_password "${new_backup_file}")"
[[ "${new_app_password}" != "${old_app_password}" ]] || fail "new application password must differ from the current password"
[[ "${new_backup_password}" != "${old_backup_password}" ]] || fail "new backup password must differ from the current password"

current_app_url="$(cat -- "${secret_dir}/database-app-url")"
current_pgpass="$(cat -- "${secret_dir}/postgres.pgpass")"
[[ "${current_app_url}" == postgres://ocservia_app:*@* ]] || fail "database-app-url does not contain the expected application role"
database_suffix="${current_app_url#*@}"
new_app_url="postgres://ocservia_app:$(urlencode "${new_app_password}")@${database_suffix}"
new_pgpass="postgres:5432:replication:ocservia_backup:$(pgpass_escape "${new_backup_password}")"

verify_login ocservia_app "${old_app_password}" ocservia
verify_login ocservia_backup "${old_backup_password}" postgres

recovery_dir="$(mktemp -d "${secret_dir}/.postgres-rotation-recovery.XXXXXX")"
chmod 0700 "${recovery_dir}"
for name in postgres-app-password postgres-backup-password database-app-url postgres.pgpass; do
  cp -- "${secret_dir}/${name}" "${recovery_dir}/${name}"
  chmod 0600 "${recovery_dir}/${name}"
done

trap recover ERR
database_changed=true
alter_roles "${new_app_password}" "${new_backup_password}"
if [[ "${terminate_sessions}" == true ]]; then
  compose exec -T postgres psql --set=ON_ERROR_STOP=1 --username ocservia_owner --dbname ocservia \
    --command "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE usename IN ('ocservia_app','ocservia_backup') AND pid <> pg_backend_pid()" >/dev/null
fi
verify_login ocservia_app "${new_app_password}" ocservia
verify_login ocservia_backup "${new_backup_password}" postgres
verify_rejected ocservia_app "${old_app_password}" ocservia
verify_rejected ocservia_backup "${old_backup_password}" postgres

files_changed=true
atomic_secret "${secret_dir}/postgres-app-password" "${new_app_password}"
atomic_secret "${secret_dir}/postgres-backup-password" "${new_backup_password}"
atomic_secret "${secret_dir}/database-app-url" "${new_app_url}"
atomic_secret "${secret_dir}/postgres.pgpass" "${new_pgpass}"
restart_clients

verify_login ocservia_app "${new_app_password}" ocservia
verify_login ocservia_backup "${new_backup_password}" postgres
verify_rejected ocservia_app "${old_app_password}" ocservia
verify_rejected ocservia_backup "${old_backup_password}" postgres

database_changed=false
files_changed=false
rm -rf -- "${recovery_dir}"
trap - ERR
unset old_app_password old_backup_password new_app_password new_backup_password
echo "PostgreSQL application and backup role credentials rotated successfully"
