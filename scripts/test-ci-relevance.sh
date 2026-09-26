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
    if [[ " ${selected} " == *' run_ci_tools '* ]]; then
      grep -Eq '^ci_suites=(guards|release|g6)( (guards|release|g6))*$' "${out}"
      case "${path}" in
        scripts/ci-tools-check.sh) expect "${out}" ci_suites 'guards release g6' ;;
        scripts/g6-buildx-cache.sh|.github/actions/g6-cache-credentials/index.js)
          expect "${out}" ci_suites 'release g6' ;;
        .github/workflows/ci.yml|.github/workflows/security.yml|scripts/ci-relevance.sh)
          expect "${out}" ci_suites guards ;;
        .github/workflows/release*.yml)
          expect "${out}" ci_suites release ;;
        .github/workflows/g6-harness-core.yml|scripts/g6-pipeline.mjs|scripts/test-g6-resource-sampler.sh|docs/acceptance/g6-*)
          expect "${out}" ci_suites g6 ;;
      esac
    else
      expect "${out}" ci_suites ''
    fi
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
deploy/managed-node/install.sh run_installers
deploy/production/install.sh run_installers
deploy/production/controller-bootstrap.sh run_installers
deploy/lib/install-env.sh run_installers
deploy/production/transportd-relays.sh run_installers
deploy/production/systemd/agent-relays.sh run_installers
deploy/production/systemd/ocservia-agent-relays.conf run_installers
scripts/prepare-bootstrap-release-assets.sh run_installers
scripts/release-agent-state-check.sh run_installers
scripts/package-agent.sh run_installers
scripts/verify-agent-package.sh run_installers
scripts/test-managed-node-install.sh run_installers
scripts/test-controller-install.sh run_installers
scripts/test-controller-bootstrap.sh run_installers
scripts/test-relay-launchers.py run_installers
scripts/test-release-agent-state-check.sh run_installers
.gitignore run_installers
deploy/managed-node/install.env.example run_ci_tools run_installers
deploy/production/Caddyfile run_ci_tools run_installers
deploy/production/compose.yaml run_ci_tools run_installers
deploy/production/backup-entrypoint.sh run_ci_tools run_installers
deploy/real-e2e/compose.yaml run_ci_tools
scripts/upgrade-agent.sh run_ci_tools run_installers
scripts/rollback-agent.sh run_ci_tools run_installers
scripts/uninstall-agent.sh run_ci_tools run_installers
.github/workflows/ci.yml run_ci_tools
.github/workflows/release.yml run_ci_tools run_installers
.github/workflows/release-upgrade.yml run_ci_tools run_installers
.github/workflows/security.yml run_ci_tools
.github/workflows/g6-harness-core.yml run_ci_tools
scripts/ci-relevance.sh run_ci_tools
scripts/ci-tools-check.sh run_ci_tools
scripts/bootstrap.sh run_docs run_go run_rust run_web run_database run_ci_tools run_installers
docs/acceptance/g6-slo.yaml run_ci_tools
docs/acceptance/g6-runtime-result-schema.json run_ci_tools
scripts/g6-runtime/package-lock.json run_ci_tools
scripts/g6-pipeline.mjs run_ci_tools
scripts/test-g6-resource-sampler.sh run_ci_tools
scripts/g6-buildx-cache.sh run_ci_tools
.github/actions/g6-cache-credentials/index.js run_ci_tools
tools/g6-harness/internal/runtime/runtime.go run_ci_tools
deploy/g6-readiness/relay.toml run_ci_tools
deploy/package/nfpm.yaml run_ci_tools run_installers
unclassified.conf run_docs run_go run_rust run_web run_database run_ci_tools run_installers
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
for flag in "${flags[@]}"; do expect "${out}" "${flag}" true; done
# A PR uses the merge base, whereas a push includes both endpoints. Renames
# are deliberately expanded into deletion + addition, including old domains.
git -C "${fixture}" checkout -q --detach "${base}"
mkdir -p "${fixture}/web"
printf 'original\n' >"${fixture}/web/source.ts"
git -C "${fixture}" add web/source.ts
git -C "${fixture}" commit -qm original
ancestor="$(git -C "${fixture}" rev-parse HEAD)"
git -C "${fixture}" mv web/source.ts README.md
git -C "${fixture}" commit -qm rename
renamed="$(git -C "${fixture}" rev-parse HEAD)"
out="${fixture}/rename.output"
(cd "${fixture}" && bash "${SCRIPT}" pull_request "${ancestor}" "${renamed}" "${out}")
expect "${out}" run_web true; expect "${out}" run_docs true
git -C "${fixture}" checkout -q --detach "${ancestor}"
mkdir -p "${fixture}/rust"
printf 'base-only\n' >"${fixture}/rust/source.rs"
git -C "${fixture}" add rust/source.rs
git -C "${fixture}" commit -qm base-only
diverged="$(git -C "${fixture}" rev-parse HEAD)"
for event in pull_request push; do
  out="${fixture}/${event}-diverged.output"
  (cd "${fixture}" && bash "${SCRIPT}" "${event}" "${diverged}" "${renamed}" "${out}")
  expected=false; [[ "${event}" != push ]] || expected=true
  expect "${out}" run_rust "${expected}"
  expect "${out}" run_web true
done
for pair in "${base} ${base}" "0000000000000000000000000000000000000000 ${base}" "1111111111111111111111111111111111111111 ${base}"; do
  read -r before after <<<"${pair}"
  out="${fixture}/fallback.output"; rm -f "${out}"
  (cd "${fixture}" && bash "${SCRIPT}" push "${before}" "${after}" "${out}")
  for flag in "${flags[@]}"; do expect "${out}" "${flag}" true; done
done
git -C "${fixture}" checkout -q --orphan unrelated
git -C "${fixture}" rm -qrf .
git -C "${fixture}" commit --allow-empty -qm unrelated
out="${fixture}/unrelated.output"
(cd "${fixture}" && bash "${SCRIPT}" pull_request "${base}" "$(git rev-parse HEAD)" "${out}")
expect "${out}" reason merge_base_unresolvable
for flag in "${flags[@]}"; do expect "${out}" "${flag}" true; done
echo 'CI routing: domain isolation, exact installer/workflow contracts, Controller Quick, Full matrix and fallback passed'
