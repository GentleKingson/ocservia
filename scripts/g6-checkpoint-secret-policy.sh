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

# The cross-domain handoff checkpoints are closed contracts: the shared
# runtime bundle and the recipient-key bundle each admit exactly one payload
# set, so any extra or missing file fails closed even without a recognized
# credential marker. checkpoint-manifest.json is optional here because the
# composite action generates it after this scan.
if [[ -e "${root}/shared-runtime.cms" || -e "${root}/recipient-cert.pem" ]]; then
  if [[ -e "${root}/shared-runtime.cms" && -e "${root}/recipient-cert.pem" ]]; then
    reject "shared checkpoint mixes encrypted runtime and recipient certificate"
  fi
  if [[ -e "${root}/shared-runtime.cms" ]]; then
    required=(shared-runtime.cms envelope.json)
  else
    required=(recipient-cert.pem)
  fi
  allowed=" ${required[*]} checkpoint-manifest.json "
  while IFS= read -r -d '' entry; do
    [[ -d "${entry}" && ! -L "${entry}" ]] && continue
    relative="${entry#"${root}"/}"
    case "${allowed}" in
      *" ${relative} "*) ;;
      *) reject "unexpected shared checkpoint payload ${relative}" ;;
    esac
  done < <(find "${root}" -mindepth 1 -print0)
  for name in "${required[@]}"; do
    [[ -f "${root}/${name}" ]] || reject "shared checkpoint is missing ${name}"
  done
fi

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
