#!/usr/bin/env bash
# Path routing for Basic CI only.
set -euo pipefail

if (($# != 4)); then
  echo "usage: $0 <event-name> <base-sha> <head-sha> <github-output>" >&2
  exit 2
fi

event="$1"; base_sha="$2"; head_sha="$3"; output="$4"
flags=(run_docs run_go run_rust run_web run_database)
for flag in "${flags[@]}"; do printf -v "${flag}" false; done
reason=recognized_paths
changed=()

fail_closed() {
  local flag
  reason="$1"
  for flag in "${flags[@]}"; do printf -v "${flag}" true; done
}

classify_path() {
  local path="$1"
  case "${path}" in
    docs/*|*.md|LICENSE|LICENSE.*)
      run_docs=true ;;
    .github/workflows/*|scripts/*|toolchains.lock|Makefile|.node-version|.nvmrc|.tool-versions)
      fail_closed infrastructure_changed ;;
    web/*)
      run_web=true; run_docs=true ;;
    control-plane/*|go.work|go.work.sum|*.go|*/go.mod|*/go.sum)
      run_go=true; run_database=true ;;
    rust/*)
      run_rust=true ;;
    *)
      fail_closed "unknown_path:${path}" ;;
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

{
  printf 'reason=%s\n' "${reason}"
  printf 'changed_count=%s\n' "${#changed[@]}"
  for flag in "${flags[@]}"; do printf '%s=%s\n' "${flag}" "${!flag}"; done
} >>"${output}"
