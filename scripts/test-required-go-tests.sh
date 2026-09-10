#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmp="$(mktemp -d "${TMPDIR:-/tmp}/required-go-tests-selftest-XXXXXX")"
trap 'rm -rf "${tmp}"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
check() {
  PR02_ENGINE="${3:-mariadb}" jq -se --arg group "${2:-database-api}" --rawfile manifest "${ROOT}/scripts/required-go-tests.txt" \
    -f "${ROOT}/scripts/check-required-go-tests.jq" "$1"
}
jq -n --rawfile manifest "${ROOT}/scripts/required-go-tests.txt" '
  [$manifest | split("\n")[] | split(" ") | select(.[0] == "database-api")
   | {Package: ("github.com/GentleKingson/ocservia/control-plane/" + .[1]), Test: .[2]}
   | . + {Action: "run"}, . + {Action: "pass"}][]
' >"${tmp}/pass.json"
check "${tmp}/pass.json"
for mutation in \
  'select(.Test != "TestAuthHTTPLoginLogoutIntegration")' \
  'select(.Action != "run")' \
  'if .Action == "pass" then .Action = "skip" else . end' \
  'if .Action == "pass" then .Action = "fail" else . end' \
  '.Test += "Renamed"' \
  'select(.Test != "TestAuthHTTPLoginLogoutIntegration/both/session-write-failure")' \
  'if .Test == "TestAuthHTTPLoginLogoutIntegration/both/session-write-failure" and .Action == "pass" then .Action = "skip" else . end' \
  'empty'; do
  jq -c "${mutation}" "${tmp}/pass.json" >"${tmp}/bad.json"
  if check "${tmp}/bad.json" >"${tmp}/error.log" 2>&1; then
    echo "required Go test guard accepted: ${mutation}" >&2
    exit 1
  fi
done
for scope in regression full; do
  group="backend-mysql-${scope}"
  jq -n --arg scope "${scope}" --rawfile manifest "${ROOT}/scripts/required-go-tests.txt" '
    [$manifest | split("\n")[] | split(" ")
     | select(.[0] == "backend-mysql-regression" or .[0] == "backend-audit-mysql" or
         ($scope == "full" and .[0] == "backend-mysql-history"))
     | {Package: ("github.com/GentleKingson/ocservia/control-plane/" + .[1]), Test: .[2]}
     | . + {Action: "run"}, . + {Action: "pass"}][]
  ' >"${tmp}/mysql.json"
  check "${tmp}/mysql.json" "${group}"
  jq -c 'if .Test == "TestRealQueryRowPoisonsBeforeScan" and .Action == "pass" then .Action = "skip" else . end' "${tmp}/mysql.json" >"${tmp}/engine.json"
  check "${tmp}/engine.json" "${group}" mysql
  if check "${tmp}/engine.json" "${group}" mariadb >/dev/null 2>&1; then
    echo "${group} accepted skipped MariaDB-specific regression" >&2; exit 1
  fi
  for name in TestRealTLS TestRealPrivileges TestRealInitializationAndHistory TestRealUsageTransactions TestRealOutboxCommitDisconnect/claim-request-lost; do
    jq -c --arg name "${name}" 'select(.Test != $name)' "${tmp}/mysql.json" >"${tmp}/bad.json"
    if check "${tmp}/bad.json" "${group}" >/dev/null 2>&1; then
      echo "${group} accepted missing ${name}" >&2; exit 1
    fi
  done
  jq -c 'if .Test == "TestRealUsageTransactions" and .Action == "pass" then .Action = "skip" else . end' "${tmp}/mysql.json" >"${tmp}/bad.json"
  if check "${tmp}/bad.json" "${group}" >/dev/null 2>&1; then
    echo "${group} accepted skipped regression" >&2; exit 1
  fi
done
# A full invocation must not accidentally validate just the daily selection.
jq -c 'select(.Test != "TestRealVersionFiveDataUpgrade")' "${tmp}/mysql.json" >"${tmp}/bad.json"
if check "${tmp}/bad.json" backend-mysql-full >/dev/null 2>&1; then
  echo 'full database guard accepted missing history' >&2; exit 1
fi
echo 'MySQL/MariaDB regression and history guards passed'
for missing in OCSERV_TEST_DATABASE_URL OCSERV_TEST_OWNER_DATABASE_URL; do
  if (export OCSERV_TEST_DATABASE_URL=test OCSERV_TEST_OWNER_DATABASE_URL=test
      unset "${missing}"
      bash "${ROOT}/scripts/required-go-tests.sh" database-api ./internal/api) >"${tmp}/error.log" 2>&1; then
    echo "database acceptance accepted missing ${missing}" >&2
    exit 1
  fi
  grep -Fq "${missing}" "${tmp}/error.log"
done
echo 'Required Go test guard: positive fixture and 10 negative cases passed'
