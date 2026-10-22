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
flags=(run_docs run_go run_rust run_web run_web_browser run_database run_ci_tools run_installers run_controller_cache)
all_images='gateway control transport backup edge relay signer mysql_backup'
expect() {
  local out=$1 key=$2 value=$3
  grep -Fxq "${key}=${value}" "${out}" || { cat "${out}" >&2; echo "expected ${key}=${value}" >&2; exit 1; }
}
expect_images() {
  local out=$1 expected=$2 actual actual_sorted expected_sorted
  local -a actual_images=() expected_tokens=()
  [[ "$(grep -c '^controller_cache_images=' "${out}")" == 1 ]] || {
    cat "${out}" >&2; echo 'expected one Controller cache image list' >&2; exit 1;
  }
  actual="$(sed -n 's/^controller_cache_images=//p' "${out}")"
  read -r -a actual_images <<<"${actual}"
  read -r -a expected_tokens <<<"${expected}"
  actual_sorted="$(printf '%s\n' "${actual_images[@]}" | LC_ALL=C sort)"
  expected_sorted="$(printf '%s\n' "${expected_tokens[@]}" | LC_ALL=C sort)"
  [[ "${actual_sorted}" == "${expected_sorted}" ]] || {
    cat "${out}" >&2; echo "expected Controller cache images: ${expected}" >&2; exit 1;
  }
  if ((${#actual_images[@]})); then expect "${out}" run_controller_cache true
  else expect "${out}" run_controller_cache false; fi
}
check_matrix() {
  local out=$1 profile=$2
  sed -n 's/^database_matrix=//p' "${out}" | jq -e --arg profile "${profile}" '
    if $profile == "full" then
      .include == [{"engine":"postgres","postgres":"18","version":"18.6"},
        {"engine":"mysql","version":"8.4.11","shard":"mysql-cutover"},
        {"engine":"mysql","version":"8.4.11","shard":"mysql-core"},
        {"engine":"mysql","version":"8.4.11","shard":"services"}]
    else
      .include == [{"engine":"postgres","postgres":"18","version":"18.6"},{"engine":"mysql","version":"8.4.11"}]
    end' >/dev/null
}
while read -r path selected; do
  git -C "${fixture}" checkout -q --detach "${base}"
  mkdir -p "${fixture}/$(dirname "${path}")"
  printf 'change\n' >"${fixture}/${path}"
  # -f: a user's global excludes (e.g. .claude/settings.local.json) must not drop fixture paths.
  git -C "${fixture}" add -f -- "${path}"
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
    expected_images=''
    if [[ " ${selected} " == *' run_controller_cache '* ]]; then
      expected_images="${all_images}"
      case "${path}" in
        web/*|deploy/production/gateway.Dockerfile|deploy/production/Caddyfile) expected_images=gateway ;;
        control-plane/gen/*) ;;
        control-plane/*|THIRD_PARTY_NOTICES.md) expected_images=control ;;
        rust/Cargo.toml|rust/Cargo.lock|rust/rust-toolchain.toml|rust/crates/*|rust/vendor/*|rust/.cargo/*|rust/transportd.Dockerfile|deploy/production/transportd-relays.sh|deploy/prepare-transport-runtime.sh)
          expected_images=transport ;;
        deploy/production/backup-entrypoint.sh) expected_images='backup mysql_backup' ;;
        deploy/production/backup.Dockerfile|scripts/postgres-backup.sh) expected_images=backup ;;
        deploy/production/backup.mysql.Dockerfile|scripts/mysql-backup.sh) expected_images=mysql_backup ;;
        deploy/production/edge.Dockerfile) expected_images=edge ;;
        deploy/production/integrated/*) expected_images='gateway edge' ;;
        deploy/production/relay.Dockerfile|deploy/production/relay.Cargo.lock|scripts/build-relay.sh)
          expected_images=relay ;;
        signer/*.go|signer/go.mod|signer/go.sum|deploy/production/signer.Dockerfile) expected_images=signer ;;
      esac
    fi
    expect_images "${out}" "${expected_images}"
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
.claude/settings.local.json run_docs run_go run_rust run_web run_web_browser run_database run_ci_tools run_installers run_controller_cache
.claude/hooks/pre-tool.sh run_docs run_go run_rust run_web run_web_browser run_database run_ci_tools run_installers run_controller_cache
.github/release.yml run_ci_tools
.github/release-notes/v9.9.9.md run_docs run_ci_tools
.github/release-notes/v9.9.9.json run_docs run_go run_rust run_web run_web_browser run_database run_ci_tools run_installers run_controller_cache
web/src/App.vue run_web run_web_browser run_controller_cache
web/src/api/generated/index.ts run_web run_web_browser run_controller_cache
web/src/views/NodesView.vue run_web run_web_browser run_controller_cache
web/src/views/OverviewView.vue run_web run_web_browser run_controller_cache
web/src/components/overview/MetricCard.vue run_web run_web_browser run_controller_cache
web/src/components/nodes/NodeDetailHeader.vue run_web run_web_browser run_controller_cache
web/src/features/configuration/useNodeConfiguration.ts run_web run_web_browser run_controller_cache
web/src/features/user-password.ts run_web run_web_browser run_controller_cache
web/src/views/AuditView.vue run_web run_web_browser run_controller_cache
web/src/views/ApprovalsView.vue run_web run_web_browser run_controller_cache
web/src/views/SettingsView.vue run_web run_web_browser run_controller_cache
web/e2e/login.spec.ts run_web run_web_browser run_controller_cache
rust/crates/agent/src/lib.rs run_rust run_controller_cache
rust/vendor/iroh/src/lib.rs run_rust run_controller_cache
rust/.cargo/config.toml run_rust run_controller_cache
rust/Cargo.lock run_rust run_controller_cache
proto/controller.proto run_docs run_go run_rust run_web run_web_browser run_database run_ci_tools run_installers run_controller_cache
control-plane/internal/platform/app/run.go run_go run_database run_controller_cache
control-plane/internal/database/mysql/backend.go run_go run_database run_controller_cache
control-plane/internal/api/routes.go run_go run_database run_controller_cache
control-plane/internal/api/node_boundary_test.go run_go run_database
control-plane/migrations/cutover_test.go run_go run_database
control-plane/Dockerfile run_go run_controller_cache
control-plane/internal/auth/password_blocklist/common-passwords.txt run_go run_database run_controller_cache
control-plane/migrations/000036.up.sql run_go run_database run_controller_cache
scripts/database-foundation-integration.sh run_go run_database
scripts/go-check.sh run_go
signer/http.go run_go run_controller_cache
signer/store_test.go run_go
signer/go.sum run_go run_controller_cache
signer/notes.txt run_docs run_go run_rust run_web run_web_browser run_database run_ci_tools run_installers run_controller_cache
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
deploy/production/gateway.Dockerfile run_ci_tools run_installers run_controller_cache
deploy/production/backup.Dockerfile run_ci_tools run_installers run_controller_cache
deploy/production/backup.mysql.Dockerfile run_ci_tools run_installers run_controller_cache
deploy/production/edge.Dockerfile run_ci_tools run_installers run_controller_cache
deploy/production/signer.Dockerfile run_ci_tools run_installers run_controller_cache
deploy/production/future.Dockerfile run_ci_tools run_installers run_controller_cache
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
scripts/bootstrap.sh run_docs run_go run_rust run_web run_web_browser run_database run_ci_tools run_installers run_controller_cache
scripts/buildx-cache.sh run_ci_tools run_controller_cache
.github/actions/build-cache-credentials/index.js run_ci_tools run_controller_cache
deploy/test-fixtures/relay.toml run_go run_database run_ci_tools
deploy/test-fixtures/scheduler-maintenance.sql run_go run_database run_ci_tools
rust/test-runtime.Dockerfile run_go run_database run_ci_tools
scripts/secret-scan.toml run_ci_tools
deploy/package/nfpm.yaml run_ci_tools run_installers
unclassified.conf run_docs run_go run_rust run_web run_web_browser run_database run_ci_tools run_installers run_controller_cache
THIRD_PARTY_NOTICES.md run_docs run_controller_cache
deploy/production/relay.Dockerfile run_ci_tools run_installers run_controller_cache
deploy/production/integrated/nginx.conf.template run_ci_tools run_installers run_controller_cache
deploy/production/relay.Cargo.lock run_ci_tools run_installers run_controller_cache
scripts/mysql-backup.sh run_go run_database run_controller_cache
scripts/postgres-backup.sh run_go run_database run_controller_cache
deploy/prepare-transport-runtime.sh run_ci_tools run_installers run_controller_cache
scripts/build-relay.sh run_rust run_ci_tools run_controller_cache
scripts/build-release-controller.sh run_ci_tools run_installers run_controller_cache
rust/transportd.Dockerfile run_ci_tools run_controller_cache
signer/Dockerfile run_docs run_go run_rust run_web run_web_browser run_database run_ci_tools run_installers run_controller_cache
.dockerignore run_docs run_go run_rust run_web run_web_browser run_database run_ci_tools run_installers run_controller_cache
scripts/release-quick-acceptance.sh run_ci_tools
deploy/production/quick-install.sh run_ci_tools run_installers
rust/agent-build.Dockerfile run_ci_tools
CASES
for profile in quick full; do
  out="${fixture}/dispatch-${profile}.output"
  (cd "${fixture}" && bash "${SCRIPT}" workflow_dispatch invalid invalid "${out}" "${profile}")
  for flag in run_docs run_go run_rust run_web run_web_browser run_database; do expect "${out}" "${flag}" true; done
  expect "${out}" run_ci_tools false
  expect "${out}" run_installers false
  expect "${out}" run_controller_cache false
  expect_images "${out}" ''
  expect "${out}" profile "${profile}"
  if [[ "${profile}" == full ]]; then expect "${out}" database_scope full; else expect "${out}" database_scope smoke; fi
  check_matrix "${out}" "${profile}"
done
out="${fixture}/invalid.output"
(cd "${fixture}" && bash "${SCRIPT}" push invalid invalid "${out}")
expect "${out}" profile quick
check_matrix "${out}" quick
for flag in "${flags[@]}"; do expect "${out}" "${flag}" true; done
expect_images "${out}" "${all_images}"
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
expect_images "${out}" gateway
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
  expect_images "${out}" gateway
done
# Multiple paths select the union once; a Go test cannot add its image.
git -C "${fixture}" checkout -q --detach "${base}"
mkdir -p "${fixture}/web" "${fixture}/control-plane/internal" "${fixture}/signer"
for path in web/first.ts web/second.ts control-plane/internal/run.go signer/store_test.go; do
  printf 'mixed\n' >"${fixture}/${path}"
  git -C "${fixture}" add -f -- "${path}"
done
git -C "${fixture}" commit -qm mixed
out="${fixture}/mixed.output"
(cd "${fixture}" && bash "${SCRIPT}" push "${base}" "$(git rev-parse HEAD)" "${out}")
expect_images "${out}" 'gateway control'
# Moving a source between images includes the old and new owners. Deleting it
# later still refreshes the image which previously copied that source.
git -C "${fixture}" checkout -q --detach "${ancestor}"
mkdir -p "${fixture}/control-plane/internal"
git -C "${fixture}" mv web/source.ts control-plane/internal/renamed.go
git -C "${fixture}" commit -qm rename-between-images
image_renamed="$(git -C "${fixture}" rev-parse HEAD)"
out="${fixture}/image-rename.output"
(cd "${fixture}" && bash "${SCRIPT}" push "${ancestor}" "${image_renamed}" "${out}")
expect_images "${out}" 'gateway control'
git -C "${fixture}" rm -q control-plane/internal/renamed.go
git -C "${fixture}" commit -qm delete-image-source
out="${fixture}/image-delete.output"
(cd "${fixture}" && bash "${SCRIPT}" push "${image_renamed}" "$(git rev-parse HEAD)" "${out}")
expect_images "${out}" control
for pair in "${base} ${base}" "0000000000000000000000000000000000000000 ${base}" "1111111111111111111111111111111111111111 ${base}"; do
  read -r before after <<<"${pair}"
  out="${fixture}/fallback.output"; rm -f "${out}"
  (cd "${fixture}" && bash "${SCRIPT}" push "${before}" "${after}" "${out}")
  for flag in "${flags[@]}"; do expect "${out}" "${flag}" true; done
  expect_images "${out}" "${all_images}"
done
git -C "${fixture}" checkout -q --detach "${ancestor}"
git -C "${fixture}" checkout -q --orphan unrelated
git -C "${fixture}" rm -qrf .
git -C "${fixture}" commit --allow-empty -qm unrelated
out="${fixture}/unrelated.output"
(cd "${fixture}" && bash "${SCRIPT}" pull_request "${base}" "$(git rev-parse HEAD)" "${out}")
expect "${out}" reason merge_base_unresolvable
for flag in "${flags[@]}"; do expect "${out}" "${flag}" true; done
expect_images "${out}" "${all_images}"
# Settings routed to Docs alone must still be rejected by docs-check when they
# carry unaudited keys that run code or widen authority.
docs="${fixture}/docs-check"
mkdir -p "${docs}/scripts" "${docs}/.claude" "${docs}/docs/getting-started"
cp "${ROOT}/scripts/docs-check.sh" "${ROOT}/scripts/env.sh" "${docs}/scripts/"
printf '# Docs\n\ndocs/getting-started/production.md\n' >"${docs}/README.md"
for page in production managed-node; do printf '# Page\n' >"${docs}/docs/getting-started/${page}.md"; done
printf '# Rules\n\n[Install](docs/getting-started/production.md#page)\n' >"${docs}/AGENTS.md"
printf '# Claude\n\n@AGENTS.md\n' >"${docs}/CLAUDE.md"
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
# CLAUDE.md must import AGENTS.md outside code, and AGENTS.md links must resolve.
cp "${ROOT}/.claude/settings.json" "${docs}/.claude/settings.json"
# shellcheck disable=SC2016 # Markdown fences are literal fixture text.
for claude in '# Claude\n\nRules live in AGENTS.md.\n' '# Claude\n\n```\n@AGENTS.md\n```\n'; do
  printf '%b' "${claude}" >"${docs}/CLAUDE.md"
  if bash "${docs}/scripts/docs-check.sh" 2>/dev/null; then echo "docs-check accepted CLAUDE.md without an import" >&2; exit 1; fi
done
printf '# Claude\n\n@AGENTS.md\n' >"${docs}/CLAUDE.md"
for link in docs/missing.md docs/getting-started/production.md#missing; do
  printf '# Rules\n\n[Broken](%s)\n' "${link}" >"${docs}/AGENTS.md"
  if bash "${docs}/scripts/docs-check.sh" 2>/dev/null; then echo "docs-check accepted AGENTS.md link ${link}" >&2; exit 1; fi
done
echo 'CI routing: domain isolation, exact installer/workflow contracts, Controller Quick, Full matrix and fallback passed'
