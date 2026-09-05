#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fixture="$(mktemp -d)"
trap 'rm -rf -- "${fixture}"' EXIT
mkdir "${fixture}/bin"
printf 'database' >"${fixture}/agent.db"
printf 'wal' >"${fixture}/agent.db-wal"
printf 'shm' >"${fixture}/agent.db-shm"
cat >"${fixture}/bin/df" <<'SH'
#!/usr/bin/env bash
if [[ "${1}" == -Pi ]]; then
  printf 'Filesystem Inodes IUsed IFree IUse%% Mounted\nfixture 1000 100 900 %s%% /\n' "${TEST_INODE_PERCENT:-10}"
else
  printf 'Filesystem Bytes Used Available Use%% Mounted\nfixture 100000 10000 90000 %s%% /\n' "${TEST_DISK_PERCENT:-10}"
fi
SH
chmod +x "${fixture}/bin/df"
export PATH="${fixture}/bin:${PATH}"
assert_status() {
  local expected="$1" status=0
  shift
  "$@" >"${fixture}/output" 2>&1 || status=$?
  if [[ "${status}" != "${expected}" ]]; then
    cat "${fixture}/output" >&2
    echo "expected status ${expected}, got ${status}" >&2
    exit 1
  fi
}
before="$(sha256sum "${fixture}/agent.db"*)"
assert_status 0 bash "${ROOT}/scripts/check-agent-storage.sh" "${fixture}/agent.db"
grep -Fxq 'journal_bytes=14' "${fixture}/output"
grep -Fxq 'wal_bytes=3' "${fixture}/output"
assert_status 1 env TEST_DISK_PERCENT=80 bash "${ROOT}/scripts/check-agent-storage.sh" "${fixture}/agent.db"
assert_status 2 env TEST_DISK_PERCENT=90 bash "${ROOT}/scripts/check-agent-storage.sh" "${fixture}/agent.db"
assert_status 2 env TEST_INODE_PERCENT=95 bash "${ROOT}/scripts/check-agent-storage.sh" "${fixture}/agent.db"
assert_status 3 env TEST_DISK_PERCENT=- bash "${ROOT}/scripts/check-agent-storage.sh" "${fixture}/agent.db"
assert_status 3 env STORAGE_WARNING_PERCENT=90 STORAGE_CRITICAL_PERCENT=80 bash "${ROOT}/scripts/check-agent-storage.sh" "${fixture}/agent.db"
assert_status 3 bash "${ROOT}/scripts/check-agent-storage.sh" "${fixture}/missing.db"
ln -s "${fixture}/agent.db" "${fixture}/linked.db"
assert_status 3 bash "${ROOT}/scripts/check-agent-storage.sh" "${fixture}/linked.db"
[[ "$(sha256sum "${fixture}/agent.db"*)" == "${before}" ]]
rm "${fixture}/agent.db-wal" "${fixture}/agent.db-shm"
assert_status 0 bash "${ROOT}/scripts/check-agent-storage.sh" "${fixture}/agent.db"
grep -Fxq 'wal_bytes=0' "${fixture}/output"
echo "Agent storage probe tests passed"
