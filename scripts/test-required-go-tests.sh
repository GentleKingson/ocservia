#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmp="$(mktemp -d "${TMPDIR:-/tmp}/required-go-tests-selftest-XXXXXX")"
trap 'rm -rf "${tmp}"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
echo 'Required-test guard self-test: synthetic events below are NOT database acceptance'
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
for scope in full; do
  group="backend-mysql-${scope}"
  jq -n --arg scope "${scope}" --rawfile manifest "${ROOT}/scripts/required-go-tests.txt" '
    [$manifest | split("\n")[] | split(" ")
     | select(.[0] == "backend-mysql-current" or .[0] == "backend-audit-mysql" or
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
# Exercise every explicit critical inventory, not just a successful go exit.
for engine in mysql mariadb; do
  while read -r group; do
    PR02_ENGINE="${engine}" jq -n --arg group "${group}" --rawfile manifest "${ROOT}/scripts/required-go-tests.txt" '
      $manifest | split("\n")[] | split(" ")
      | select(.[0] == $group and (length < 4 or .[3] == env.PR02_ENGINE))
      | {Package: ("github.com/GentleKingson/ocservia/control-plane/" + .[1]), Test: .[2]}
      | . + {Action: "run"}, . + {Action: "pass"}
    ' >"${tmp}/critical.json"
    check "${tmp}/critical.json" "${group}" "${engine}"
    PR02_ENGINE="${engine}" jq -ner --arg group "${group}" --arg mode select \
      --rawfile manifest "${ROOT}/scripts/required-go-tests.txt" \
      -f "${ROOT}/scripts/check-required-go-tests.jq" >/dev/null
    for mutation in 'select(.Action != "run")' 'select(.Action != "pass")' \
      'if .Action == "pass" then .Action = "skip" else . end' \
      'if .Action == "pass" then .Action = "fail" else . end' '.Test += "Renamed"' 'empty'; do
      jq -c "${mutation}" "${tmp}/critical.json" >"${tmp}/bad.json"
      if check "${tmp}/bad.json" "${group}" "${engine}" >/dev/null 2>&1; then
        echo "${group} accepted ${mutation}" >&2; exit 1
      fi
    done
    # Removing any single required child must fail even with its parent pass.
    while read -r name; do
      jq -c --arg name "${name}" 'select(.Test != $name)' "${tmp}/critical.json" >"${tmp}/bad.json"
      if check "${tmp}/bad.json" "${group}" "${engine}" >/dev/null 2>&1; then
        echo "${group} accepted missing ${name}" >&2; exit 1
      fi
    done < <(jq -r '.Test' "${tmp}/critical.json" | sort -u)
  done < <(awk '$1 ~ /^regression-/ {print $1}' "${ROOT}/scripts/required-go-tests.txt" | sort -u)
done
for mode in check select; do
  if jq -ne --arg group nonexistent --arg mode "${mode}" --rawfile manifest "${ROOT}/scripts/required-go-tests.txt" \
    -f "${ROOT}/scripts/check-required-go-tests.jq" >/dev/null 2>&1; then
    echo "empty group accepted in ${mode}" >&2; exit 1
  fi
done
for script in database-integration.sh database-foundation-integration.sh; do
  if DATABASE_TEST_SCOPE=invalid bash "${ROOT}/scripts/${script}" >"${tmp}/scope.log" 2>&1; then
    echo 'invalid scope accepted' >&2; exit 1
  fi
  grep -Fq 'DATABASE_TEST_SCOPE must be full or regression' "${tmp}/scope.log"
done
# This small fixture RUNS the slash-separated -run expression, including
# literal regex metacharacters and a sibling that must never execute.
mkdir "${tmp}/selection"
printf 'module github.com/GentleKingson/ocservia/control-plane/internal/selectionfixture\n\ngo 1.26.6\n' >"${tmp}/selection/go.mod"
cat >"${tmp}/selection/selection_test.go" <<'GO'
package selectionfixture
import "testing"
func TestSelected(t *testing.T) {
  t.Run("literal.+(x)[y]$", func(t *testing.T) {})
  t.Run("other", func(t *testing.T) { t.Fatal("unselected sibling ran") })
}
func TestSelectedExtra(t *testing.T) { t.Fatal("unselected parent ran") }
GO
printf 'fixture internal/selectionfixture TestSelected/literal.+(x)[y]$\n' >"${tmp}/selection.txt"
selection="$(jq -nr --arg group fixture --arg mode select --rawfile manifest "${tmp}/selection.txt" -f "${ROOT}/scripts/check-required-go-tests.jq")"
IFS=$'\t' read -r package pattern <<<"${selection}"
(cd "${tmp}/selection" && GOWORK=off go test -json -count=1 -run "${pattern}") >"${tmp}/selected.json"
jq -se --arg group fixture --rawfile manifest "${tmp}/selection.txt" -f "${ROOT}/scripts/check-required-go-tests.jq" "${tmp}/selected.json"
printf 'fixture internal/selectionfixture TestSelectedExtra/other\n' >>"${tmp}/selection.txt"
if jq -ne --arg group fixture --arg mode select --rawfile manifest "${tmp}/selection.txt" \
  -f "${ROOT}/scripts/check-required-go-tests.jq" >/dev/null 2>&1; then
  echo 'unrelated subtest parents accepted a cross-product selection' >&2; exit 1
fi
echo 'Explicit critical inventories, scope rejection and real subtest selection passed'
for missing in OCSERV_TEST_DATABASE_URL OCSERV_TEST_OWNER_DATABASE_URL; do
  if (export OCSERV_TEST_DATABASE_URL=test OCSERV_TEST_OWNER_DATABASE_URL=test
      unset "${missing}"
      bash "${ROOT}/scripts/required-go-tests.sh" database-api ./internal/api) >"${tmp}/error.log" 2>&1; then
    echo "database acceptance accepted missing ${missing}" >&2
    exit 1
  fi
  grep -Fq "${missing}" "${tmp}/error.log"
done
echo 'Required Go test guard checks passed'
