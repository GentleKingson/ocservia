#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="${ROOT}/scripts/ci-relevance.sh"
fixture="$(mktemp -d)"
trap 'rm -rf -- "${fixture}"' EXIT

flags=(run_docs run_go run_rust run_web run_database)

git -C "${fixture}" init -q
git -C "${fixture}" config user.name test
git -C "${fixture}" config user.email test@example.invalid
printf '*.output\n' >>"${fixture}/.git/info/exclude"
paths=(
  README.md docs/development/guide.md docs/acceptance/g6-slo.yaml scripts/README.md web/README.md
  web/src/App.vue web/test/unit.test.ts web/e2e/app.spec.ts web/package.json
  control-plane/internal/domain/helper.go control-plane/internal/auth/store.go
  control-plane/migrations/000001.up.sql control-plane/go.mod go.work
  rust/crates/agent/src/lib.rs rust/Cargo.lock rust/rust-toolchain.toml
  .github/workflows/ci.yml scripts/ci-relevance.sh toolchains.lock Makefile
  .github/workflows/g6-readiness.yml .github/workflows/g6-harness-core.yml
  .github/actions/g6-install-release/action.yml
  scripts/g6-readiness-fd-a.sh scripts/test-g6-workflow-contract.sh
  scripts/g6-buildx-cache.sh scripts/g6-timing.sh .github/actions/g6-cache-credentials/action.yml
  tools/g6-harness/internal/runtime/orchestrator.go deploy/g6-readiness/compose.yaml
  rust/g6-runtime.Dockerfile rust/crates/g6-probe/src/main.rs
  deploy/real-e2e/controller.compose.yaml
  scripts/real-e2e-controller.sh scripts/real-e2e-node.sh scripts/real-e2e-artifact.sh
  scripts/test-real-e2e-artifact.sh scripts/p1-resilience-capacity.sh
  scripts/test-p1-resilience-capacity.sh
  scripts/security-acceptance-f1.sh scripts/security-acceptance-f2.sh scripts/security-acceptance-f3.sh
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


# Only the five basic domains can be selected.
out="$(case_commit readme README.md)"
expect_only "${out}" run_docs
out="$(case_commit docs docs/development/guide.md docs/acceptance/g6-slo.yaml scripts/README.md web/README.md)"
expect_only "${out}" run_docs
for path in web/src/App.vue web/test/unit.test.ts web/e2e/app.spec.ts web/package.json; do
  out="$(case_commit "web_$(basename "${path}")" "${path}")"
  expect_only "${out}" run_web run_docs
done
for path in control-plane/internal/domain/helper.go control-plane/internal/auth/store.go control-plane/migrations/000001.up.sql control-plane/go.mod go.work; do
  out="$(case_commit "go_$(basename "${path}")" "${path}")"
  expect_only "${out}" run_go run_database
done
for path in rust/crates/agent/src/lib.rs rust/Cargo.lock rust/rust-toolchain.toml; do
  out="$(case_commit "rust_$(basename "${path}")" "${path}")"
  expect_only "${out}" run_rust
done
for path in .github/workflows/ci.yml scripts/ci-relevance.sh toolchains.lock Makefile; do
  out="$(case_commit "infra_$(basename "${path}")" "${path}")"
  expect_only "${out}" "${flags[@]}"
done

# G6-only changes select basic documentation checks, never acceptance.
for path in .github/workflows/g6-readiness.yml .github/workflows/g6-harness-core.yml \
  .github/actions/g6-install-release/action.yml \
  scripts/g6-readiness-fd-a.sh scripts/test-g6-workflow-contract.sh \
  scripts/g6-buildx-cache.sh scripts/g6-timing.sh .github/actions/g6-cache-credentials/action.yml \
  tools/g6-harness/internal/runtime/orchestrator.go deploy/g6-readiness/compose.yaml \
  rust/g6-runtime.Dockerfile rust/crates/g6-probe/src/main.rs; do
  out="$(case_commit "g6_$(basename "${path}")" "${path}")"
  expect_only "${out}" run_docs
done

# Script-level manual acceptance changes select only basic documentation checks.
for path in deploy/real-e2e/controller.compose.yaml \
  scripts/real-e2e-controller.sh scripts/real-e2e-node.sh scripts/real-e2e-artifact.sh \
  scripts/test-real-e2e-artifact.sh scripts/p1-resilience-capacity.sh \
  scripts/test-p1-resilience-capacity.sh \
  scripts/security-acceptance-f1.sh scripts/security-acceptance-f2.sh scripts/security-acceptance-f3.sh; do
  out="$(case_commit "manual_$(basename "${path}")" "${path}")"
  expect_only "${out}" run_docs
done

# Mixed changes are the union of their basic domains.
out="$(case_commit docs_go docs/development/guide.md control-plane/internal/domain/helper.go)"
expect_only "${out}" run_docs run_go run_database
out="$(case_commit web_rust web/src/App.vue rust/crates/agent/src/lib.rs)"
expect_only "${out}" run_docs run_web run_rust
out="$(case_commit migration_web control-plane/migrations/000001.up.sql web/src/App.vue)"
expect_only "${out}" run_docs run_go run_database run_web

# Deletions retain the old path's impact. With rename detection off, both
# sides of a cross-domain rename contribute to the union.
git -C "${fixture}" checkout -q --detach "${base}"
git -C "${fixture}" rm -q web/test/unit.test.ts
git -C "${fixture}" commit -qm delete_known
head="$(git -C "${fixture}" rev-parse HEAD)"; out="${fixture}/delete.output"
(cd "${fixture}" && "${SCRIPT}" pull_request "${base}" "${head}" "${out}")
expect_only "${out}" run_docs run_web
git -C "${fixture}" checkout -q --detach "${base}"
mkdir -p "${fixture}/rust/crates/observability/src"
git -C "${fixture}" mv docs/development/guide.md rust/crates/observability/src/guide.rs
git -C "${fixture}" commit -qm rename_known
head="$(git -C "${fixture}" rev-parse HEAD)"; out="${fixture}/rename.output"
(cd "${fixture}" && "${SCRIPT}" pull_request "${base}" "${head}" "${out}")
expect_only "${out}" run_docs run_rust

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
expect_only "${out}" "${flags[@]}"; expect_flag "${out}" reason workflow_dispatch_basic_checks

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
expect_only "${out}" run_docs run_web
expect_flag "${out}" changed_count 1
out="${fixture}/push.output"
(cd "${fixture}" && "${SCRIPT}" push "${base}" "${base_tip}" "${out}")
expect_only "${out}" run_go run_database
expect_flag "${out}" changed_count 1

echo "CI relevance classifier tests passed"
