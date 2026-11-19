#!/usr/bin/env bash
# Sourced by installer tests with a fixture-specific die function.

can_root() {
  (( EUID == 0 )) || sudo -n true >/dev/null 2>&1
}

as_root() {
  if (( EUID == 0 )); then
    "$@"
  else
    sudo -n "$@"
  fi
}

assert_status() {
  (( RUN_STATUS == "$1" )) || die "${2:-unexpected installer status}: expected $1, got ${RUN_STATUS}"
}

assert_output() {
  grep -q -- "$1" <<<"${RUN_OUTPUT}" || die "expected output to contain '$1'"
}

assert_log_contains() {
  grep -qF -- "$2" "$1" || die "expected $1 to contain '$2'"
}

assert_log_empty() {
  local log
  for log in "$@"; do
    [[ ! -s "${log}" ]] || die "expected ${log} to stay empty"
  done
}
