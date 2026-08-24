#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="${ROOT}/scripts/ci-relevance.sh"
fixture="$(mktemp -d)"
trap 'rm -rf -- "${fixture}"' EXIT

# Every case classifies the incremental diff between the previous fixture
# commit and the new one, mirroring one pull-request change set per case.
git -C "${fixture}" init -q
git -C "${fixture}" config user.name test
git -C "${fixture}" config user.email test@example.invalid
mkdir -p "${fixture}/docs/acceptance" "${fixture}/docs/development" \
  "${fixture}/web/src" "${fixture}/web/tests" \
  "${fixture}/control-plane/cmd/server" "${fixture}/control-plane/migrations" \
  "${fixture}/rust/agent/src" "${fixture}/.github/workflows" \
  "${fixture}/deploy/production" "${fixture}/scripts"
printf 'base\n' >"${fixture}/README.md"
printf 'base\n' >"${fixture}/docs/development/guide.md"
printf 'base\n' >"${fixture}/docs/acceptance/g6-slo.yaml"
printf 'base\n' >"${fixture}/web/src/app.ts"
printf 'base\n' >"${fixture}/web/tests/app.test.ts"
printf 'base\n' >"${fixture}/control-plane/cmd/server/main.go"
printf 'base\n' >"${fixture}/rust/agent/src/lib.rs"
printf 'base\n' >"${fixture}/control-plane/migrations/0001.up.sql"
printf 'base\n' >"${fixture}/.github/workflows/ci.yml"
printf 'base\n' >"${fixture}/toolchains.lock"
printf 'base\n' >"${fixture}/deploy/production/relay.Dockerfile"
printf 'base\n' >"${fixture}/scripts/bootstrap.sh"
printf 'base\n' >"${fixture}/web/src/unknown.bin"
git -C "${fixture}" add .
git -C "${fixture}" commit -qm base
prev="$(git -C "${fixture}" rev-parse HEAD)"

# Classifies the incremental diff and advances the base for the next case.
# The prev update stays in the parent shell because command substitution
# would otherwise discard it.
step() {
  local label="$1"
  local head
  head="$(git -C "${fixture}" rev-parse HEAD)"
  (trap - EXIT; cd "${fixture}" && "${SCRIPT}" pull_request "${prev}" "${head}" "${fixture}/${label}.output")
  prev="${head}"
}

expect_flag() {
  local output="$1" key="$2" value="$3"
  grep -qx "${key}=${value}" "${output}" || {
    echo "${key} expected ${value} in ${output}" >&2
    cat "${output}" >&2
    exit 1
  }
}

expect_full() {
  local output="$1"
  for flag in run_backend run_database run_rust run_native run_web \
    run_browser run_p1_smoke run_contracts run_security; do
    expect_flag "${output}" "${flag}" true
  done
}

# 1. README-only change: documentation edits keep the documentation policy
#    worker and the repository secret and license scan.
printf 'after\n' >"${fixture}/README.md"
git -C "${fixture}" commit -qam readme
step readme
readme_output="${fixture}/readme.output"
expect_flag "${readme_output}" category docs_only
expect_flag "${readme_output}" reason documentation_only
expect_flag "${readme_output}" run_contracts true
expect_flag "${readme_output}" run_security true
for flag in run_backend run_database run_rust run_native run_web \
  run_browser run_p1_smoke; do
  expect_flag "${readme_output}" "${flag}" false
done

# 2. Ordinary docs change: the same documentation-only authorization.
printf 'after\n' >"${fixture}/docs/development/guide.md"
git -C "${fixture}" commit -qam docs
step docs
docs_output="${fixture}/docs.output"
expect_flag "${docs_output}" category docs_only
expect_flag "${docs_output}" run_contracts true
expect_flag "${docs_output}" run_security true
expect_flag "${docs_output}" run_backend false
expect_flag "${docs_output}" run_web false

# 3. Web source only: web, browser, P1 smoke, contracts, and security run.
printf 'after\n' >"${fixture}/web/src/app.ts"
git -C "${fixture}" commit -qam websrc
step websrc
web_output="${fixture}/websrc.output"
expect_flag "${web_output}" category web_only
expect_flag "${web_output}" reason web_source_only
for flag in run_web run_browser run_p1_smoke run_contracts run_security; do
  expect_flag "${web_output}" "${flag}" true
done
for flag in run_backend run_database run_rust run_native; do
  expect_flag "${web_output}" "${flag}" false
done

# 4. Web tests only: still web-only coverage.
printf 'after\n' >"${fixture}/web/tests/app.test.ts"
git -C "${fixture}" commit -qam webtests
step webtests
webtests_output="${fixture}/webtests.output"
expect_flag "${webtests_output}" category web_only
expect_flag "${webtests_output}" run_browser true
expect_flag "${webtests_output}" run_backend false

# 5. Go source stays full validation.
printf 'after\n' >"${fixture}/control-plane/cmd/server/main.go"
git -C "${fixture}" commit -qam gosrc
step gosrc
go_output="${fixture}/gosrc.output"
expect_flag "${go_output}" reason full_validation
expect_full "${go_output}"

# 6. Rust source stays full validation.
printf 'after\n' >"${fixture}/rust/agent/src/lib.rs"
git -C "${fixture}" commit -qam rustsrc
step rustsrc
rust_output="${fixture}/rustsrc.output"
expect_full "${rust_output}"

# 7. Database migration stays full validation.
printf 'after\n' >"${fixture}/control-plane/migrations/0001.up.sql"
git -C "${fixture}" commit -qam migration
step migration
migration_output="${fixture}/migration.output"
expect_flag "${migration_output}" run_database true
expect_full "${migration_output}"

# 8. Workflow YAML stays full validation.
printf 'after\n' >"${fixture}/.github/workflows/ci.yml"
git -C "${fixture}" commit -qam workflow
step workflow
workflow_output="${fixture}/workflow.output"
expect_full "${workflow_output}"

# 9. Bootstrap and toolchain pins stay full validation.
printf 'after\n' >"${fixture}/toolchains.lock"
printf 'after\n' >"${fixture}/scripts/bootstrap.sh"
git -C "${fixture}" commit -qam toolchains
step toolchains
toolchains_output="${fixture}/toolchains.output"
expect_full "${toolchains_output}"

# 10. G6 acceptance contracts stay full validation.
printf 'after\n' >"${fixture}/docs/acceptance/g6-slo.yaml"
git -C "${fixture}" commit -qam acceptance
step acceptance
acceptance_output="${fixture}/acceptance.output"
expect_full "${acceptance_output}"

# 11. Unknown paths outside the recognized trees stay full validation; an
#     unrecognized file type inside web/ remains web-covered source.
printf 'after\n' >"${fixture}/web/src/unknown.bin"
git -C "${fixture}" commit -qam webunknown
step webunknown
webunknown_output="${fixture}/webunknown.output"
expect_flag "${webunknown_output}" category web_only
mkdir -p "${fixture}/misc-root"
printf 'after\n' >"${fixture}/misc-root/unknown.bin"
git -C "${fixture}" add misc-root
git -C "${fixture}" commit -qm unknown
step unknown
unknown_output="${fixture}/unknown.output"
expect_full "${unknown_output}"

# 12. Deleted and renamed files: deleting a web file still counts as a web
#     change; a docs-to-web rename surfaces both sides because renames are
#     disabled, so it classifies as full validation.
git -C "${fixture}" rm -q web/tests/app.test.ts
git -C "${fixture}" commit -qm deleteweb
step deleteweb
delete_output="${fixture}/deleteweb.output"
expect_flag "${delete_output}" category web_only
expect_flag "${delete_output}" run_web true
expect_flag "${delete_output}" run_backend false
git -C "${fixture}" mv docs/development/guide.md web/src/renamed-guide.ts
git -C "${fixture}" commit -qm rename
step rename
rename_output="${fixture}/rename.output"
expect_flag "${rename_output}" reason full_validation
expect_full "${rename_output}"

# 13. Mixed categories (docs plus Go) stay full validation.
printf 'mixed\n' >"${fixture}/docs/development/combined-guide.md"
printf 'mixed\n' >"${fixture}/control-plane/cmd/server/main.go"
git -C "${fixture}" add docs/development/combined-guide.md
git -C "${fixture}" commit -qam mixed
step mixed
mixed_output="${fixture}/mixed.output"
expect_full "${mixed_output}"

# 14. Empty/unresolved diffs fail closed, and non-pull-request events always
#     request full validation.
head="$(git -C "${fixture}" rev-parse HEAD)"
empty_output="${fixture}/empty.output"
(trap - EXIT; cd "${fixture}" && "${SCRIPT}" pull_request "${head}" "${head}" "${empty_output}")
expect_flag "${empty_output}" reason empty_diff_fail_closed
expect_full "${empty_output}"
push_output="${fixture}/push.output"
(trap - EXIT; cd "${fixture}" && "${SCRIPT}" push "${head}" "${head}" "${push_output}")
expect_flag "${push_output}" category full_event_fallback
expect_full "${push_output}"
dispatch_output="${fixture}/dispatch.output"
(trap - EXIT; cd "${fixture}" && "${SCRIPT}" workflow_dispatch "${head}" "${head}" "${dispatch_output}")
expect_flag "${dispatch_output}" reason non_pull_request_full_fallback
expect_full "${dispatch_output}"

echo "CI relevance classifier tests passed"
