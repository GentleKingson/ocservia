#!/usr/bin/env bash
# Path routing for Basic CI.
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

flags=(run_docs run_go run_rust run_web run_database run_ci_tools run_controller_cache)
for flag in "${flags[@]}"; do printf -v "${flag}" false; done
reason=recognized_paths
changed=()
ci_suites=()
controller_images=(gateway control transport backup edge relay signer)
controller_cache_images=()

tools_suite() {
  # shellcheck disable=SC2034
  run_ci_tools=true
  case " ${ci_suites[*]:-} " in *" $1 "*) return 0 ;; esac
  ci_suites+=("$1")
}

select_controller_images() {
  # shellcheck disable=SC2034
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
  for flag in source-style contracts; do tools_suite "${flag}"; done
  select_controller_images "${controller_images[@]}"
}

# shellcheck disable=SC2034
classify_path() {
  local path="$1"
  case "${path}" in
    proto/*|openapi/*|control-plane/gen/*|scripts/bootstrap.sh|scripts/env.sh|scripts/checksums.txt|toolchains.lock|.github/actions/setup-tools/*)
      fail_closed shared_contract_or_toolchain_changed ;;
    control-plane/migrations/*|control-plane/internal/database/*|scripts/database-postgres-smoke.sh|scripts/postgres-test-common.sh|scripts/database-artifact-policy.sh)
      run_go=true; run_database=true ;;
    control-plane/*|signer/*|go.work*|scripts/go-check.sh)
      run_go=true ;;
    rust/*|scripts/rust-check.sh|scripts/agent-boundary-check.sh)
      run_rust=true ;;
    web/*|scripts/web-check.sh|scripts/e2e.sh)
      run_web=true ;;
    scripts/generate.sh|scripts/generated-clean.sh|scripts/check-breaking.sh|scripts/lint.sh)
      tools_suite source-style; tools_suite contracts ;;
    scripts/*|deploy/*|.github/*|Makefile|.tool-versions|.nvmrc|.node-version)
      tools_suite source-style ;;
    .claude/settings.json|*.md|docs/upstream/*|LICENSE|LICENSE.*)
      run_docs=true ;;
    testdata/*)
      run_go=true; run_rust=true ;;
    *)
      fail_closed "unknown_path:${path}" ;;
  esac

  case "${path}" in
    scripts/*.sh|scripts/*.py|deploy/*.sh|deploy/*.py) tools_suite source-style ;;
    scripts/redact-release-upgrade-log.mjs|.github/actions/build-cache-credentials/index.js) run_web=true ;;
    web/src/api/generated/*|rust/crates/contracts/src/generated/*) tools_suite contracts ;;
  esac

  case "${path}" in
    control-plane/*_test.go|signer/*_test.go|web/e2e/*|web/test/*|web/playwright.*) ;;
    web/*|deploy/production/gateway.Dockerfile|deploy/production/Caddyfile) select_controller_images gateway ;;
    control-plane/*|THIRD_PARTY_NOTICES.md) select_controller_images control ;;
    signer/*|deploy/production/signer.Dockerfile) select_controller_images signer ;;
    rust/*|deploy/production/transportd-relays.sh|deploy/prepare-transport-runtime.sh) select_controller_images transport ;;
    deploy/production/backup.Dockerfile|deploy/production/backup-entrypoint.sh|scripts/postgres-backup.sh) select_controller_images backup ;;
    deploy/production/edge.Dockerfile|deploy/production/integrated/*) select_controller_images edge ;;
    deploy/production/relay*|scripts/build-relay.sh) select_controller_images relay ;;
  esac
}

if [[ "${event}" == workflow_dispatch ]]; then
  reason=workflow_dispatch_basic_checks
  tools_suite source-style; tools_suite contracts
  for flag in run_docs run_go run_rust run_web run_database; do printf -v "${flag}" true; done
elif [[ "${event}" != pull_request && "${event}" != push ]]; then
  fail_closed "unsupported_event:${event}"
elif ! [[ "${base_sha}" =~ ^[0-9a-f]{40}$ && "${head_sha}" =~ ^[0-9a-f]{40}$ ]]; then
  fail_closed invalid_sha
elif [[ "${event}" == push && "${base_sha}" == 0000000000000000000000000000000000000000 ]]; then
  fail_closed all_zero_before_sha
else
  diff_range="${base_sha}..${head_sha}"
  [[ "${event}" != pull_request ]] || diff_range="${base_sha}...${head_sha}"
  while IFS= read -r -d '' path; do
    changed+=("${path}")
  done < <(git diff --name-only -z --no-renames "${diff_range}" 2>/dev/null || true)
  if ((${#changed[@]} == 0)); then
    fail_closed empty_diff_fail_closed
  else
    for path in "${changed[@]}"; do classify_path "${path}"; done
  fi
fi

profiles=()
suites=" ${ci_suites[*]:-} "
[[ "${suites}" != *" source-style "* ]] || profiles+=(source-style)
[[ "${suites}" != *" contracts "* ]] || profiles+=(contracts)
ci_tools_profiles="${profiles[*]:-}"
ci_tools_cache_label="${ci_tools_profiles// /-}"

{
  printf 'profile=%s\n' "${profile}"
  printf 'reason=%s\n' "${reason}"
  printf 'changed_count=%s\n' "${#changed[@]}"
  printf 'ci_suites=%s\n' "${ci_suites[*]}"
  printf 'ci_tools_profiles=%s\n' "${ci_tools_profiles}"
  printf 'ci_tools_cache_label=%s\n' "${ci_tools_cache_label}"
  printf 'controller_cache_images=%s\n' "${controller_cache_images[*]}"
  for flag in "${flags[@]}"; do printf '%s=%s\n' "${flag}" "${!flag}"; done
} >>"${output}"
