#!/usr/bin/env bash
# Repository-owned change-impact router for the required CI workflows.
set -euo pipefail

if (($# != 4)); then
  echo "usage: $0 <event-name> <base-sha> <head-sha> <github-output>" >&2
  exit 2
fi

event="$1"; base_sha="$2"; head_sha="$3"; output="$4"
flags=(run_go_standard run_go_race run_runtime_artifacts run_database run_local_slice run_stage_contracts run_production_relays run_credential_rotation run_web run_browser run_p1_smoke run_contracts_policy run_rust run_native run_license run_g6_smoke)
for flag in "${flags[@]}"; do printf -v "${flag}" false; done
full=false; category=impact_union; reason=recognized_impact_union; changed=()

set_all() {
  local flag
  full=true
  for flag in "${flags[@]}"; do printf -v "${flag}" true; done
}
fail_closed() { category=full; reason="$1"; set_all; }
enable_go() { run_go_standard=true; run_go_race=true; }

classify_path() {
  local path="$1"
  case "${path}" in
    rust/*/Cargo.toml|rust/*/Cargo.lock|control-plane/go.mod|control-plane/go.sum|tools/*/go.mod|tools/*/go.sum)
      run_license=true ;;
  esac
  case "${path}" in
    .github/workflows/ci.yml)
      fail_closed ci_authority_changed ;;
    toolchains.lock|.node-version|.nvmrc|.tool-versions|.dockerignore|scripts/checksums.txt|scripts/bootstrap.sh|scripts/env.sh|scripts/ci-preflight.sh)
      fail_closed global_toolchain_changed ;;
    .github/workflows/g6-harness-core.yml|.github/workflows/g6-harness-smoke.yml|.github/actions/g6-*/*)
      run_contracts_policy=true; run_g6_smoke=true ;;
    .github/workflows/g6-readiness.yml|.github/workflows/p1-capacity.yml|.github/workflows/real-e2e.yml|.github/workflows/release.yml|.github/workflows/rust-cache-provision.yml|.github/workflows/*)
      run_contracts_policy=true ;;
    docs/acceptance/g6-*.json|docs/acceptance/g6-*.yaml|docs/acceptance/README.md)
      run_contracts_policy=true; run_g6_smoke=true ;;
    docs/acceptance/*)
      run_contracts_policy=true ;;
    docs/upstream/*)
      run_contracts_policy=true; run_stage_contracts=true; run_database=true; run_runtime_artifacts=true ;;
    docs/development/telemetry.md)
      run_contracts_policy=true; run_stage_contracts=true ;;
    .github/ISSUE_TEMPLATE/*|.github/PULL_REQUEST_TEMPLATE*|.github/CODEOWNERS|README.md|SECURITY.md|CODE_OF_CONDUCT.md|CONTRIBUTING.md|AGENTS.md|docs/*.md|docs/**/*.md)
      run_contracts_policy=true ;;
    .github/dependabot.yml)
      run_contracts_policy=true; run_license=true ;;
    LICENSE|LICENSE.*|THIRD_PARTY_NOTICES.md)
      run_contracts_policy=true; run_license=true ;;

    web/test/*|web/eslint.config.*|web/tsconfig.json)
      run_web=true ;;
    web/src/api/generated/*)
      run_web=true; run_browser=true; run_contracts_policy=true ;;
    web/src/*|web/e2e/*|web/index.html|web/vite.config.*|web/playwright.config.*|web/Dockerfile|web/playwright.Dockerfile)
      run_web=true; run_browser=true ;;
    web/package.json|web/package-lock.json)
      run_web=true; run_browser=true; run_license=true ;;
    web/*)
      run_web=true ;;

    openapi/*)
      run_contracts_policy=true; run_stage_contracts=true; run_web=true; run_browser=true; enable_go ;;
    proto/*)
      run_contracts_policy=true; run_stage_contracts=true; run_rust=true; run_g6_smoke=true; enable_go ;;

    go.work|go.work.sum|control-plane/go.mod|control-plane/go.sum|tools/g6-harness/go.mod|tools/g6-harness/go.sum)
      enable_go; run_license=true ;;
    tools/g6-harness/*.go|tools/g6-harness/**/*.go)
      enable_go; run_g6_smoke=true ;;
    control-plane/migrations/*)
      run_database=true; run_runtime_artifacts=true; run_g6_smoke=true
      if [[ "${path}" == *.go ]]; then enable_go; fi ;;
    control-plane/gen/proto/*)
      enable_go; run_contracts_policy=true ;;
    control-plane/internal/operations/*|control-plane/internal/enrollment/*|control-plane/internal/localslice/*|control-plane/internal/telemetry/*|control-plane/internal/userstate/*|control-plane/internal/useroperations/*|control-plane/internal/configplan/*|control-plane/internal/certificates/*|control-plane/internal/approvals/*|control-plane/internal/audit/*|control-plane/internal/rbac/*|control-plane/internal/auth/*|control-plane/internal/privdattestation/*|control-plane/internal/api/*|control-plane/internal/coordination/*|control-plane/internal/connectionowner/*|control-plane/internal/ownersession/*|control-plane/internal/postgresinput/*)
      enable_go; run_database=true; run_runtime_artifacts=true
      case "${path}" in
        control-plane/internal/localslice/*|control-plane/internal/operations/*|control-plane/internal/eventstream/*|control-plane/internal/transportclient/*|control-plane/internal/trustserver/*|control-plane/internal/udssecurity/*) run_local_slice=true ;;
      esac
      case "${path}" in
        control-plane/internal/operations/*|control-plane/internal/eventstream/*|control-plane/internal/telemetry/*|control-plane/internal/connectionowner/*) run_p1_smoke=true ;;
      esac
      if [[ "${path}" == control-plane/internal/coordination/maintenance.go ]]; then run_g6_smoke=true; fi ;;
    control-plane/internal/eventstream/*|control-plane/internal/transportclient/*|control-plane/internal/trustserver/*|control-plane/internal/udssecurity/*|control-plane/cmd/ocserv-control/*)
      enable_go; run_local_slice=true; run_runtime_artifacts=true ;;
    control-plane/internal/platform/app/*|control-plane/internal/platform/config/*|control-plane/internal/coordination/maintenance.go|control-plane/Dockerfile)
      if [[ "${path}" == *.go ]]; then enable_go; fi
      if [[ "${path}" == control-plane/Dockerfile ]]; then run_contracts_policy=true; fi
      run_g6_smoke=true; run_production_relays=true ;;
    control-plane/*.go|control-plane/**/*.go)
      enable_go ;;
    control-plane/*)
      run_contracts_policy=true ;;

    rust/Cargo.toml|rust/Cargo.lock|rust/deny.toml|rust/rust-toolchain.toml)
      run_rust=true; run_license=true ;;
    rust/crates/transportd-stub/*|rust/crates/transportd-stub/**/*|rust/transportd-stub.Dockerfile)
      run_rust=true; run_local_slice=true; run_runtime_artifacts=true; run_p1_smoke=true ;;
    rust/crates/agent/*|rust/crates/agent/**/*|rust/crates/privd/*|rust/crates/privd/**/*|rust/crates/privd-attestation/*|rust/crates/privd-attestation/**/*|rust/crates/ocserv-adapter/*|rust/crates/ocserv-adapter/**/*|rust/crates/upgrader/*|rust/crates/upgrader/**/*)
      run_rust=true; run_native=true; run_g6_smoke=true ;;
    rust/crates/g6-probe/*|rust/crates/g6-probe/**/*|rust/crates/g6-tunnel/*|rust/crates/g6-tunnel/**/*|rust/g6-runtime.Dockerfile)
      run_rust=true; run_g6_smoke=true ;;
    rust/crates/contracts/src/generated/*)
      run_rust=true; run_g6_smoke=true; run_contracts_policy=true ;;
    rust/crates/transportd/*|rust/crates/transportd/**/*|rust/crates/contracts/*|rust/crates/contracts/**/*|rust/crates/agent-protocol/*|rust/crates/agent-protocol/**/*|rust/crates/agent-identity/*|rust/crates/agent-identity/**/*|rust/crates/command-authorization/*|rust/crates/command-authorization/**/*|rust/crates/command-journal/*|rust/crates/command-journal/**/*)
      run_rust=true; run_g6_smoke=true ;;
    rust/vendor/*)
      run_rust=true; run_license=true ;;
    rust/*)
      run_rust=true ;;

    deploy/g6-readiness/*|deploy/g6-readiness/**/*)
      run_g6_smoke=true ;;
    deploy/production/rotate-postgres-credentials.sh|deploy/production/postgres-init/*|deploy/production/backup-entrypoint.sh|deploy/production/backup.Dockerfile)
      run_production_relays=true; run_credential_rotation=true ;;
    deploy/production/controller.sh|deploy/production/controller-release-smoke.sh)
      run_production_relays=true; run_contracts_policy=true ;;
    deploy/production/*|deploy/production/**/*)
      run_production_relays=true ;;
    deploy/compose/*|deploy/compose/**/*)
      run_browser=true; run_p1_smoke=true ;;
    deploy/systemd/*|deploy/package/*|deploy/prepare-transport-runtime.sh)
      run_native=true; run_contracts_policy=true ;;
    deploy/real-e2e/*|deploy/real-e2e/**/*|deploy/g6-ha-pitr/*|deploy/g6-ha-pitr/**/*)
      run_contracts_policy=true ;;

    scripts/ci-relevance.sh)
      fail_closed ci_relevance_authority_changed ;;
    scripts/test-controller-compose-lifecycle.sh|scripts/test-controller-host-bootstrap.sh)
      run_production_relays=true ;;
    scripts/test-ci-relevance.sh|scripts/test-bootstrap-profiles.sh|scripts/test-ci-runtime-artifact.sh|scripts/ci-runtime-artifact.sh|scripts/test-toolchain-consistency.sh|scripts/test-controller-release-manifest.sh|scripts/test-controller-release-bundle.sh|scripts/verify-controller-release-bundle.sh|scripts/test-controller-release-smoke.sh|scripts/test-controller-lifecycle.sh|scripts/test-release-checksum-manifest.sh|scripts/release-checksum-manifest.sh|scripts/generate-controller-release-manifest.mjs)
      run_contracts_policy=true ;;
    scripts/g6-*|scripts/*g6*)
      run_contracts_policy=true; run_g6_smoke=true ;;
    scripts/database-integration.sh)
      run_database=true; run_runtime_artifacts=true; run_contracts_policy=true ;;
    scripts/local-slice-integration.sh)
      run_local_slice=true; run_runtime_artifacts=true; run_contracts_policy=true ;;
    scripts/real-e2e-*.sh|scripts/test-real-e2e-workflow.sh)
      run_contracts_policy=true ;;
    scripts/p1-*|scripts/test-p1-*)
      run_p1_smoke=true; run_contracts_policy=true ;;
    scripts/i13-native-*|scripts/package-native-*|scripts/install-agent.sh|scripts/uninstall-agent.sh|scripts/upgrade-agent.sh|scripts/rollback-agent.sh)
      run_native=true; run_contracts_policy=true ;;
    scripts/i14-*|scripts/i15-*|scripts/i16-*|scripts/i17-*|scripts/i19-*)
      run_stage_contracts=true; run_contracts_policy=true ;;
    scripts/i18-postgres-credential-rotation.sh)
      run_credential_rotation=true; run_contracts_policy=true ;;
    scripts/i18-*|scripts/postgres-backup.sh)
      run_production_relays=true; run_contracts_policy=true ;;
    scripts/generate.sh|scripts/generated-clean.sh|scripts/test-generated-clean.sh|scripts/check-breaking.sh|scripts/*contract*|scripts/*policy*|scripts/docs-check.sh|scripts/check-public-repository.sh|scripts/test-public-repository-policy.sh)
      run_contracts_policy=true ;;
    scripts/license-check.sh)
      run_license=true; run_contracts_policy=true ;;
    scripts/security-check.sh|scripts/g6-secret-scan.toml|scripts/test-g6-secret-scan-config*|.gitleaksignore)
      run_contracts_policy=true ;;
    scripts/go-check.sh)
      enable_go; run_contracts_policy=true ;;
    scripts/rust-check.sh|scripts/agent-boundary-check.sh|scripts/transport-boundary-check.sh)
      run_rust=true; run_contracts_policy=true ;;
    scripts/web-check.sh|scripts/e2e.sh)
      run_web=true; run_browser=true; run_contracts_policy=true ;;
    Makefile|.gitignore|.gitattributes)
      run_contracts_policy=true ;;
    *)
      fail_closed "unknown_path:${path}" ;;
  esac
}

valid_sha() { [[ "$1" =~ ^[0-9a-f]{40}$ ]] && git cat-file -e "$1^{commit}" 2>/dev/null; }

if [[ "${event}" == workflow_dispatch ]]; then
  fail_closed workflow_dispatch_full_validation
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

if [[ "${run_database}" == true || "${run_local_slice}" == true ]]; then run_runtime_artifacts=true; fi
run_backend=false
for flag in run_go_standard run_go_race run_runtime_artifacts run_database run_local_slice run_stage_contracts run_production_relays run_credential_rotation run_p1_smoke run_rust; do
  if [[ "${!flag}" == true ]]; then run_backend=true; fi
done
if [[ "${full}" != true && "${reason}" == recognized_impact_union ]]; then
  active=()
  for flag in "${flags[@]}"; do
    if [[ "${!flag}" == true ]]; then active+=("${flag#run_}"); fi
  done
  category="$(IFS=+; echo "${active[*]:-none}")"
fi

{
  printf 'category=%s\n' "${category}"
  printf 'reason=%s\n' "${reason}"
  printf 'changed_count=%s\n' "${#changed[@]}"
  printf 'full=%s\n' "${full}"
  printf 'run_backend=%s\n' "${run_backend}"
  for flag in "${flags[@]}"; do printf '%s=%s\n' "${flag}" "${!flag}"; done
} >>"${output}"
