#!/usr/bin/env bash
set -euo pipefail

if (($# != 3)); then
  echo "usage: $0 <base-sha> <head-sha> <github-output>" >&2
  exit 2
fi

base_sha="$1"
head_sha="$2"
output="$3"

[[ "${base_sha}" =~ ^[0-9a-f]{40}$ ]] || {
  echo "invalid base SHA" >&2
  exit 2
}
[[ "${head_sha}" =~ ^[0-9a-f]{40}$ ]] || {
  echo "invalid head SHA" >&2
  exit 2
}

changed=()
while IFS= read -r -d '' path; do
  changed+=("${path}")
done < <(git diff --name-only -z "${base_sha}" "${head_sha}")
relevant=false
reason=documentation_only

if ((${#changed[@]} == 0)); then
  relevant=true
  reason=empty_diff_fail_closed
else
  for path in "${changed[@]}"; do
    case "${path}" in
      docs/acceptance/*)
        relevant=true
        reason=executable_or_contract_change
        break
        ;;
      README.md|LICENSE|LICENSE.*|SECURITY.md|CODE_OF_CONDUCT.md|CONTRIBUTING.md|docs/*|.github/ISSUE_TEMPLATE/*|.github/PULL_REQUEST_TEMPLATE*)
        ;;
      *)
        relevant=true
        reason=executable_or_contract_change
        break
        ;;
    esac
  done
fi

{
  printf 'relevant=%s\n' "${relevant}"
  printf 'reason=%s\n' "${reason}"
  printf 'changed_count=%s\n' "${#changed[@]}"
} >>"${output}"
