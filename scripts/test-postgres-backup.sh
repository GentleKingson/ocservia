#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WRITER="${ROOT}/scripts/postgres-backup.sh"

stat -c '%u' . >/dev/null 2>&1 || {
  echo "PostgreSQL backup unit tests skipped: GNU stat is unavailable" >&2
  exit 0
}
[[ -x "${WRITER}" ]] || {
  echo "PostgreSQL backup unit tests require an executable postgres-backup.sh" >&2
  exit 1
}

fixture="$(mktemp -d "${TMPDIR:-${HOME}}/.ocservia-postgres-backup-test.XXXXXX")"
work="${fixture}/work"
writer_bin="${fixture}/writer-bin"
writer_log="${fixture}/writer.log"
RUN_STATUS=0
RUN_OUTPUT=""

can_root() {
  (( EUID == 0 )) || sudo -n true >/dev/null 2>&1
}

as_root() {
  if (( EUID == 0 )); then
    "$@"
  else
    sudo -n "$@"
  fi
}

cleanup() {
  local status=$?
  trap - EXIT INT TERM
  if can_root; then
    as_root rm -rf -- "${fixture}"
  else
    rm -rf -- "${fixture}"
  fi
  exit "${status}"
}
trap cleanup EXIT INT TERM

die() {
  echo "PostgreSQL backup unit tests: $1" >&2
  [[ -n "${RUN_OUTPUT:-}" ]] && printf '%s\n' "${RUN_OUTPUT}" >&2
  exit 1
}

assert_status() {
  (( RUN_STATUS == "$1" )) || die "${2:-unexpected writer status}: expected $1, got ${RUN_STATUS}"
}

assert_output() {
  grep -q -- "$1" <<<"${RUN_OUTPUT}" || die "expected output to contain '$1'"
}

assert_log_empty() {
  local log
  for log in "$@"; do
    [[ ! -s "${log}" ]] || die "expected ${log} to stay empty"
  done
}

install -d -m 0755 -- "${work}"
mkdir -m 700 -- "${writer_bin}"
cat >"${writer_bin}/psql" <<'EOF'
#!/usr/bin/env bash
printf '180006\n'
EOF
cat >"${writer_bin}/pg_basebackup" <<'EOF'
#!/usr/bin/env bash
echo pg_basebackup >>"${WRITER_LOG}"
pgdata="" checkpoint=""
for arg in "$@"; do
  case "${arg}" in
    --pgdata=*) pgdata="${arg#--pgdata=}" ;;
    --checkpoint=*) checkpoint="${arg#--checkpoint=}" ;;
  esac
done
[[ "${checkpoint}" == spread ]] || exit 1
mkdir -m 0700 -- "${pgdata}"
printf 'START WAL LOCATION: 0/01000028 (file 000000010000000000000001)\n' >"${pgdata}/backup_label"
printf '{"PostgreSQL-Backup-Manifest-Version":1}\n' >"${pgdata}/backup_manifest"
EOF
cat >"${writer_bin}/pg_verifybackup" <<'EOF'
#!/usr/bin/env bash
exit "${WRITER_VERIFY_STATUS:-0}"
EOF
cat >"${writer_bin}/date" <<'EOF'
#!/usr/bin/env bash
if [[ "$*" == "-u +%Y%m%dT%H%M%SZ" ]]; then echo 20260101T000000Z; else exec "${WRITER_REAL_DATE}" "$@"; fi
EOF
chmod 0755 -- "${writer_bin}/psql" "${writer_bin}/pg_basebackup" "${writer_bin}/pg_verifybackup" "${writer_bin}/date"

run_writer() {
  RUN_STATUS=0
  : >"${writer_log}"
  RUN_OUTPUT="$(env PATH="${writer_bin}:${PATH}" WRITER_LOG="${writer_log}" WRITER_REAL_DATE="$(command -v date)" BACKUP_ROOT="$1" \
    RUN_ID=writer-test "${WRITER}" --once 2>&1)" || RUN_STATUS=$?
}
assert_not_dumped() { assert_log_empty "${writer_log}"; }

wroot="${work}/writer"
install -d -m 0700 -- "${wroot}"
install -d -m 0755 -- "${wroot}/loose"
run_writer "${wroot}/loose"
assert_status 1 "a writer root with a loose mode must fail"
assert_output "mode 0700"
assert_not_dumped
install -d -m 0700 -- "${wroot}/loose-base" "${wroot}/loose-base/base"
chmod 0750 -- "${wroot}/loose-base/base"
run_writer "${wroot}/loose-base"
assert_status 1 "a base directory with a loose mode must fail"
assert_not_dumped
install -d -m 0700 -- "${wroot}/link-target" "${wroot}/link-base"
ln -s -- ../link-target "${wroot}/link-base/base"
run_writer "${wroot}/link-base"
assert_status 1 "a symlinked base directory must fail"
assert_not_dumped
ln -s -- link-target "${wroot}/link-root"
run_writer "${wroot}/link-root"
assert_status 1 "a symlinked writer root must fail"
assert_not_dumped
if can_root; then
  install -d -m 0700 -- "${wroot}/foreign"
  as_root chown 999:999 -- "${wroot}/foreign"
  if (( EUID == 0 )); then
    run_writer "${wroot}/foreign"
    assert_status 1 "a writer root owned by another user must fail"
    assert_not_dumped
  fi
fi
echo "the backup writer rejects unprotected paths before backing up"

install -d -m 0700 -- "${wroot}/staging" "${wroot}/staging/base"
mkdir -- "${wroot}/staging/base/.20260101T000000Z-writer-test.tmp"
printf 'keep\n' >"${wroot}/staging/base/.20260101T000000Z-writer-test.tmp/sentinel"
run_writer "${wroot}/staging"
assert_status 1 "an existing staging directory must be refused"
assert_not_dumped
[[ "$(cat -- "${wroot}/staging/base/.20260101T000000Z-writer-test.tmp/sentinel")" == keep ]] ||
  die "an existing staging directory was reused or removed"
flock -n "${wroot}/staging/.backup.lock" true || die "a refused backup held its lock"
install -d -m 0700 -- "${wroot}/staging-link" "${wroot}/staging-link/base"
ln -s -- "${wroot}/link-target" "${wroot}/staging-link/base/.20260101T000000Z-writer-test.tmp"
run_writer "${wroot}/staging-link"
assert_status 1 "a symlinked staging path must be refused"
assert_not_dumped
[[ -L "${wroot}/staging-link/base/.20260101T000000Z-writer-test.tmp" ]] || die "a symlinked staging path was removed"
install -d -m 0700 -- "${wroot}/final" "${wroot}/final/base"
ln -s -- "${wroot}/link-target" "${wroot}/final/base/20260101T000000Z"
run_writer "${wroot}/final"
assert_status 1 "an existing output object must be refused"
assert_not_dumped
install -d -m 0700 -- "${wroot}/latest" "${wroot}/latest/base" "${wroot}/latest/LATEST"
run_writer "${wroot}/latest"
assert_status 1 "a LATEST directory must be refused"
assert_not_dumped

install -d -m 0700 -- "${wroot}/ok"
run_writer "${wroot}/ok"
assert_status 0 "a protected writer root must back up"
assert_output "backup completed id=20260101T000000Z"
[[ "$(stat -c '%a' "${wroot}/ok/base")" == 700 && ! -e "${wroot}/ok/wal" ]] || die "the writer must protect the base directory"
[[ "$(cat -- "${wroot}/ok/LATEST")" == 20260101T000000Z ]] || die "the LATEST marker was not published"
[[ "$(tar -xOzf "${wroot}/ok/base/20260101T000000Z/base.tar.gz" ./backup_manifest)" == '{"PostgreSQL-Backup-Manifest-Version":1}' &&
  "$(cat -- "${wroot}/ok/base/20260101T000000Z/OCSERVIA_BACKUP_ID")" == 20260101T000000Z ]] ||
  die "the backup files were not published"
[[ -z "$(find "${wroot}/ok" -name '.LATEST*' -o -name '*.tmp')" ]] || die "the writer left temporary objects"
echo "the backup writer backs up under a protected path"

install -d -m 0700 -- "${wroot}/failed-verification"
export WRITER_VERIFY_STATUS=1
run_writer "${wroot}/failed-verification"
assert_status 1 "one-shot verification failures must fail"
[[ ! -e "${wroot}/failed-verification/LATEST" && -z "$(ls -A "${wroot}/failed-verification/base")" ]] ||
  die "an unverified backup was published or left behind"
# A controlled sleep proves the loop reaches its retry delay rather than
# exiting immediately or attempting to publish a failed backup.
cat >"${writer_bin}/sleep" <<'EOF'
#!/usr/bin/env bash
printf 'backup worker sleep=%s\n' "$1"
exit 42
EOF
chmod 0755 "${writer_bin}/sleep"
RUN_STATUS=0
RUN_OUTPUT="$(env PATH="${writer_bin}:${PATH}" WRITER_LOG="${writer_log}" WRITER_REAL_DATE="$(command -v date)" \
  BACKUP_ROOT="${wroot}/failed-verification" RUN_ID=writer-test "${WRITER}" 2>&1)" || RUN_STATUS=$?
assert_status 42 "verification failure in the loop must reach the retry delay"
assert_output "backup attempt failed; retrying in 900 seconds"
assert_output "backup worker sleep=900"
unset WRITER_VERIFY_STATUS
