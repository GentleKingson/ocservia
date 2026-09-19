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

fail_closed() {
  local flag
  reason="$1"
  for flag in run_docs run_go run_rust run_web run_database; do printf -v "${flag}" true; done
}

# Flags are read indirectly through ${!flag} when writing the outputs.
# shellcheck disable=SC2034
classify_path() {
  local path="$1"
  case "${path}" in
    docs/*|*.md|LICENSE|LICENSE.*) run_docs=true ;;
    proto/*|openapi/*|control-plane/gen/*) fail_closed shared_contract_changed ;;
    web/*|scripts/web-check.sh) run_web=true ;;
    rust/*|scripts/rust-check.sh) run_rust=true ;;
    deploy/managed-node/*|scripts/test-managed-node-install.sh|scripts/test-controller-install.sh|scripts/test-controller-bootstrap.sh|scripts/test-relay-launchers.py|scripts/test-release-agent-state-check.sh)
      run_rust=true; run_installers=true ;;
    control-plane/cmd/*|control-plane/migrations/*|control-plane/internal/*|control-plane/go.*|go.work*)
      run_go=true; run_database=true ;;
    control-plane/*|tools/g6-harness/*|scripts/go-check.sh) run_go=true ;;
    .github/workflows/ci.yml|scripts/ci-*|scripts/test-ci-*|scripts/*required-go-tests*|scripts/*bootstrap*|scripts/go-test-environment.sh|scripts/env.sh|scripts/checksums.txt|toolchains.lock)
      fail_closed ci_tools_changed; run_ci_tools=true ;;
    scripts/database-*.sh) run_go=true; run_database=true ;;
    .github/workflows/g6-*|.github/actions/g6-*/*|deploy/g6-*/*|deploy/real-e2e/*|scripts/*g6*|scripts/real-e2e-*|scripts/test-real-e2e-*|scripts/p1-*|scripts/test-p1-*|scripts/security-acceptance-*)
      run_docs=true ;;
    *) fail_closed "unknown_path:${path}" ;;
  esac
}

valid_sha() { [[ "$1" =~ ^[0-9a-f]{40}$ ]] && git cat-file -e "$1^{commit}" 2>/dev/null; }

if [[ "${event}" == workflow_dispatch ]]; then
  fail_closed workflow_dispatch_basic_checks
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
  for flag in "${flags[@]}"; do printf '%s=%s\n' "${flag}" "${!flag}"; done
} >>"${output}"
