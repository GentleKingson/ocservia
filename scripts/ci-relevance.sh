#!/usr/bin/env bash
# Path routing for Basic CI only.
set -euo pipefail

if (($# < 4 || $# > 5)); then
  echo "usage: $0 <event-name> <base-sha> <head-sha> <github-output> [quick|full]" >&2
  exit 2
fi

event="$1"; base_sha="$2"; head_sha="$3"; output="$4"
profile="${5-full}"
if [[ "${event}" != workflow_dispatch ]]; then profile=quick; fi
case "${profile}" in
  quick|full) ;;
  *) echo 'CI profile must be quick or full' >&2; exit 2 ;;
esac
database_scope=smoke
if [[ "${profile}" == full ]]; then database_scope=full; fi
flags=(run_docs run_go run_rust run_web run_web_browser run_database run_ci_tools run_installers run_controller_cache)
for flag in "${flags[@]}"; do printf -v "${flag}" false; done
reason=recognized_paths
changed=()
ci_suites=()

tools_suite() {
  # Read indirectly with the other routing flags below.
  # shellcheck disable=SC2034
  run_ci_tools=true
  local suite
  for suite in "${ci_suites[@]}"; do [[ "${suite}" != "$1" ]] || return 0; done
  ci_suites+=("$1")
}

fail_closed() {
  local flag
  reason="$1"
  for flag in "${flags[@]}"; do printf -v "${flag}" true; done
  tools_suite guards; tools_suite release
}

# Flags are read indirectly through ${!flag} when writing the outputs.
# shellcheck disable=SC2034
classify_path() {
  local path="$1"
  case "${path}" in
    # Executable/configuration contracts must precede documentation suffixes.
    scripts/ci-tools-check.sh)
      tools_suite guards; tools_suite release ;;
    scripts/build-relay.sh)
      run_rust=true; tools_suite release ;;
    scripts/buildx-cache.sh|scripts/test-buildx-cache-fallback.sh|.github/actions/build-cache-credentials/*)
      tools_suite release ;;
    scripts/test-build-cache-credentials.sh|scripts/secret-scan.toml|scripts/test-secret-scan-config*.sh)
      tools_suite release ;;
    deploy/test-fixtures/*|rust/test-runtime.Dockerfile)
      run_go=true; run_database=true; tools_suite release ;;
    proto/*|openapi/*|control-plane/gen/*) fail_closed shared_contract_changed ;;
    web/*|scripts/web-check.sh) run_web=true ;;
    rust/agent-build.Dockerfile|rust/transportd.Dockerfile)
      tools_suite release ;;
    rust/*|scripts/rust-check.sh) run_rust=true ;;
    # Run only by Release Check; no installer contract executes it, while
    # test-release-workflows.rb (release suite) asserts that Release Check calls it.
    scripts/release-quick-acceptance.sh) tools_suite release ;;
    deploy/managed-node/install.sh|deploy/production/install.sh|deploy/production/controller-bootstrap.sh|\
    deploy/lib/install-env.sh|deploy/production/transportd-relays.sh|deploy/production/systemd/agent-relays.sh|\
    deploy/production/systemd/ocservia-agent-relays.conf|scripts/prepare-bootstrap-release-assets.sh|\
    scripts/release-agent-state-check.sh|scripts/package-agent.sh|scripts/verify-agent-package.sh|\
    scripts/test-managed-node-install.sh|scripts/test-controller-install.sh|scripts/test-controller-bootstrap.sh|\
    scripts/test-relay-launchers.py|scripts/test-release-agent-state-check.sh|.gitignore)
      run_installers=true ;;
    .github/workflows/release*.yml|scripts/*release*|scripts/build-agent-binaries.sh|\
    scripts/*controller*|scripts/*agent-upgrade*|scripts/install-agent.sh|scripts/upgrade-agent.sh|\
    scripts/rollback-agent.sh|scripts/uninstall-agent.sh|deploy/package/*|deploy/systemd/*|\
    deploy/bootstrap/*|deploy/managed-node/*|deploy/production/*|deploy/prepare-transport-runtime.sh)
      tools_suite release; run_installers=true ;;
    deploy/compose/*|deploy/database-e2e/*|scripts/*database*|scripts/*mysql*|scripts/*postgres*|\
    scripts/i18-*-backup-restore-smoke.sh)
      run_go=true; run_database=true ;;
    # test-release-workflows.rb (release suite) reads the Release notes config and bridge.
    .github/release.yml) tools_suite release ;;
    .github/release-notes/*.md) run_docs=true; tools_suite release ;;
    # Shared Claude Code settings never reach CI or images; docs-check enforces its audited keys.
    # Other non-Markdown .claude/ files (hooks, local settings) stay unknown and fail closed.
    .claude/settings.json) run_docs=true ;;
    docs/*.md|*.md|LICENSE|LICENSE.*) run_docs=true ;;
    control-plane/cmd/*|control-plane/migrations/*|control-plane/internal/*|control-plane/go.*|go.work*)
      run_go=true; run_database=true ;;
    control-plane/*|scripts/go-check.sh) run_go=true ;;
    # Independent module: imports no control-plane package, uses no SQL backend, and
    # go-check.sh tests it. Other signer/ files stay unknown and fail closed.
    signer/*.go|signer/go.mod|signer/go.sum) run_go=true ;;
    .github/workflows/ci.yml)
      tools_suite guards ;;
    .github/workflows/security.yml) tools_suite guards ;;
    scripts/ci-*|scripts/test-ci-*|scripts/*required-go-tests*|scripts/test-bootstrap-*|scripts/go-test-environment.sh)
      tools_suite guards ;;
    scripts/bootstrap.sh|scripts/env.sh|scripts/checksums.txt|toolchains.lock)
      fail_closed shared_toolchain_changed ;;
    deploy/real-e2e/*|scripts/real-e2e-*|scripts/test-real-e2e-*|scripts/p1-*|scripts/test-p1-*|scripts/security-acceptance-*)
      tools_suite release ;;
    *) fail_closed "unknown_path:${path}" ;;
  esac
  # Mirrors the COPY sources of the Dockerfiles built by build-release-controller.sh
  # (build context is the repository root) and the builder/cache machinery itself.
  case "${path}" in
    web/*|control-plane/*|signer/*|THIRD_PARTY_NOTICES.md|\
    rust/Cargo.toml|rust/Cargo.lock|rust/rust-toolchain.toml|rust/.cargo/*|rust/vendor/*|rust/crates/*|\
    rust/transportd.Dockerfile|\
    deploy/production/Caddyfile|deploy/production/*.Dockerfile|deploy/production/backup-entrypoint.sh|\
    deploy/production/relay-entrypoint.sh|deploy/production/relay-healthcheck.sh|\
    deploy/production/relay.Cargo.lock|deploy/production/transportd-relays.sh|\
    deploy/production/integrated/*|deploy/prepare-transport-runtime.sh|\
    scripts/postgres-backup.sh|scripts/mysql-server-check.sh|scripts/mysql-backup.sh|\
    scripts/mysql-restore-verify.sh|scripts/build-relay.sh|scripts/checksums.txt|toolchains.lock|\
    .dockerignore|scripts/build-release-controller.sh|scripts/buildx-cache.sh|\
    .github/actions/build-cache-credentials/*|.github/workflows/ci.yml)
      run_controller_cache=true ;;
  esac
  # Session/Workspace, routing, rollout and shared UI paths also run the stubbed
  # browser regressions (docs/development/web.md#validation).
  case "${path}" in
    web/src/api/*|web/src/shared/*|web/src/App.vue|web/src/main.ts|web/src/main.css|web/index.html|\
    web/src/components/ui/*|web/src/components/layout/*|web/src/features/operations/*|\
    web/src/views/LoginView.vue|web/src/views/OperationsView.vue|web/src/views/RolloutDetailView.vue|\
    web/e2e/*|web/playwright.config.ts|web/test/run-auth-browser.mjs|web/vite.config.ts|\
    web/package.json|web/package-lock.json)
      run_web_browser=true ;;
  esac
}

valid_sha() { [[ "$1" =~ ^[0-9a-f]{40}$ ]] && git cat-file -e "$1^{commit}" 2>/dev/null; }

if [[ "${event}" == workflow_dispatch ]]; then
  reason=workflow_dispatch_basic_checks
  for flag in run_docs run_go run_rust run_web run_web_browser run_database; do printf -v "${flag}" true; done
elif [[ "${event}" != pull_request && "${event}" != push ]]; then
  fail_closed "unsupported_event:${event}"
elif ! [[ "${base_sha}" =~ ^[0-9a-f]{40}$ && "${head_sha}" =~ ^[0-9a-f]{40}$ ]]; then
  fail_closed invalid_sha
elif [[ "${event}" == push && "${base_sha}" == 0000000000000000000000000000000000000000 ]]; then
  fail_closed all_zero_before_sha
elif ! valid_sha "${base_sha}" || ! valid_sha "${head_sha}"; then
  fail_closed unresolvable_sha
else
  diff_file="$(mktemp)"
  trap 'rm -f -- "${diff_file}"' EXIT
  diff_ok=true
  if [[ "${event}" == pull_request ]]; then
    if ! git merge-base "${base_sha}" "${head_sha}" >/dev/null 2>&1; then
      fail_closed merge_base_unresolvable; diff_ok=false
    elif ! git diff --name-only -z --no-renames "${base_sha}...${head_sha}" >"${diff_file}"; then
      fail_closed diff_failed; diff_ok=false
    fi
  elif ! git diff --name-only -z --no-renames "${base_sha}" "${head_sha}" >"${diff_file}"; then
    fail_closed diff_failed; diff_ok=false
  fi
  if [[ "${diff_ok}" == true ]]; then
    while IFS= read -r -d '' path; do changed+=("${path}"); done <"${diff_file}"
    if ((${#changed[@]} == 0)); then
      fail_closed empty_diff_fail_closed
    else
      for path in "${changed[@]}"; do classify_path "${path}"; done
    fi
  fi
fi

matrix='{"include":[{"engine":"postgres","postgres":"18","version":"18.6"},{"engine":"mysql","version":"8.4.10"}]}'
if [[ "${database_scope}" == full ]]; then
  # MySQL full acceptance is the critical path; its complementary shards run in parallel.
  matrix='{"include":[{"engine":"postgres","postgres":"18","version":"18.6"},{"engine":"mysql","version":"8.4.10","shard":"mysql-cutover"},{"engine":"mysql","version":"8.4.10","shard":"mysql-core"},{"engine":"mysql","version":"8.4.10","shard":"services"}]}'
fi

{
  printf 'database_matrix=%s\n' "${matrix}"
  printf 'profile=%s\n' "${profile}"
  printf 'database_scope=%s\n' "${database_scope}"
  printf 'reason=%s\n' "${reason}"
  printf 'changed_count=%s\n' "${#changed[@]}"
  printf 'ci_suites=%s\n' "${ci_suites[*]}"
  for flag in "${flags[@]}"; do printf '%s=%s\n' "${flag}" "${!flag}"; done
} >>"${output}"
