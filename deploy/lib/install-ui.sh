#!/usr/bin/env bash
# Shared input collection only; all mutation stays in the existing installers.
install_ui_require_tty() {
  [[ -t 0 && -t 1 ]] || fail "interactive installation requires a TTY; use --non-interactive with install.env and required options"
}

install_ui_prompt() {
  local key="$1" label="$2" default="${3:-}" answer
  default="${!key:-${default}}"
  read -r -p "${label}${default:+ [${default}]}: " answer || fail "input ended; installation cancelled"
  printf -v "${key}" '%s' "${answer:-${default}}"
  export "${key?}"
}

install_ui_confirm() {
  local answer
  read -r -p 'Proceed with installation? [y/N]: ' answer || fail "input ended; installation cancelled"
  case "${answer}" in y|Y|yes|YES) ;; *) fail "installation cancelled" ;; esac
}

# Persist only the installer's public descriptors and protected file paths.
# Config files remain data: never source them as shell code.
install_ui_save() {
  local file="$1" key value temporary
  shift
  [[ ! -L "${file}" && ( ! -e "${file}" || -f "${file}" ) ]] || fail "configuration output must be a regular file"
  temporary="$(mktemp "${file}.XXXXXX")" || fail "cannot save configuration"
  chmod 600 "${temporary}"
  for key in "$@"; do
    [[ -n "${!key+x}" ]] || continue
    value="${!key}"
    if [[ "${value}" == *'$'* || "${value}" == *'`'* || "${value}" =~ [[:cntrl:]] ]]; then
      rm -f -- "${temporary}"
      fail "${key} must be a literal configuration value"
    fi
    printf '%s=%s\n' "${key}" "${value}" >>"${temporary}"
  done
  mv -f -- "${temporary}" "${file}"
  echo "saved configuration: ${file} (mode 0600)"
}
