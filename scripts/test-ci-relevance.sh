#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="${ROOT}/scripts/ci-relevance.sh"
fixture="$(mktemp -d)"
trap 'rm -rf -- "${fixture}"' EXIT

flags=(run_go_standard run_go_race run_runtime_artifacts run_database run_local_slice run_stage_contracts run_production_relays run_credential_rotation run_web run_browser run_p1_smoke run_contracts_policy run_rust run_native run_license run_g6_smoke full)

git -C "${fixture}" init -q
git -C "${fixture}" config user.name test
git -C "${fixture}" config user.email test@example.invalid
printf '*.output\n' >>"${fixture}/.git/info/exclude"
paths=(
  README.md docs/development/guide.md docs/acceptance/g6-slo.yaml
  docs/acceptance/v0.2-release-readiness.md web/src/App.vue web/test/unit.test.ts
  web/e2e/app.spec.ts control-plane/internal/domain/helper.go
  control-plane/internal/auth/store.go control-plane/migrations/000001.up.sql
  rust/crates/observability/src/lib.rs rust/crates/agent/src/lib.rs
  rust/crates/transportd-stub/src/lib.rs deploy/production/compose.yaml
  deploy/production/rotate-postgres-credentials.sh openapi/openapi.yaml
  proto/ocserv/platform/agent/v1/agent.proto tools/g6-harness/internal/smoke/pipeline.go
  deploy/systemd/ocservia-agent.service .github/workflows/g6-harness-smoke.yml
  .github/workflows/release.yml scripts/ci-relevance.sh scripts/prepare-bootstrap-release-assets.sh scripts/test-controller-release-smoke.sh scripts/test-controller-release-bundle.sh scripts/verify-controller-release-bundle.sh scripts/test-controller-lifecycle.sh scripts/test-controller-compose-lifecycle.sh scripts/test-controller-host-bootstrap.sh web/package.json
  deploy/production/controller.sh deploy/production/controller-release-smoke.sh deploy/production/bootstrap-host.sh
  deploy/production/install.sh scripts/test-controller-install.sh
  deploy/production/controller-bootstrap.sh scripts/test-controller-bootstrap.sh
  deploy/managed-node/install.sh scripts/test-managed-node-install.sh
  deploy/lib/install-env.sh install.env.example
  scripts/real-e2e-node.sh deploy/real-e2e/controller.compose.yaml
  control-plane/Dockerfile
)
for path in "${paths[@]}"; do
  mkdir -p "${fixture}/$(dirname "${path}")"
  printf 'base\n' >"${fixture}/${path}"
done
git -C "${fixture}" add .
git -C "${fixture}" commit -qm base
base="$(git -C "${fixture}" rev-parse HEAD)"

expect_flag() {
  local output="$1" key="$2" expected="$3"
  grep -qx "${key}=${expected}" "${output}" || {
    echo "${key} expected ${expected} in ${output}" >&2
    cat "${output}" >&2
    exit 1
  }
}

expect_only() {
  local output="$1"; shift
  local flag expected
  for flag in "${flags[@]}"; do
    expected=false
    for enabled in "$@"; do [[ "${flag}" == "${enabled}" ]] && expected=true; done
    expect_flag "${output}" "${flag}" "${expected}"
  done
}

case_commit() {
  local label="$1"; shift
  git -C "${fixture}" checkout -q --detach "${base}"
  for path in "$@"; do printf '%s\n' "${label}" >>"${fixture}/${path}"; done
  git -C "${fixture}" add -A
  git -C "${fixture}" commit -qm "${label}"
  local head="$(git -C "${fixture}" rev-parse HEAD)"
  local output="${fixture}/${label}.output"
  (cd "${fixture}" && "${SCRIPT}" pull_request "${base}" "${head}" "${output}")
  printf '%s\n' "${output}"
}

# Ordinary Markdown is CI-neutral; machine-readable G6 contracts add the
# contracts/policy and G6 smoke domains.
out="$(case_commit readme README.md)"
expect_only "${out}"
out="$(case_commit docs docs/development/guide.md)"
expect_only "${out}"
out="$(case_commit acceptance_markdown docs/acceptance/v0.2-release-readiness.md)"
expect_only "${out}"
out="$(case_commit acceptance docs/acceptance/g6-slo.yaml)"
expect_only "${out}" run_contracts_policy run_g6_smoke

# Browser runners are reserved for runtime and E2E inputs, not unit-only
# Web changes.
out="$(case_commit web_source web/src/App.vue)"
expect_only "${out}" run_web run_browser
out="$(case_commit web_unit web/test/unit.test.ts)"
expect_only "${out}" run_web
out="$(case_commit web_e2e web/e2e/app.spec.ts)"
expect_only "${out}" run_web run_browser

# Language suites are independent; integration flags are added from the
# package boundary actually exercised by each harness.
out="$(case_commit go control-plane/internal/domain/helper.go)"
expect_only "${out}" run_go_standard run_go_race
out="$(case_commit db_go control-plane/internal/auth/store.go)"
expect_only "${out}" run_go_standard run_go_race run_runtime_artifacts run_database
out="$(case_commit migration control-plane/migrations/000001.up.sql)"
expect_only "${out}" run_runtime_artifacts run_database run_g6_smoke
out="$(case_commit rust rust/crates/observability/src/lib.rs)"
expect_only "${out}" run_rust
out="$(case_commit native_rust rust/crates/agent/src/lib.rs)"
expect_only "${out}" run_rust run_native run_g6_smoke
out="$(case_commit transport_stub rust/crates/transportd-stub/src/lib.rs)"
expect_only "${out}" run_runtime_artifacts run_local_slice run_p1_smoke run_rust

# Deployment, credentials, machine contracts, G6, packaging, and workflow
# callers each contribute only their known impact domains.
out="$(case_commit production deploy/production/compose.yaml)"
expect_only "${out}" run_production_relays
out="$(case_commit rotation deploy/production/rotate-postgres-credentials.sh)"
expect_only "${out}" run_production_relays run_credential_rotation
out="$(case_commit openapi openapi/openapi.yaml)"
expect_only "${out}" run_go_standard run_go_race run_stage_contracts run_web run_browser run_contracts_policy
out="$(case_commit proto proto/ocserv/platform/agent/v1/agent.proto)"
expect_only "${out}" run_go_standard run_go_race run_stage_contracts run_contracts_policy run_rust run_g6_smoke
out="$(case_commit g6_runtime tools/g6-harness/internal/smoke/pipeline.go)"
expect_only "${out}" run_go_standard run_go_race run_g6_smoke
out="$(case_commit g6_contract docs/acceptance/g6-slo.yaml)"
expect_only "${out}" run_contracts_policy run_g6_smoke
out="$(case_commit systemd deploy/systemd/ocservia-agent.service)"
expect_only "${out}" run_contracts_policy run_native
out="$(case_commit workflow .github/workflows/g6-harness-smoke.yml)"
expect_only "${out}" run_contracts_policy run_g6_smoke
out="$(case_commit release_workflow .github/workflows/release.yml)"
expect_only "${out}" run_contracts_policy
out="$(case_commit controller_lifecycle scripts/test-controller-lifecycle.sh)"
expect_only "${out}" run_contracts_policy
out="$(case_commit controller_compose_lifecycle scripts/test-controller-compose-lifecycle.sh)"
expect_only "${out}" run_production_relays
out="$(case_commit controller_bootstrap_test scripts/test-controller-host-bootstrap.sh)"
expect_only "${out}" run_production_relays
out="$(case_commit controller_bootstrap_host deploy/production/bootstrap-host.sh)"
expect_only "${out}" run_production_relays
out="$(case_commit controller_installer deploy/production/install.sh)"
expect_only "${out}" run_production_relays
out="$(case_commit controller_install_test scripts/test-controller-install.sh)"
expect_only "${out}" run_production_relays
out="$(case_commit controller_stage1_bootstrap deploy/production/controller-bootstrap.sh)"
expect_only "${out}" run_production_relays run_contracts_policy
out="$(case_commit controller_stage1_bootstrap_test scripts/test-controller-bootstrap.sh)"
expect_only "${out}" run_production_relays
out="$(case_commit managed_node_installer deploy/managed-node/install.sh)"
expect_only "${out}" run_native run_contracts_policy
out="$(case_commit managed_node_install_test scripts/test-managed-node-install.sh)"
expect_only "${out}" run_native run_contracts_policy
out="$(case_commit bootstrap_release_assets scripts/prepare-bootstrap-release-assets.sh)"
expect_only "${out}" run_contracts_policy
out="$(case_commit install_env_loader deploy/lib/install-env.sh)"
expect_only "${out}" run_native run_contracts_policy run_production_relays
out="$(case_commit install_env_example install.env.example)"
expect_only "${out}" run_native run_contracts_policy run_production_relays
out="$(case_commit controller_release_smoke scripts/test-controller-release-smoke.sh)"
expect_only "${out}" run_contracts_policy
out="$(case_commit controller_release_bundle scripts/test-controller-release-bundle.sh)"
expect_only "${out}" run_contracts_policy
out="$(case_commit controller_release_verifier scripts/verify-controller-release-bundle.sh)"
expect_only "${out}" run_contracts_policy
out="$(case_commit controller_entrypoint deploy/production/controller.sh)"
expect_only "${out}" run_contracts_policy run_production_relays
out="$(case_commit controller_smoke deploy/production/controller-release-smoke.sh)"
expect_only "${out}" run_contracts_policy run_production_relays
out="$(case_commit real_e2e_script scripts/real-e2e-node.sh)"
expect_only "${out}" run_contracts_policy
out="$(case_commit real_e2e_deploy deploy/real-e2e/controller.compose.yaml)"
expect_only "${out}" run_contracts_policy
out="$(case_commit control_dockerfile control-plane/Dockerfile)"
expect_only "${out}" run_contracts_policy run_production_relays run_g6_smoke
out="$(case_commit classifier_authority scripts/ci-relevance.sh)"
expect_only "${out}" "${flags[@]}"
expect_flag "${out}" reason ci_relevance_authority_changed

# Known mixed changes are the OR-union, never a category fallback.
out="$(case_commit docs_go docs/development/guide.md control-plane/internal/domain/helper.go)"
expect_only "${out}" run_go_standard run_go_race
out="$(case_commit web_rust web/src/App.vue rust/crates/observability/src/lib.rs)"
expect_only "${out}" run_web run_browser run_rust
out="$(case_commit migration_web control-plane/migrations/000001.up.sql web/src/App.vue)"
expect_only "${out}" run_runtime_artifacts run_database run_web run_browser run_g6_smoke

# Deletions retain the old path's impact. With rename detection off, both
# sides of a cross-domain rename contribute to the union.
git -C "${fixture}" checkout -q --detach "${base}"
git -C "${fixture}" rm -q web/test/unit.test.ts
git -C "${fixture}" commit -qm delete_known
head="$(git -C "${fixture}" rev-parse HEAD)"; out="${fixture}/delete.output"
(cd "${fixture}" && "${SCRIPT}" pull_request "${base}" "${head}" "${out}")
expect_only "${out}" run_web
git -C "${fixture}" checkout -q --detach "${base}"
mkdir -p "${fixture}/rust/crates/observability/src"
git -C "${fixture}" mv docs/development/guide.md rust/crates/observability/src/guide.rs
git -C "${fixture}" commit -qm rename_known
head="$(git -C "${fixture}" rev-parse HEAD)"; out="${fixture}/rename.output"
(cd "${fixture}" && "${SCRIPT}" pull_request "${base}" "${head}" "${out}")
expect_only "${out}" run_rust

# Unknown, empty, invalid, and dispatch classifications fail closed.
git -C "${fixture}" checkout -q --detach "${base}"
printf 'unknown\n' >"${fixture}/build.yaml"
git -C "${fixture}" add build.yaml; git -C "${fixture}" commit -qm unknown
head="$(git -C "${fixture}" rev-parse HEAD)"; out="${fixture}/unknown.output"
(cd "${fixture}" && "${SCRIPT}" pull_request "${base}" "${head}" "${out}")
expect_only "${out}" "${flags[@]}"
expect_flag "${out}" reason unknown_path:build.yaml
out="${fixture}/empty.output"; (cd "${fixture}" && "${SCRIPT}" pull_request "${base}" "${base}" "${out}")
expect_only "${out}" "${flags[@]}"; expect_flag "${out}" reason empty_diff_fail_closed
out="${fixture}/invalid.output"; (cd "${fixture}" && "${SCRIPT}" pull_request invalid "${base}" "${out}")
expect_only "${out}" "${flags[@]}"; expect_flag "${out}" reason invalid_sha
out="${fixture}/unresolvable.output"; (cd "${fixture}" && "${SCRIPT}" pull_request 1111111111111111111111111111111111111111 "${base}" "${out}")
expect_only "${out}" "${flags[@]}"; expect_flag "${out}" reason unresolvable_sha
out="${fixture}/zero-before.output"; (cd "${fixture}" && "${SCRIPT}" push 0000000000000000000000000000000000000000 "${base}" "${out}")
expect_only "${out}" "${flags[@]}"; expect_flag "${out}" reason all_zero_before_sha
out="${fixture}/dispatch.output"; (cd "${fixture}" && "${SCRIPT}" workflow_dispatch invalid invalid "${out}")
expect_only "${out}" "${flags[@]}"; expect_flag "${out}" reason workflow_dispatch_full_validation

# Push uses before..head. PR uses base-tip...head, excluding a base-only change
# added after the PR branch point.
git -C "${fixture}" checkout -q -B pr-branch "${base}"
printf 'pr\n' >>"${fixture}/web/test/unit.test.ts"
git -C "${fixture}" commit -qam pr-only
pr_head="$(git -C "${fixture}" rev-parse HEAD)"
git -C "${fixture}" checkout -q -B advanced-base "${base}"
printf 'base-only\n' >>"${fixture}/control-plane/internal/auth/store.go"
git -C "${fixture}" commit -qam base-only
base_tip="$(git -C "${fixture}" rev-parse HEAD)"
out="${fixture}/three-dot.output"
(cd "${fixture}" && "${SCRIPT}" pull_request "${base_tip}" "${pr_head}" "${out}")
expect_only "${out}" run_web
expect_flag "${out}" changed_count 1
out="${fixture}/push.output"
(cd "${fixture}" && "${SCRIPT}" push "${base}" "${base_tip}" "${out}")
expect_only "${out}" run_go_standard run_go_race run_runtime_artifacts run_database
expect_flag "${out}" changed_count 1

echo "CI relevance classifier tests passed"
