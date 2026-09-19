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
if [[ "${profile}" == full ]]; then database_scope=compatibility; fi
flags=(run_docs run_go run_rust run_web run_database run_ci_tools run_installers)
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
  tools_suite guards; tools_suite release; tools_suite g6
}

# Flags are read indirectly through ${!flag} when writing the outputs.
# shellcheck disable=SC2034
classify_path() {
  local path="$1"
  case "${path}" in
    # Executable/configuration contracts must precede documentation suffixes.
    docs/acceptance/g6-*.json|docs/acceptance/g6-slo.yaml|\
    .github/workflows/g6-*|.github/actions/g6-*/*|deploy/g6-*/*|\
    tools/g6-harness/*|scripts/*g6*|scripts/testdata/pre34-telemetry-runtime.patch)
      tools_suite g6 ;;
    proto/*|openapi/*|control-plane/gen/*) fail_closed shared_contract_changed ;;
    web/*|scripts/web-check.sh) run_web=true ;;
    rust/g6-runtime.Dockerfile) tools_suite g6 ;;
    rust/agent-build.Dockerfile|rust/transportd.Dockerfile)
      tools_suite release; tools_suite g6 ;;
    rust/*|scripts/rust-check.sh) run_rust=true ;;
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
    docs/*.md|*.md|LICENSE|LICENSE.*) run_docs=true ;;
    control-plane/cmd/*|control-plane/migrations/*|control-plane/internal/*|control-plane/go.*|go.work*)
      run_go=true; run_database=true ;;
    control-plane/*|scripts/go-check.sh) run_go=true ;;
    .github/workflows/ci.yml)
      tools_suite guards ;;
    .github/workflows/security.yml) tools_suite guards ;;
    scripts/ci-*|scripts/test-ci-*|scripts/*required-go-tests*|scripts/test-bootstrap-*|scripts/go-test-environment.sh)
      tools_suite guards ;;
    scripts/bootstrap.sh|scripts/env.sh|scripts/checksums.txt|toolchains.lock)
      fail_closed shared_toolchain_changed ;;
    deploy/real-e2e/*|scripts/real-e2e-*|scripts/test-real-e2e-*|scripts/p1-*|scripts/test-p1-*|scripts/security-acceptance-*)
      tools_suite g6 ;;
    *) fail_closed "unknown_path:${path}" ;;
  esac
}

valid_sha() { [[ "$1" =~ ^[0-9a-f]{40}$ ]] && git cat-file -e "$1^{commit}" 2>/dev/null; }

if [[ "${event}" == workflow_dispatch ]]; then
  reason=workflow_dispatch_basic_checks
  for flag in run_docs run_go run_rust run_web run_database; do printf -v "${flag}" true; done
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

matrix='{"include":[{"engine":"postgres","postgres":"17","version":"17.10"},{"engine":"mysql","version":"8.4.10"}]}'
if [[ "${profile}" == full ]]; then
  matrix='{"include":[{"engine":"postgres","postgres":"17","version":"17.10"},{"engine":"postgres","postgres":"18","version":"18.6"},{"engine":"mysql","version":"8.4.10"},{"engine":"mariadb","version":"12.3.2"}]}'
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
