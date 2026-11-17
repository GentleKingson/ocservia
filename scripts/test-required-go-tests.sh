#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/go-test-environment.sh
source "${ROOT}/scripts/go-test-environment.sh"
require_test_commands go jq setsid python3
tmp="$(mktemp -d "${TMPDIR:-/tmp}/required-go-tests-selftest-XXXXXX")"
trap 'rm -rf "${tmp}"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
echo 'Required-test guard self-test: synthetic events below are NOT database acceptance'
check() {
  jq -se --arg group "${2:-database-api}" --rawfile manifest "${ROOT}/scripts/required-go-tests.txt" \
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
  'select(.Test != "TestAuthLogSessionFailureIntegration")' \
  'if .Test == "TestAuthLogSessionFailureIntegration" and .Action == "pass" then .Action = "skip" else . end' \
  'empty'; do
  jq -c "${mutation}" "${tmp}/pass.json" >"${tmp}/bad.json"
  if check "${tmp}/bad.json" >"${tmp}/error.log" 2>&1; then
    echo "required Go test guard accepted: ${mutation}" >&2
    exit 1
  fi
done
jq -n --rawfile manifest "${ROOT}/scripts/required-go-tests.txt" '
  [$manifest | split("\n")[] | split(" ")
   | select(.[0] == "postgres-snapshot")
   | {Package: ("github.com/GentleKingson/ocservia/control-plane/" + .[1]), Test: .[2]}
   | . + {Action: "run"}, . + {Action: "pass"}][]
' >"${tmp}/pg_snapshot.json"
check "${tmp}/pg_snapshot.json" postgres-snapshot
for name in TestPostgreSQLCutoverCleanupRollback TestPostgreSQLArtifactFreshAndUpgrade TestPostgreSQLSnapshotCatalogReadPermissions; do
  jq -c --arg name "${name}" 'select(.Test != $name)' "${tmp}/pg_snapshot.json" >"${tmp}/bad.json"
  if check "${tmp}/bad.json" postgres-snapshot >/dev/null 2>&1; then
    echo "postgres-snapshot accepted missing ${name}" >&2; exit 1
  fi
done
# Shard pattern partitions a guard cleanly; each half still fails closed.
shard_check() {
  jq -se --arg group postgres-snapshot --arg shard_mode "$2" --arg shard_pattern '^TestPostgreSQLCutover' \
    --rawfile manifest "${ROOT}/scripts/required-go-tests.txt" -f "${ROOT}/scripts/check-required-go-tests.jq" "$1"
}
full_required="$(check "${tmp}/pg_snapshot.json" postgres-snapshot | jq .required)"
run_required="$(shard_check "${tmp}/pg_snapshot.json" run | jq .required)"
skip_required="$(shard_check "${tmp}/pg_snapshot.json" skip | jq .required)"
((run_required > 0 && skip_required > 0 && run_required + skip_required == full_required)) || {
  echo "shard pattern does not partition postgres-snapshot: ${run_required}+${skip_required}!=${full_required}" >&2; exit 1; }
jq -c 'select(.Test | startswith("TestPostgreSQLCutover") | not)' "${tmp}/pg_snapshot.json" >"${tmp}/non_cutover.json"
shard_check "${tmp}/non_cutover.json" skip >/dev/null
if shard_check "${tmp}/non_cutover.json" run >/dev/null 2>&1; then echo 'run shard accepted missing crash recovery' >&2; exit 1; fi
jq -c 'select(.Test != "TestPostgreSQLArtifactFreshAndUpgrade")' "${tmp}/non_cutover.json" >"${tmp}/bad.json"
if shard_check "${tmp}/bad.json" skip >/dev/null 2>&1; then echo 'skip shard accepted missing TestPostgreSQLArtifactFreshAndUpgrade' >&2; exit 1; fi
echo 'PostgreSQL snapshot and recovery guards passed'
# Exercise every explicit critical inventory, not just a successful go exit.
while read -r group; do
  jq -n --arg group "${group}" --rawfile manifest "${ROOT}/scripts/required-go-tests.txt" '
    $manifest | split("\n")[] | split(" ")
    | select(.[0] == $group)
    | {Package: ("github.com/GentleKingson/ocservia/control-plane/" + .[1]), Test: .[2]}
    | . + {Action: "run"}, . + {Action: "pass"}
  ' >"${tmp}/critical.json"
  check "${tmp}/critical.json" "${group}"
  jq -ner --arg group "${group}" --arg mode select \
    --rawfile manifest "${ROOT}/scripts/required-go-tests.txt" \
    -f "${ROOT}/scripts/check-required-go-tests.jq" >/dev/null
  for mutation in 'select(.Action != "run")' 'select(.Action != "pass")' \
    'if .Action == "pass" then .Action = "skip" else . end' \
    'if .Action == "pass" then .Action = "fail" else . end' '.Test += "Renamed"' 'empty'; do
    jq -c "${mutation}" "${tmp}/critical.json" >"${tmp}/bad.json"
    if check "${tmp}/bad.json" "${group}" >/dev/null 2>&1; then
      echo "${group} accepted ${mutation}" >&2; exit 1
    fi
  done
  # Removing any single required child must fail even with its parent pass.
  while read -r name; do
    jq -c --arg name "${name}" 'select(.Test != $name)' "${tmp}/critical.json" >"${tmp}/bad.json"
    if check "${tmp}/bad.json" "${group}" >/dev/null 2>&1; then
      echo "${group} accepted missing ${name}" >&2; exit 1
    fi
  done < <(jq -r '.Test' "${tmp}/critical.json" | sort -u)
done < <(awk '$1 ~ /^regression-/ {print $1}' "${ROOT}/scripts/required-go-tests.txt" | sort -u)
for mode in check select; do
  if jq -ne --arg group nonexistent --arg mode "${mode}" --rawfile manifest "${ROOT}/scripts/required-go-tests.txt" \
    -f "${ROOT}/scripts/check-required-go-tests.jq" >/dev/null 2>&1; then
    echo "empty group accepted in ${mode}" >&2; exit 1
  fi
done
for scope in invalid '' compatibility; do
  if DATABASE_TEST_SCOPE="${scope}" bash "${ROOT}/scripts/database-integration.sh" >"${tmp}/scope.log" 2>&1; then
    echo 'invalid scope accepted' >&2; exit 1
  fi
  grep -Fq 'DATABASE_TEST_SCOPE must be smoke, full or regression' "${tmp}/scope.log"
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
func TestShared(t *testing.T) {
  t.Run("original", func(t *testing.T) { t.Run("nested", func(t *testing.T) {}) })
}
func TestOther(t *testing.T) {}
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
# Shared preparation keeps the original cases below a selected parent, alongside
# unrelated top-level tests. Missing descendants must still fail the real guard.
printf 'fixture internal/selectionfixture TestShared\nfixture internal/selectionfixture TestShared/original\nfixture internal/selectionfixture TestShared/original/nested\nfixture internal/selectionfixture TestOther\n' >"${tmp}/shared.txt"
selection="$(jq -nr --arg group fixture --arg mode select --rawfile manifest "${tmp}/shared.txt" -f "${ROOT}/scripts/check-required-go-tests.jq")"
IFS=$'\t' read -r package pattern <<<"${selection}"
[[ "${package}" == github.com/GentleKingson/ocservia/control-plane/internal/selectionfixture ]]
(cd "${tmp}/selection" && GOWORK=off go test -json -count=1 -run "${pattern}") >"${tmp}/shared.json"
jq -se --arg group fixture --rawfile manifest "${tmp}/shared.txt" -f "${ROOT}/scripts/check-required-go-tests.jq" "${tmp}/shared.json"
jq -c 'select(.Test != "TestShared/original/nested")' "${tmp}/shared.json" >"${tmp}/bad.json"
if jq -se --arg group fixture --rawfile manifest "${tmp}/shared.txt" \
  -f "${ROOT}/scripts/check-required-go-tests.jq" "${tmp}/bad.json" >/dev/null 2>&1; then
  echo 'shared fixture accepted missing original descendant' >&2; exit 1
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

# Exercise the real wrapper, without a database or a second test inventory.
mkdir -p "${tmp}/wrapper/scripts" "${tmp}/wrapper/bin"
cp "${ROOT}/scripts/required-go-tests.sh" "${ROOT}/scripts/check-required-go-tests.jq" \
  "${ROOT}/scripts/go-test-environment.sh" "${tmp}/wrapper/scripts/"
printf 'fixture internal/fixture TestRequired\n' >"${tmp}/wrapper/scripts/required-go-tests.txt"
cat >"${tmp}/wrapper/bin/go" <<'SH'
#!/usr/bin/env bash
set -eu
echo '{"Package":"github.com/GentleKingson/ocservia/control-plane/internal/fixture","Test":"TestRequired","Action":"run"}'
case "${FIXTURE_MODE}" in
  missing) exit 0 ;;
  fail) exit 7 ;;
  hang)
    trap 'echo stopped >"${FIXTURE_STOPPED}"; exit 143' TERM
    while :; do sleep 0.1; done ;;
esac
printf '{"Package":"github.com/GentleKingson/ocservia/control-plane/internal/fixture","Test":"TestRequired","Action":"%s"}\n' "${FIXTURE_MODE}"
SH
chmod +x "${tmp}/wrapper/bin/go"
python3 - "${tmp}/wrapper" <<'PY'
import os
import signal
import subprocess
import sys
import time
from pathlib import Path

root = Path(sys.argv[1])
base_env = dict(os.environ, PATH=f"{root / 'bin'}:{os.environ['PATH']}", FIXTURE_STOPPED=str(root / "stopped"))
signals = {"INT": signal.SIGINT, "TERM": signal.SIGTERM}
expected_codes = {"pass": 0, "fail": 7, "INT": 130, "TERM": 143}

for mode in ["pass", "fail", "missing", "skip", "INT", "TERM"]:
    log = root / f"{mode}.log"
    sig = signals.get(mode)
    env = dict(base_env, FIXTURE_MODE="hang" if sig is not None else mode)
    with log.open("wb") as stream:
        proc = subprocess.Popen(
            ["bash", str(root / "scripts/required-go-tests.sh"), "fixture"],
            env=env,
            stdout=stream,
            stderr=subprocess.STDOUT,
        )
    if sig is not None:
        deadline = time.monotonic() + 10
        while True:
            if log.exists() and '"Action":"run"' in log.read_text(encoding="utf-8", errors="replace"):
                break
            if time.monotonic() > deadline:
                raise RuntimeError("JSON was not streamed")
            time.sleep(0.05)
        proc.send_signal(sig)
    returncode = proc.wait()
    expected = expected_codes.get(mode)
    if expected is not None:
        if returncode != expected:
            raise RuntimeError(f"incorrect wrapper status {mode}: {returncode}")
    elif returncode == 0:
        raise RuntimeError(f"incorrect wrapper status {mode}: {returncode}")
    stopped = root / "stopped"
    if sig is not None:
        if not stopped.exists():
            raise RuntimeError("child process was not terminated")
        stopped.unlink()
PY
echo 'Streaming, exit status and process-group signal cleanup passed'
mkdir "${tmp}/timeout"
printf 'module github.com/GentleKingson/ocservia/control-plane/internal/fixture\n\ngo 1.26.6\n' >"${tmp}/timeout/go.mod"
cat >"${tmp}/timeout/timeout_test.go" <<'GO'
package fixture
import (
  "os/exec"
  "testing"
  "time"
)
func TestRequired(t *testing.T) {
  child := exec.Command("bash", "-c", `trap 'touch "$FIXTURE_STOPPED"; exit 0' TERM; while :; do sleep 0.1; done`)
  if err := child.Start(); err != nil { t.Fatal(err) }
  time.Sleep(time.Minute)
}
GO
if (cd "${tmp}/timeout" && GOWORK=off FIXTURE_STOPPED="${tmp}/timeout-stopped" \
  bash "${tmp}/wrapper/scripts/required-go-tests.sh" fixture -timeout=1s .) >"${tmp}/timeout.log" 2>&1; then
  echo 'Go timeout incorrectly passed' >&2; exit 1
fi
grep -q 'test timed out after 1s' "${tmp}/timeout.log"
test -f "${tmp}/timeout-stopped"

# Run the actual PostgreSQL smoke routing with disposable command stubs, not databases.
cp "${ROOT}/scripts/database-postgres-smoke.sh" "${ROOT}/scripts/env.sh" "${tmp}/wrapper/scripts/"
cp "${ROOT}/scripts/required-go-tests.txt" "${tmp}/wrapper/scripts/"
mkdir "${tmp}/wrapper/control-plane"
cat >"${tmp}/wrapper/scripts/required-go-tests.sh" <<'SH'
#!/usr/bin/env bash
printf '%s%s\n' "${REQUIRED_SHARD_RUN:+run=${REQUIRED_SHARD_RUN} }${REQUIRED_SHARD_SKIP:+skip=${REQUIRED_SHARD_SKIP} }" "$*" >>"${ROUTE_LOG}"
SH
cat >"${tmp}/wrapper/scripts/test-enrollment-restart.sh" <<'SH'
#!/usr/bin/env bash
[[ "$1" == ocservia-pg-smoke-* ]] || exit 1
printf 'backend-enrollment-restart\n' >>"${ROUTE_LOG}"
SH
cat >"${tmp}/wrapper/bin/docker" <<'SH'
#!/usr/bin/env bash
if [[ "$1" == port ]]; then echo '127.0.0.1:12345'; fi
if [[ "$1" == exec && "$2" == -i ]]; then cat >/dev/null; fi
exit 0
SH
cat >"${tmp}/wrapper/bin/openssl" <<'SH'
#!/usr/bin/env bash
while (($#)); do
  case "$1" in -keyout|-out) shift; touch "$1" ;; esac
  shift
done
SH
cat >"${tmp}/wrapper/bin/go" <<'SH'
#!/usr/bin/env bash
if [[ "$*" == 'env CGO_ENABLED' ]]; then echo 1; exit 0; fi
if [[ "$*" == 'env CC' ]]; then echo fixture-cc; exit 0; fi
printf '%s\n' "$*" >>"${ROUTE_LOG}"
SH
cat >"${tmp}/wrapper/bin/fixture-cc" <<'SH'
#!/usr/bin/env bash
cat >/dev/null
SH
chmod +x "${tmp}/wrapper/bin/"*
# Run the production PostgreSQL business-suite function with recording command
# boundaries. This proves complete Integration selectors and isolated API state
# without pretending that the fixture executes SQL or business acceptance.
export ROUTE_LOG="${tmp}/postgres-full.route"
run_postgres_fixture() {
  bash -euo pipefail -s -- "${tmp}/wrapper" "${ROOT}/scripts/database-postgres-tests.sh" <<'SH'
  ROOT="$1"
  TEST_CONTROL_PLANE="${ROOT}/control-plane"
  container=ocservia-pg-smoke-fixture major=18 port=12345
  runtime_url=postgres://app/ocservia owner_url=postgres://owner/ocservia
  latest_runtime_url=postgres://app/ocservia_latest latest_owner_url=postgres://owner/ocservia_latest
  clone_database() { printf 'clone %s\n' "$*" >>"${ROUTE_LOG}"; }
  checked_go_tests() {
    printf 'checked %s|%s|%s\n' "$*" "${OCSERV_TEST_DATABASE_URL}" "${OCSERV_TEST_OWNER_DATABASE_URL}" >>"${ROUTE_LOG}"
    [[ "${POSTGRES_FAIL_GROUP:-}" != "$1" ]] || return 19
  }
  assert_auth_fixture_cleanup() { printf 'cleanup %s\n' "$*" >>"${ROUTE_LOG}"; }
  docker() { printf 'docker %s\n' "$*" >>"${ROUTE_LOG}"; }
  source "$2"
  PATH="${ROOT}/bin:${PATH}"
  run_postgres_business_tests "$container" "$major" "$port" "$runtime_url" "$owner_url" "$latest_runtime_url" "$latest_owner_url"
SH
}
run_postgres_fixture
for package in userstate useroperations configplan certificates; do
  case "${package}" in
    configplan) group=config ;;
    *) group="${package}" ;;
  esac
  grep -Fxq "backend-policy-${group} -p 1 ./internal/${package} -run Integration" "${ROUTE_LOG}"
done
grep -Fxq 'test -p 1 ./internal/telemetry -run Integration -count=1' "${ROUTE_LOG}"
grep -Fxq 'clone ocservia-pg-smoke-fixture ocservia_latest ocservia_policy_api' "${ROUTE_LOG}"
grep -Fxq 'checked backend-policy-api --select -race -p 1 -parallel 1|postgres://app/ocservia_policy_api|postgres://owner/ocservia_policy_api' "${ROUTE_LOG}"
grep -Fxq 'docker exec ocservia-pg-smoke-fixture dropdb -U ocservia_owner ocservia_policy_api' "${ROUTE_LOG}"
for failed_group in backend-auth backend-policy-api database-auth database-api; do
  export ROUTE_LOG="${tmp}/postgres-failed.route"
  : >"${ROUTE_LOG}"
  status=0
  POSTGRES_FAIL_GROUP="${failed_group}" run_postgres_fixture >"${tmp}/postgres-failed.log" 2>&1 || status=$?
  [[ "${status}" == 19 ]] || { echo "PostgreSQL routing swallowed ${failed_group}: ${status}" >&2; exit 1; }
  [[ "$(tail -n 1 "${ROUTE_LOG}")" == "checked ${failed_group} "* ]] || {
    echo "PostgreSQL routing continued after ${failed_group}" >&2; exit 1; }
done
# Exact, ordered PostgreSQL smoke route: a missing, duplicated, replaced or extra entry fails.
printf '%s\n' '--smoke ./internal/platform/app TestDatabaseCoreSmoke' \
  '--smoke ./migrations TestDatabaseInitializationSmoke' \
  'backend-enrollment-restart' \
  'postgres-snapshot --select -race -timeout=10m' >"${tmp}/expected-postgres-smoke.route"
export ROUTE_LOG="${tmp}/database-postgres-smoke.sh.route"
PATH="${tmp}/wrapper/bin:${PATH}" DATABASE_TEST_SCOPE=smoke PG_MAJOR=18 \
  bash "${tmp}/wrapper/scripts/database-postgres-smoke.sh" >/dev/null
cmp -s "${tmp}/expected-postgres-smoke.route" "${ROUTE_LOG}" || {
  diff "${tmp}/expected-postgres-smoke.route" "${ROUTE_LOG}" >&2
  echo 'postgres smoke route changed' >&2; exit 1; }
echo 'Current smoke includes core, initialization, restart and snapshot checks'

# Basic CI has one explicit entry, not the deep acceptance manifest above.
mkdir "${tmp}/smoke"
printf 'module smoke\n\ngo 1.26.6\n' >"${tmp}/smoke/go.mod"
cat >"${tmp}/smoke/smoke_test.go" <<'GO'
package smoke
import ("os"; "testing")
func TestPass(t *testing.T) {
  f, err := os.OpenFile(os.Getenv("SMOKE_COUNT"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
  if err != nil { t.Fatal(err) }
  defer f.Close()
  if _, err := f.WriteString("ran\n"); err != nil { t.Fatal(err) }
}
func TestSkipped(t *testing.T) { t.Skip("missing database fixture") }
func TestFailure(t *testing.T) { t.Fatal("injected core failure") }
GO
for _attempt in 1 2; do
  (cd "${tmp}/smoke" && GOWORK=off SMOKE_COUNT="${tmp}/count" \
    bash "${ROOT}/scripts/required-go-tests.sh" --smoke . TestPass) >"${tmp}/smoke.log" 2>&1
done
test "$(wc -l <"${tmp}/count")" -eq 2
for entry in TestSkipped TestFailure TestMissing; do
  if (cd "${tmp}/smoke" && GOWORK=off bash "${ROOT}/scripts/required-go-tests.sh" --smoke . "${entry}") >"${tmp}/smoke.log" 2>&1; then
    echo "core guard accepted ${entry}" >&2; exit 1
  fi
done
echo 'Core guard: fresh execution, failure, skipped and absent entry checks passed'
