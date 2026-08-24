#!/usr/bin/env bash
# Conservative pull-request relevance classifier for the primary CI workflow.
#
# The classifier authorizes worker skipping for exactly two high-confidence
# change categories. Everything else — including unknown paths, mixed
# categories, workflow and deployment changes, acceptance contracts, and
# unresolvable diffs — classifies as full validation. Required-check
# aggregators independently re-check the authorization flags, so an
# unexpected skip can never pass silently.
set -euo pipefail

if (($# != 4)); then
  echo "usage: $0 <event-name> <base-sha> <head-sha> <github-output>" >&2
  exit 2
fi

event="$1"
base_sha="$2"
head_sha="$3"
output="$4"

[[ "${base_sha}" =~ ^[0-9a-f]{40}$ ]] || {
  echo "invalid base SHA" >&2
  exit 2
}
[[ "${head_sha}" =~ ^[0-9a-f]{40}$ ]] || {
  echo "invalid head SHA" >&2
  exit 2
}

# Worker authorization flags. run_database is a strict subset of run_backend
# because the database workers consume the commit-bound runtime artifact.
category=full
reason=full_validation
changed=()

if [[ "${event}" != "pull_request" ]]; then
  # Push and manual-dispatch runs never skip validation work.
  category=full_event_fallback
  reason=non_pull_request_full_fallback
else
  changed=()
  while IFS= read -r -d '' path; do
    changed+=("${path}")
  done < <(git diff --name-only -z --no-renames "${base_sha}" "${head_sha}")

  if ((${#changed[@]} == 0)); then
    # An empty or unresolvable diff must not authorize any skip.
    reason=empty_diff_fail_closed
  else
    docs_only=true
    web_only=true
    for path in "${changed[@]}"; do
      case "${path}" in
        # Acceptance contracts are executable verification inputs and stay
        # full validation; the check must precede the generic docs pattern.
        docs/acceptance/*)
          docs_only=false
          web_only=false
          break
          ;;
        # Documentation-only paths deliberately mirror the G6 smoke
        # relevance classifier.
        README.md|LICENSE|LICENSE.*|SECURITY.md|CODE_OF_CONDUCT.md|CONTRIBUTING.md|docs/*|.github/ISSUE_TEMPLATE/*|.github/PULL_REQUEST_TEMPLATE*)
          web_only=false
          ;;
        web/*)
          docs_only=false
          ;;
        *)
          docs_only=false
          web_only=false
          break
          ;;
      esac
    done
    if [[ "${docs_only}" == true ]]; then
      category=docs_only
      reason=documentation_only
    elif [[ "${web_only}" == true ]]; then
      category=web_only
      reason=web_source_only
    fi
  fi
fi

case "${category}" in
  docs_only)
    run_backend=false
    run_database=false
    run_rust=false
    run_native=false
    run_web=false
    run_browser=false
    run_p1_smoke=false
    ;;
  web_only)
    run_backend=false
    run_database=false
    run_rust=false
    run_native=false
    run_web=true
    run_browser=true
    run_p1_smoke=true
    ;;
  *)
    run_backend=true
    run_database=true
    run_rust=true
    run_native=true
    run_web=true
    run_browser=true
    run_p1_smoke=true
    ;;
esac

if [[ "${run_database}" == true && "${run_backend}" != true ]]; then
  echo "classifier invariant violated: database workers require the backend flag" >&2
  exit 1
fi

{
  printf 'category=%s\n' "${category}"
  printf 'reason=%s\n' "${reason}"
  printf 'changed_count=%s\n' "${#changed[@]}"
  printf 'run_backend=%s\n' "${run_backend}"
  printf 'run_database=%s\n' "${run_database}"
  printf 'run_rust=%s\n' "${run_rust}"
  printf 'run_native=%s\n' "${run_native}"
  printf 'run_web=%s\n' "${run_web}"
  printf 'run_browser=%s\n' "${run_browser}"
  printf 'run_p1_smoke=%s\n' "${run_p1_smoke}"
} >>"${output}"
