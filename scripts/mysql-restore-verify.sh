#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: $0 --backend <mysql|mariadb> --backup-dir <absolute-path> --target-config <mode-0600.cnf> [--old-writers-fenced]" >&2
  exit 2
}

backend="" backup_dir="" target_config="" old_writers_fenced=false
while (($#)); do
  case "$1" in
    --backend) (($# >= 2)) || usage; backend="$2"; shift 2 ;;
    --backup-dir) (($# >= 2)) || usage; backup_dir="$2"; shift 2 ;;
    --target-config) (($# >= 2)) || usage; target_config="$2"; shift 2 ;;
    --old-writers-fenced) old_writers_fenced=true; shift ;;
    *) usage ;;
  esac
done
case "${backend}" in mysql|mariadb) ;; *) usage ;; esac
if [[ "${backend}" == mysql ]]; then client=mysql; else client=mariadb; fi
[[ "${backup_dir}" == /* && -d "${backup_dir}" && ! -L "${backup_dir}" ]] || usage
[[ -f "${target_config}" && ! -L "${target_config}" && "$(stat -c %a "${target_config}")" == 600 ]] || usage
for file in database.sql metadata SHA256SUMS; do
  [[ -f "${backup_dir}/${file}" && ! -L "${backup_dir}/${file}" ]] || { echo "backup artifact is missing or unsafe: ${file}" >&2; exit 1; }
done
if [[ "$(wc -l <"${backup_dir}/SHA256SUMS")" != 2 ]] ||
  ! awk 'NF == 2 && $1 ~ /^[0-9a-f]{64}$/ && ($2 == "database.sql" || $2 == "metadata") {seen[$2]++} END {exit !(seen["database.sql"] == 1 && seen["metadata"] == 1)}' \
    "${backup_dir}/SHA256SUMS"; then
  echo "backup checksum manifest is invalid" >&2
  exit 1
fi
(cd "${backup_dir}" && sha256sum -c SHA256SUMS)

metadata_backend="$(sed -n 's/^backend=//p' "${backup_dir}/metadata")"
database="$(sed -n 's/^database=//p' "${backup_dir}/metadata")"
[[ "${metadata_backend}" == "${backend}" && "${database}" =~ ^[A-Za-z0-9_]{1,64}$ ]] || {
  echo "backup metadata does not match the requested backend" >&2; exit 1;
}
existing="$("${client}" --defaults-extra-file="${target_config}" --batch --skip-column-names \
  -e "SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME='${database}'")"
[[ "${existing}" == 0 ]] || { echo "isolated restore target database already exists" >&2; exit 1; }
"${client}" --defaults-extra-file="${target_config}" <"${backup_dir}/database.sql"

"${client}" --defaults-extra-file="${target_config}" --database="${database}" --batch --skip-column-names <<'SQL'
SELECT IF((SELECT COUNT(*) FROM controller_schema_compatibility)=1,'schema:ok','schema:failed');
SELECT IF((SELECT COUNT(*) FROM audit_events WHERE event_hash IS NULL OR event_mac IS NULL)=0,'audit-shape:ok','audit-shape:failed');
SELECT CONCAT('identities:',COUNT(*)) FROM identities;
SELECT CONCAT('scheduler_epoch:',epoch) FROM scheduler_leadership WHERE id=1;
SELECT CONCAT('operations_unfinished:',COUNT(*)) FROM operations WHERE state NOT IN ('succeeded','failed','unknown','expired','rolled_back','superseded');
SELECT CONCAT('commands_unfinished:',COUNT(*)) FROM commands WHERE state NOT IN ('succeeded','failed','rejected','unknown','expired','rolled_back','superseded');
SELECT CONCAT('idempotency_records:',COUNT(*)) FROM operations WHERE idempotency_key IS NOT NULL;
SQL

if [[ "${old_writers_fenced}" != true ]]; then
  echo "restore verified structurally; command authority remains blocked until old writers are fenced and audit authenticity is checked by an isolated Controller" >&2
  exit 3
fi
lease_live="$("${client}" --defaults-extra-file="${target_config}" --database="${database}" --batch --skip-column-names \
  -e "SELECT COUNT(*) FROM scheduler_leadership WHERE id=1 AND lease_until > UTC_TIMESTAMP(6)")"
[[ "${lease_live}" == 0 ]] || { echo "restored scheduler authority lease is still live; commands remain blocked" >&2; exit 3; }
echo "restore data checks passed; keep command authority blocked until isolated Controller audit verification and operator cutover are complete"
