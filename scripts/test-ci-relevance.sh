#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="${ROOT}/scripts/ci-relevance.sh"
fixture="$(mktemp -d)"
trap 'rm -rf -- "${fixture}"' EXIT
git -C "${fixture}" init -q
git -C "${fixture}" config user.name test
git -C "${fixture}" config user.email test@example.invalid
git -C "${fixture}" commit --allow-empty -qm base
base="$(git -C "${fixture}" rev-parse HEAD)"
flags=(run_docs run_go run_rust run_web run_database run_ci_tools run_installers)
expect() {
  local out=$1 key=$2 value=$3
  grep -Fxq "${key}=${value}" "${out}" || { cat "${out}" >&2; echo "expected ${key}=${value}" >&2; exit 1; }
}
check_matrix() {
  local out=$1 profile=$2
  sed -n 's/^database_matrix=//p' "${out}" | jq -e --arg profile "${profile}" '
    .include == (if $profile == "quick" then
      [{"engine":"postgres","postgres":"17","version":"17.10"},{"engine":"mysql","version":"8.4.10"}]
    else
      [{"engine":"postgres","postgres":"17","version":"17.10"},{"engine":"postgres","postgres":"18","version":"18.6"},{"engine":"mysql","version":"8.4.10"},{"engine":"mariadb","version":"12.3.2"}]
    end)' >/dev/null
}
while read -r path selected; do
  git -C "${fixture}" checkout -q --detach "${base}"
  mkdir -p "${fixture}/$(dirname "${path}")"
  printf 'change\n' >"${fixture}/${path}"
  git -C "${fixture}" add -- "${path}"
  git -C "${fixture}" commit -qm change
  head="$(git -C "${fixture}" rev-parse HEAD)"
  for event in pull_request push; do
    out="${fixture}/result.output"
    rm -f "${out}"
    (cd "${fixture}" && bash "${SCRIPT}" "${event}" "${base}" "${head}" "${out}" full)
    for flag in "${flags[@]}"; do
      expected=false
      [[ " ${selected} " != *" ${flag} "* ]] || expected=true
      expect "${out}" "${flag}" "${expected}"
    done
    expect "${out}" profile quick
    expect "${out}" database_scope smoke
    check_matrix "${out}" quick
    rm -f "${out}"
  done
done <<'CASES'
README.md run_docs
docs/development/testing.md run_docs
web/src/App.vue run_web
web/src/api/generated/index.ts run_web
rust/crates/agent/src/lib.rs run_rust
control-plane/internal/platform/app/run.go run_go run_database
control-plane/internal/database/mysql/backend.go run_go run_database
control-plane/internal/api/routes.go run_go run_database
control-plane/migrations/000036.up.sql run_go run_database
scripts/database-foundation-integration.sh run_go run_database
scripts/go-check.sh run_go
scripts/web-check.sh run_web
scripts/test-controller-install.sh run_rust run_installers
scripts/ci-relevance.sh run_docs run_go run_rust run_web run_database run_ci_tools
unclassified.conf run_docs run_go run_rust run_web run_database
CASES
for profile in quick full; do
  out="${fixture}/dispatch-${profile}.output"
  (cd "${fixture}" && bash "${SCRIPT}" workflow_dispatch invalid invalid "${out}" "${profile}")
  for flag in run_docs run_go run_rust run_web run_database; do expect "${out}" "${flag}" true; done
  expect "${out}" run_ci_tools false
  expect "${out}" run_installers false
  expect "${out}" profile "${profile}"
  scope=smoke; [[ "${profile}" != full ]] || scope=compatibility
  expect "${out}" database_scope "${scope}"
  check_matrix "${out}" "${profile}"
done
out="${fixture}/invalid.output"
(cd "${fixture}" && bash "${SCRIPT}" push invalid invalid "${out}")
expect "${out}" profile quick
check_matrix "${out}" quick
for flag in run_docs run_go run_rust run_web run_database; do expect "${out}" "${flag}" true; done
echo 'CI routing: docs/Web/Rust isolation, Controller Quick, Full matrix and fallback passed'
