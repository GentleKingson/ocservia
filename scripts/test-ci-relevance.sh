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
flags=(run_docs run_go run_rust run_web run_database run_ci_tools run_installers run_controller_cache)
expect() {
  local out=$1 key=$2 value=$3
  grep -Fxq "${key}=${value}" "${out}" || { cat "${out}" >&2; echo "expected ${key}=${value}" >&2; exit 1; }
}
check_matrix() {
  local out=$1 profile=$2
  sed -n 's/^database_matrix=//p' "${out}" | jq -e --arg profile "${profile}" '
    if $profile == "full" then
      .include == [{"engine":"postgres","postgres":"18","version":"18.6"},
        {"engine":"mysql","version":"8.4.10","shard":"mysql-cutover"},
        {"engine":"mysql","version":"8.4.10","shard":"mysql-core"},
        {"engine":"mysql","version":"8.4.10","shard":"services"}]
    else
      .include == [{"engine":"postgres","postgres":"18","version":"18.6"},{"engine":"mysql","version":"8.4.10"}]
    end' >/dev/null
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
      grep -Eq '^ci_suites=(guards|release)( (guards|release))*$' "${out}"
      case "${path}" in
        scripts/ci-tools-check.sh) expect "${out}" ci_suites 'guards release' ;;
        scripts/buildx-cache.sh|.github/actions/build-cache-credentials/index.js)
          expect "${out}" ci_suites 'release' ;;
        .github/workflows/ci.yml|.github/workflows/security.yml|scripts/ci-relevance.sh)
          expect "${out}" ci_suites guards ;;
        .github/workflows/release*.yml|.github/release.yml|.github/release-notes/*.md|scripts/release-quick-acceptance.sh)
          expect "${out}" ci_suites release ;;
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
CLAUDE.md run_docs
.claude/agents/sonnet-implementer.md run_docs
.claude/settings.json run_docs
.claude/settings.local.json run_docs run_go run_rust run_web run_database run_ci_tools run_installers run_controller_cache
.claude/hooks/pre-tool.sh run_docs run_go run_rust run_web run_database run_ci_tools run_installers run_controller_cache
.github/release.yml run_ci_tools
.github/release-notes/v9.9.9.md run_docs run_ci_tools
.github/release-notes/v9.9.9.json run_docs run_go run_rust run_web run_database run_ci_tools run_installers run_controller_cache
web/src/App.vue run_web run_controller_cache
web/src/api/generated/index.ts run_web run_controller_cache
rust/crates/agent/src/lib.rs run_rust run_controller_cache
control-plane/internal/platform/app/run.go run_go run_database run_controller_cache
control-plane/internal/database/mysql/backend.go run_go run_database run_controller_cache
control-plane/internal/api/routes.go run_go run_database run_controller_cache
control-plane/migrations/000036.up.sql run_go run_database run_controller_cache
scripts/database-foundation-integration.sh run_go run_database
scripts/go-check.sh run_go
signer/http.go run_go run_controller_cache
signer/store_test.go run_go run_controller_cache
signer/go.sum run_go run_controller_cache
signer/notes.txt run_docs run_go run_rust run_web run_database run_ci_tools run_installers run_controller_cache
scripts/web-check.sh run_web
deploy/managed-node/install.sh run_installers
deploy/production/install.sh run_installers
deploy/production/controller-bootstrap.sh run_installers
deploy/lib/install-env.sh run_installers
deploy/production/transportd-relays.sh run_installers run_controller_cache
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
deploy/production/Caddyfile run_ci_tools run_installers run_controller_cache
deploy/production/compose.yaml run_ci_tools run_installers
deploy/production/backup-entrypoint.sh run_ci_tools run_installers run_controller_cache
deploy/real-e2e/compose.yaml run_ci_tools
scripts/upgrade-agent.sh run_ci_tools run_installers
scripts/rollback-agent.sh run_ci_tools run_installers
scripts/uninstall-agent.sh run_ci_tools run_installers
.github/workflows/ci.yml run_ci_tools run_controller_cache
.github/workflows/release.yml run_ci_tools run_installers
.github/workflows/release-upgrade.yml run_ci_tools run_installers
.github/workflows/security.yml run_ci_tools
scripts/ci-relevance.sh run_ci_tools
scripts/ci-tools-check.sh run_ci_tools
scripts/bootstrap.sh run_docs run_go run_rust run_web run_database run_ci_tools run_installers run_controller_cache
scripts/buildx-cache.sh run_ci_tools run_controller_cache
.github/actions/build-cache-credentials/index.js run_ci_tools run_controller_cache
deploy/test-fixtures/relay.toml run_go run_database run_ci_tools
deploy/test-fixtures/scheduler-maintenance.sql run_go run_database run_ci_tools
rust/test-runtime.Dockerfile run_go run_database run_ci_tools
scripts/secret-scan.toml run_ci_tools
deploy/package/nfpm.yaml run_ci_tools run_installers
unclassified.conf run_docs run_go run_rust run_web run_database run_ci_tools run_installers run_controller_cache
THIRD_PARTY_NOTICES.md run_docs run_controller_cache
deploy/production/relay.Dockerfile run_ci_tools run_installers run_controller_cache
deploy/production/integrated/nginx.conf.template run_ci_tools run_installers run_controller_cache
deploy/production/relay.Cargo.lock run_ci_tools run_installers run_controller_cache
scripts/mysql-backup.sh run_go run_database run_controller_cache
scripts/build-relay.sh run_rust run_ci_tools run_controller_cache
scripts/build-release-controller.sh run_ci_tools run_installers run_controller_cache
rust/transportd.Dockerfile run_ci_tools run_controller_cache
signer/Dockerfile run_docs run_go run_rust run_web run_database run_ci_tools run_installers run_controller_cache
.dockerignore run_docs run_go run_rust run_web run_database run_ci_tools run_installers run_controller_cache
scripts/release-quick-acceptance.sh run_ci_tools
deploy/production/quick-install.sh run_ci_tools run_installers
rust/agent-build.Dockerfile run_ci_tools
CASES
for profile in quick full; do
  out="${fixture}/dispatch-${profile}.output"
  (cd "${fixture}" && bash "${SCRIPT}" workflow_dispatch invalid invalid "${out}" "${profile}")
  for flag in run_docs run_go run_rust run_web run_database; do expect "${out}" "${flag}" true; done
  expect "${out}" run_ci_tools false
  expect "${out}" run_installers false
  expect "${out}" run_controller_cache false
  expect "${out}" profile "${profile}"
  if [[ "${profile}" == full ]]; then expect "${out}" database_scope full; else expect "${out}" database_scope smoke; fi
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
expect "${out}" run_web true; expect "${out}" run_docs true; expect "${out}" run_controller_cache true
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
# Settings routed to Docs alone must still be rejected by docs-check when they
# carry unaudited keys that run code or widen authority.
docs="${fixture}/docs-check"
mkdir -p "${docs}/scripts" "${docs}/.claude" "${docs}/docs/getting-started"
cp "${ROOT}/scripts/docs-check.sh" "${ROOT}/scripts/env.sh" "${docs}/scripts/"
printf '# Docs\n\ndocs/getting-started/production.md\n' >"${docs}/README.md"
for page in production managed-node; do printf '# Page\n' >"${docs}/docs/getting-started/${page}.md"; done
git -C "${docs}" init -q
git -C "${docs}" add .
cp "${ROOT}/.claude/settings.json" "${docs}/.claude/settings.json"
bash "${docs}/scripts/docs-check.sh"
# Narrowing rules stay free-form; an empty allow list grants nothing.
printf '%s\n' '{"permissions":{"allow":[],"ask":["Bash(git rebase:*)"],"deny":["Bash(git push --force:*)"]}}' \
  >"${docs}/.claude/settings.json"
bash "${docs}/scripts/docs-check.sh"
# Every allow rule must match the reviewed list exactly, so structurally valid
# but unreviewed grants (history rewrites, wildcards, other tools) fail.
for settings in '{"hooks":{}}' '{"env":{"A":"b"}}' '{"permissions":{"defaultMode":"bypassPermissions"}}' \
  '{"permissions":{"allow":[1]}}' '{"model":"x"}{"model":"y"}' 'not json' \
  '{"permissions":{"allow":["Bash(git filter-branch:*)"]}}' \
  '{"permissions":{"allow":["Bash(git push --force-with-lease:*)"]}}' \
  '{"permissions":{"allow":["Bash(*)"]}}' '{"permissions":{"allow":["Bash"]}}' \
  '{"permissions":{"allow":["WebFetch"]}}'; do
  printf '%s\n' "${settings}" >"${docs}/.claude/settings.json"
  if bash "${docs}/scripts/docs-check.sh" 2>/dev/null; then echo "docs-check accepted ${settings}" >&2; exit 1; fi
done
echo 'CI routing: domain isolation, exact installer/workflow contracts, Controller Quick, Full matrix and fallback passed'
