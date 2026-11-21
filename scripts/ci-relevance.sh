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
controller_images=(gateway control transport backup edge relay signer)
controller_cache_images=()

tools_suite() {
  # shellcheck disable=SC2034 # Read indirectly via ${!flag} when writing outputs.
  run_ci_tools=true
  case " ${ci_suites[*]:-} " in *" $1 "*) return 0 ;; esac
  ci_suites+=("$1")
}

select_controller_images() {
  # shellcheck disable=SC2034 # Read indirectly via ${!flag} when writing outputs.
  run_controller_cache=true
  local image
  for image in "$@"; do
    case " ${controller_cache_images[*]:-} " in *" ${image} "*) continue ;; esac
    controller_cache_images+=("${image}")
  done
}

fail_closed() {
  local flag
  reason="$1"
  for flag in "${flags[@]}"; do printf -v "${flag}" true; done
  for flag in guards release cross-vm source-style contracts; do tools_suite "${flag}"; done
  select_controller_images "${controller_images[@]}"
}

# Flags are read indirectly through ${!flag} when writing outputs.
# shellcheck disable=SC2034
classify_path() {
  local path="$1"
  case "${path}" in
    # Shared contracts, toolchains and base CI action fail closed.
    proto/*|openapi/*|control-plane/gen/*) fail_closed shared_contract_changed ;;
    scripts/bootstrap.sh|scripts/env.sh|scripts/checksums.txt|toolchains.lock)
      fail_closed shared_toolchain_changed ;;
    .github/actions/setup-tools/*) fail_closed shared_ci_action_changed ;;

    # CI tooling, guards, contracts and style runners.
    scripts/ci-tools-check.sh)
      for suite in guards release cross-vm source-style contracts; do tools_suite "${suite}"; done ;;
    scripts/ci-go-cache-key.sh) run_go=true; tools_suite guards ;;
    .github/workflows/ci.yml|.github/workflows/security.yml|\
    .tool-versions|.nvmrc|.node-version|Makefile|\
    scripts/ci-*|scripts/*required-go-tests*|scripts/go-test-environment.sh|\
    scripts/test-ci-relevance.sh|scripts/test-toolchain-consistency.sh|\
    scripts/check-public-repository.sh|\
    scripts/license-check.sh|scripts/test.sh|scripts/verify.sh)
      tools_suite guards ;;
    scripts/generate.sh|scripts/generated-clean.sh|scripts/check-breaking.sh)
      tools_suite contracts ;;
    scripts/lint.sh) tools_suite source-style; tools_suite contracts ;;
    scripts/format.sh) run_go=true; run_rust=true; run_web=true; tools_suite contracts ;;

    # Cross-VM offline syntax check only; credentialed three-VM run stays manual.
    .github/workflows/real-e2e.yml|.github/actions/real-e2e-*/*|deploy/real-e2e/*|scripts/real-e2e-*)
      tools_suite cross-vm ;;
    # Also imported by real-e2e-business.py.
    scripts/release_business_runtime.py|scripts/release_business_scenarios.py)
      tools_suite release; tools_suite cross-vm; run_installers=true ;;

    # Release, build-cache, secret-scan and manual acceptance contract suites.
    scripts/build-relay.sh) run_rust=true; tools_suite release ;;
    .dockerignore|.gitleaksignore|.github/release.yml|\
    .github/actions/build-cache-credentials/*|.github/actions/download-release-product/*|\
    rust/agent-build.Dockerfile|rust/transportd.Dockerfile|\
    scripts/buildx-cache.sh|\
    scripts/security-check.sh|scripts/secret-scan.toml|scripts/test-secret-scan-config-runtime.sh|\
    scripts/release-quick-acceptance.sh|scripts/resilience-capacity*.sh|scripts/security-acceptance-*|\
    scripts/perf-bench.sh)
      tools_suite release ;;

    # Cross-language fixtures and integration drivers (Go + Rust).
    testdata/*|scripts/transport-boundary-check.sh|scripts/export-node-sealing-keys.py|\
    scripts/development-simulation-integration.sh|scripts/single-relay-network-smoke.py)
      run_go=true; run_rust=true ;;

    # Database and test-fixture paths.
    deploy/test-fixtures/*|rust/test-runtime.Dockerfile)
      run_go=true; run_database=true; tools_suite release ;;
    deploy/lib/postgres-entrypoint.sh)
      run_database=true; run_installers=true ;;
    deploy/compose/*|deploy/database-e2e/*|scripts/*database*|scripts/*postgres*|\
    scripts/*-backup-restore-smoke.sh|scripts/test-enrollment-restart.sh)
      run_go=true; run_database=true ;;

    # Web, Rust, Go and Signer modules.
    web/package.json) run_web=true; tools_suite guards ;;
    web/*|scripts/web-check.sh|scripts/e2e.sh) run_web=true ;;
    rust/rust-toolchain.toml) run_rust=true; tools_suite guards ;;
    rust/*|scripts/rust-check.sh|scripts/agent-boundary-check.sh|\
    scripts/check-agent-storage.sh|scripts/native-ocserv-integration.sh)
      run_rust=true ;;
    control-plane/go.mod|go.work*)
      run_go=true; run_database=true; tools_suite guards ;;
    control-plane/cmd/*|control-plane/migrations/*|control-plane/internal/*|control-plane/go.*)
      run_go=true; run_database=true ;;
    signer/go.mod)
      run_go=true; tools_suite guards ;;
    control-plane/*|scripts/go-check.sh|signer/*.go|signer/go.sum)
      run_go=true ;;

    # Installer-only contracts (avoids running unrelated release contract suites).
    .gitignore|deploy/managed-node/install.sh|deploy/production/install.sh|\
    deploy/production/controller-bootstrap.sh|deploy/lib/install-*.sh|\
    deploy/lib/node-bootstrap.sh|deploy/lib/controller-runtime.sh|\
    deploy/production/transportd-relays.sh|deploy/production/systemd/agent-relays.sh|\
    deploy/production/systemd/ocservia-agent-relays.conf|\
    scripts/prepare-bootstrap-release-assets.sh|\
    scripts/package-agent.sh|scripts/verify-agent-package.sh|\
    scripts/test-installer-common.sh|scripts/test-managed-node-install.sh|scripts/test-node-bootstrap.py|\
    scripts/test-controller-install.sh|scripts/test-controller-bootstrap.sh|\
    scripts/test-production-auth-config.sh|scripts/test-relay-launchers.py|scripts/test-vpn-audit.py)
      run_installers=true ;;

    # Broader release/installer/packaging/deployment paths.
    .github/workflows/release*.yml|install.env.example|\
    scripts/*release*|scripts/*controller*|scripts/*agent-upgrade*|\
    scripts/build-agent-binaries.sh|scripts/install-agent.sh|scripts/upgrade-agent.sh|\
    scripts/rollback-agent.sh|scripts/uninstall-agent.sh|scripts/agent-package-smoke.sh|\
    scripts/package-native-agent.sh|scripts/verify-bootstrap-endpoint.sh|\
    deploy/package/*|deploy/systemd/*|deploy/bootstrap/*|deploy/managed-node/*|\
    deploy/production/*|deploy/prepare-transport-runtime.sh)
      tools_suite release; run_installers=true ;;

    # Documentation and non-code repository metadata.
    # Shared Claude Code settings never reach CI or images; docs-check enforces its audited keys.
    # Other non-Markdown .claude/ files stay unknown and fail closed.
    .claude/settings.json|.github/CODEOWNERS|.github/PULL_REQUEST_TEMPLATE.md|\
    .github/dependabot.yml|.gitattributes|scripts/docs-check.sh|\
    docs/*.md|docs/upstream/*|*.md|LICENSE|LICENSE.*)
      run_docs=true ;;

    *) fail_closed "unknown_path:${path}" ;;
  esac

  # Orthogonal checks independent of each file's primary behavior suite.
  case "${path}" in
    scripts/*.sh|scripts/*.py|deploy/*.sh|deploy/bootstrap/install-controller|deploy/bootstrap/install-node|deploy/database-e2e/systemctl)
      tools_suite source-style ;;
  esac
  case "${path}" in
    scripts/generate-controller-release-manifest.mjs|scripts/redact-release-upgrade-log.mjs|\
    scripts/release-business-browser.mjs|scripts/test-release-upgrade.mjs|\
    .github/actions/build-cache-credentials/index.js)
      run_web=true ;;
  esac
  case "${path}" in
    web/src/api/generated/*|rust/crates/contracts/src/generated/*) tools_suite contracts ;;
  esac

  # Mirrors the COPY sources of the 7 Dockerfiles built by build-release-controller.sh.
  case "${path}" in
    control-plane/*_test.go|signer/*_test.go|\
    web/e2e/*|web/test/*|web/playwright.config.ts|web/playwright.Dockerfile|\
    web/Dockerfile|web/.prettierignore|web/eslint.config.ts) ;;
    web/*|deploy/production/gateway.Dockerfile|deploy/production/Caddyfile)
      select_controller_images gateway ;;
    control-plane/*|THIRD_PARTY_NOTICES.md) select_controller_images control ;;
    signer/*|deploy/production/signer.Dockerfile) select_controller_images signer ;;
    rust/Cargo.toml|rust/Cargo.lock|rust/rust-toolchain.toml|rust/.cargo/*|rust/vendor/*|rust/crates/*|\
    rust/transportd.Dockerfile|deploy/production/transportd-relays.sh|deploy/prepare-transport-runtime.sh)
      select_controller_images transport ;;
    deploy/production/backup.Dockerfile|deploy/production/backup-entrypoint.sh|scripts/postgres-backup.sh)
      select_controller_images backup ;;
    deploy/production/edge.Dockerfile|deploy/production/integrated/nginx.conf.template|deploy/production/integrated/edge-entrypoint.sh)
      select_controller_images edge ;;
    deploy/production/relay.Dockerfile|scripts/build-relay.sh|\
    deploy/production/relay-entrypoint.sh|deploy/production/relay-healthcheck.sh|\
    deploy/production/relay.Cargo.lock) select_controller_images relay ;;
    deploy/production/*.Dockerfile|scripts/bootstrap.sh|scripts/env.sh|scripts/checksums.txt|toolchains.lock|\
    .dockerignore|scripts/build-release-controller.sh|scripts/buildx-cache.sh|\
    .github/actions/build-cache-credentials/*)
      select_controller_images "${controller_images[@]}" ;;
  esac

  # App/UI/E2E/build sources run the stubbed browser regressions (skipping pure unit tests and linters).
  case "${path}" in
    web/src/*|web/index.html|web/e2e/*|web/playwright.config.ts|web/test/run-auth-browser.mjs|\
    web/vite.config.ts|web/package.json|web/package-lock.json)
      run_web_browser=true ;;
  esac
}

valid_sha() { [[ "$1" =~ ^[0-9a-f]{40}$ ]] && git cat-file -e "$1^{commit}" 2>/dev/null; }

if [[ "${event}" == workflow_dispatch ]]; then
  reason=workflow_dispatch_basic_checks
  tools_suite guards; tools_suite release; tools_suite source-style; tools_suite contracts
  for flag in run_docs run_go run_rust run_web run_web_browser run_database run_installers; do printf -v "${flag}" true; done
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

{
  printf 'profile=%s\n' "${profile}"
  printf 'database_scope=%s\n' "${database_scope}"
  printf 'reason=%s\n' "${reason}"
  printf 'changed_count=%s\n' "${#changed[@]}"
  printf 'ci_suites=%s\n' "${ci_suites[*]}"
  printf 'controller_cache_images=%s\n' "${controller_cache_images[*]}"
  for flag in "${flags[@]}"; do printf '%s=%s\n' "${flag}" "${!flag}"; done
} >>"${output}"
