#!/usr/bin/env bash
set -euo pipefail

archive="${1:?archive required}"
checksum="${2:?checksum required}"
signature="${3:?signature required}"
public_key="${4:?public key required}"

for file in "${archive}" "${checksum}" "${signature}" "${public_key}"; do
  if [[ ! -f "${file}" || -L "${file}" ]]; then
    echo "package input must be a regular file: ${file}" >&2
    exit 1
  fi
done
expected_name="$(basename "${archive}")"
if [[ "$(awk 'NR == 1 { print $2 }' "${checksum}")" != "${expected_name}" ]] || [[ "$(wc -l <"${checksum}")" -ne 1 ]]; then
  echo "checksum manifest must name exactly the supplied archive" >&2
  exit 1
fi
openssl pkeyutl -verify -rawin -pubin -inkey "${public_key}" -sigfile "${signature}" -in "${checksum}" >/dev/null
(cd "$(dirname "${archive}")" && sha256sum --check "$(basename "${checksum}")") >/dev/null
if tar -tzf "${archive}" | awk -F/ '$1 == "" || $0 ~ /(^|\/)\.\.($|\/)/ { bad = 1 } END { exit bad ? 0 : 1 }'; then
  echo "package contains an unsafe path" >&2
  exit 1
fi
for required in MANIFEST scripts/install-agent.sh scripts/upgrade-agent.sh scripts/uninstall-agent.sh rust/target/release/ocservia-agent rust/target/release/ocservia-privd; do
  if ! tar -tzf "${archive}" | grep -Eq "^[^/]+/${required}$"; then
    echo "package is missing ${required}" >&2
    exit 1
  fi
done
echo "agent package signature and contents verified"
