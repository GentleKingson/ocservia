#!/usr/bin/env bash
# Strict, non-executing install.env loader shared by the Controller and
# managed-node installers (deploy/production/install.sh and
# deploy/managed-node/install.sh). Sourced, never executed directly.
#
# Contract:
# - The caller names its own allowlist; any other key in the file fails
#   closed, so a typo cannot silently drop a setting and internal test seams
#   never gain a production configuration entry.
# - Blank lines and lines whose first character is '#' are ignored; every
#   other line must be strict KEY=VALUE with KEY matching ^[A-Z][A-Z0-9_]*$.
# - Nothing in the file is ever executed, sourced, or expanded. Values
#   containing '$' or '`' are rejected outright — no supported value
#   (hostname, URL, path, fingerprint, environment tag) needs them, and the
#   rejection guarantees command substitution can never reach a shell even
#   if a future edit made this loader less careful.
# - A value wrapped in one static outer layer of matching single or double
#   quotes has exactly that layer removed: no expansion, no escape
#   interpretation. Empty values are valid.
# - Priority: explicit shell environment > install.env > installer
#   defaults. Before the file is read, every allowlisted variable already
#   set (non-empty) in the environment is recorded, and the loader only
#   fills and exports variables that were not explicitly set.
# - File safety: the file must be an existing regular file, must not be a
#   symlink, and must not be group- or world-writable. Root ownership is
#   deliberately not required: a launcher user must be able to maintain its
#   own configuration in a normal working directory.
# - Errors report the path, line, and reason only; file contents are never
#   printed.
# - A missing file is a silent no-op: without install.env the installers
#   behave exactly as before this loader existed.

install_env_die() {
  echo "install.env: $1" >&2
  exit 1
}

# install_env_load <file> <allowlisted-key>...
install_env_load() {
  local file="$1"
  shift
  local -a allowlist=("$@")
  local -a seen=()
  local -a preset=()
  local allowed key line value mode first last lineno=0 known

  if [[ -L "${file}" ]]; then
    install_env_die "refusing the configuration symlink ${file}; install.env must be a regular file"
  fi
  if [[ ! -e "${file}" ]]; then
    return 0
  fi
  if [[ ! -f "${file}" ]]; then
    install_env_die "${file} is not a regular file"
  fi
  if [[ ! -r "${file}" ]]; then
    install_env_die "${file} is not readable by the invoking user"
  fi
  mode="$(stat -c '%a' -- "${file}")" ||
    install_env_die "cannot inspect the permissions of ${file}"
  if (( (8#${mode} & 8#022) != 0 )); then
    install_env_die "refusing the group/world-writable configuration file ${file} (mode ${mode})"
  fi

  for allowed in "${allowlist[@]}"; do
    if [[ -n "${!allowed:-}" ]]; then
      preset+=("${allowed}")
    fi
  done

  while IFS= read -r line || [[ -n "${line}" ]]; do
    lineno=$((lineno + 1))
    if [[ -z "${line}" || "${line}" == \#* ]]; then
      continue
    fi
    if [[ ! "${line}" =~ ^([A-Z][A-Z0-9_]*)=(.*)$ ]]; then
      install_env_die "${file}:${lineno}: expected KEY=VALUE, a # comment, or a blank line"
    fi
    key="${BASH_REMATCH[1]}"
    value="${BASH_REMATCH[2]}"
    case "${value}" in
      *'$'* | *'`'*)
        install_env_die "${file}:${lineno}: ${key} must be a literal value; shell expansion syntax is never evaluated"
        ;;
    esac
    if [[ "${value}" =~ [[:cntrl:]] ]]; then
      install_env_die "${file}:${lineno}: ${key} contains a control character"
    fi
    if (( ${#value} >= 2 )); then
      first="${value:0:1}"
      last="${value:${#value}-1:1}"
      if [[ ( "${first}" == "'" && "${last}" == "'" ) || ( "${first}" == '"' && "${last}" == '"' ) ]]; then
        value="${value:1:${#value}-2}"
      fi
    fi
    known=false
    for allowed in "${allowlist[@]}"; do
      if [[ "${key}" == "${allowed}" ]]; then
        known=true
        break
      fi
    done
    if [[ "${known}" != true ]]; then
      install_env_die "${file}:${lineno}: unknown configuration variable ${key}"
    fi
    for allowed in ${seen[@]+"${seen[@]}"}; do
      if [[ "${key}" == "${allowed}" ]]; then
        install_env_die "${file}:${lineno}: duplicate configuration variable ${key}"
      fi
    done
    seen+=("${key}")
    for allowed in ${preset[@]+"${preset[@]}"}; do
      if [[ "${key}" == "${allowed}" ]]; then
        continue 2
      fi
    done
    printf -v "${key}" '%s' "${value}"
    # shellcheck disable=SC2163 # key is a validated allowlisted identifier
    export "${key}"
  done <"${file}"

  if (( ${#seen[@]} > 0 )); then
    echo "loaded configuration from ${file}"
  fi
}
