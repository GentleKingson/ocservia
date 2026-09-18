#!/usr/bin/env bash

# Sourced by validation entrypoints; never installs host packages or changes PATH.
require_test_commands() {
  local command missing=0
  for command in "$@"; do
    if ! command -v "${command}" >/dev/null 2>&1; then
      echo "${0##*/}: required test command is missing: ${command} (see docs/development/testing.md)" >&2
      missing=1
    fi
  done
  return "${missing}"
}

require_go_race() (
  require_test_commands go mktemp rm || exit 1
  [[ "$(go env CGO_ENABLED)" == 1 ]] || {
    echo "${0##*/}: -race requires CGO_ENABLED=1 and a working C compiler" >&2
    exit 1
  }
  local compiler=() tmp
  read -r -a compiler <<<"$(go env CC)"
  require_test_commands "${compiler[0]}" || exit 1
  tmp="$(mktemp -d "${TMPDIR:-/tmp}/go-race-preflight-XXXXXX")"
  trap 'rm -rf "${tmp}"' EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  printf 'int main(void) { return 0; }\n' | "${compiler[@]}" -x c - -o "${tmp}/probe" || {
    echo "${0##*/}: -race requires a C compiler that can compile and link (CC)" >&2
    exit 1
  }
)

require_test_docker() {
  require_test_commands docker || return 1
  docker info >/dev/null 2>&1 || {
    echo "${0##*/}: database tests require access to a running Docker daemon" >&2
    return 1
  }
}
