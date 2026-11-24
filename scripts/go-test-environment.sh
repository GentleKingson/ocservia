#!/usr/bin/env bash

# Sourced by validation entrypoints; never installs host packages or changes PATH.
require_test_commands() {
  local command missing=0
  for command in "$@"; do
    if ! command -v "${command}" >/dev/null 2>&1; then
      echo "${0##*/}: required test command is missing: ${command} (see docs/development/release-checks.md)" >&2
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

dir_has_safe_ancestry() {
  local current="$1" mode
  [[ -d "${current}" && ! -L "${current}" && "${current}" == /* ]] || return 1
  while :; do
    [[ -d "${current}" && ! -L "${current}" ]] || return 1
    mode="$(stat -c '%a' "${current}" 2>/dev/null || stat -f '%Lp' "${current}" 2>/dev/null)" || return 1
    [[ "${mode}" =~ ^[0-7]+$ ]] || return 1
    (((8#${mode} & 8#022) == 0)) || return 1
    [[ "${current}" == / ]] && return 0
    current="${current%/*}"
    [[ -n "${current}" ]] || current=/
  done
}

# Create a private 0700 temporary directory outside the repository checkout whose
# ancestor chain is not group/world writable (required by signer store, secret
# loaders, and Unix socket checks).
private_go_tmpdir() {
  local prefix="$1" repo_root="" candidate resolved dir
  if [[ -n "${ROOT:-}" && -d "${ROOT}" ]]; then
    repo_root="$(cd "${ROOT}" && pwd -P)"
  fi
  local -a candidates=()
  [[ -n "${TMPDIR:-}" ]] && candidates+=("${TMPDIR}")
  [[ -n "${XDG_RUNTIME_DIR:-}" ]] && candidates+=("${XDG_RUNTIME_DIR}")
  if [[ -n "${HOME:-}" ]]; then
    if [[ -d "${HOME}" && ! -e "${HOME}/.cache" ]]; then
      mkdir -m 0700 "${HOME}/.cache" 2>/dev/null || true
    fi
    candidates+=("${HOME}/.cache")
  fi
  for candidate in "${candidates[@]}"; do
    [[ -d "${candidate}" ]] || continue
    resolved="$(cd "${candidate}" 2>/dev/null && pwd -P)" || continue
    if [[ -n "${repo_root}" && ( "${resolved}" == "${repo_root}" || "${resolved}" == "${repo_root}"/* ) ]]; then
      continue
    fi
    dir_has_safe_ancestry "${resolved}" || continue
    dir="$(mktemp -d "${resolved}/${prefix}.XXXXXX" 2>/dev/null)" || continue
    chmod 0700 "${dir}"
    printf '%s\n' "${dir}"
    return 0
  done
  echo "${0##*/}: no private temporary directory with non-group/world-writable ancestry outside the repository; set TMPDIR to a private 0700 directory" >&2
  return 1
}
