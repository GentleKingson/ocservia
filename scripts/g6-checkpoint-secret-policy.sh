#!/usr/bin/env bash
# Reject plaintext credentials before a G6 cross-runner checkpoint is archived.
set -euo pipefail

root="${1:?checkpoint directory is required}"
[[ -d "${root}" ]] || {
  echo "checkpoint directory does not exist: ${root}" >&2
  exit 1
}

reject() {
  echo "checkpoint secret policy rejected ${1}" >&2
  exit 1
}

while IFS= read -r -d '' entry; do
  [[ -d "${entry}" && ! -L "${entry}" ]] && continue
  relative="${entry#"${root}"/}"
  name="${relative##*/}"
  [[ -f "${entry}" && ! -L "${entry}" ]] || reject "non-regular payload ${relative}"

  case "${name}" in
    *password*|*token*|*cookie*|*secret*|*.p12|*.pfx|*.key|controller.key|relay-leaf.key|command-signing.pem|tunnel-*.key)
      reject "sensitive filename ${relative}"
      ;;
  esac

  if LC_ALL=C grep -aEiq -e \
    '-----BEGIN ([A-Z0-9 ]* )?(ENCRYPTED |RSA |EC |ED25519 )?PRIVATE KEY-----|(^|[^[:alnum:]_])(password|token|cookie|secret)[[:space:]]*[:=]' \
    "${entry}"; then
    reject "plaintext credential marker in ${relative}"
  fi
done < <(find "${root}" -mindepth 1 -print0)
