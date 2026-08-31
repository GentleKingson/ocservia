#!/usr/bin/env bash
set -euo pipefail

if (($# != 2)); then
  echo "usage: $0 <release-manifest> <trusted-public-key>" >&2
  exit 2
fi

MANIFEST="$1"
TRUSTED_PUBLIC_KEY="$2"

fail() {
  echo "controller release bundle verification: $*" >&2
  exit 1
}

require_tool() {
  command -v "$1" >/dev/null 2>&1 || fail "required tool is missing: $1"
}

require_canonical_file() {
  local label="$1" path="$2" resolved

  [[ "${path}" == /* ]] || fail "${label} must be an absolute path"
  case "${path}" in
    /|*/|*/../*|*/..|*/./*|*/.) fail "${label} must be a canonical path without traversal" ;;
  esac
  [[ -f "${path}" && ! -L "${path}" ]] || fail "${label} must be a regular file and not a symlink"
  resolved="$(realpath -e -- "${path}")" || fail "${label} does not exist"
  [[ "${resolved}" == "${path}" ]] || fail "${label} must not contain symlink ancestry"
}

require_trusted_public_key() {
  local label="$1" path="$2" uid mode ancestor ancestor_uid ancestor_mode

  require_canonical_file "${label}" "${path}"

  IFS=: read -r uid mode < <(stat -c '%u:%a' "${path}")
  [[ "${uid}" == "0" || "${uid}" == "$(id -u)" ]] ||
    fail "${label} must be root- or launcher-owned"
  (( (8#${mode} & 8#022) == 0 )) ||
    fail "${label} must not be group/world writable"

  ancestor="$(dirname -- "${path}")"
  while true; do
    [[ "$(realpath -e -- "${ancestor}")" == "${ancestor}" ]] ||
      fail "${label} path ancestry must not contain symlinks"
    IFS=: read -r ancestor_uid ancestor_mode < <(stat -c '%u:%a' "${ancestor}")
    [[ "${ancestor_uid}" == "0" || "${ancestor_uid}" == "$(id -u)" ]] ||
      fail "${label} path ancestry must be root- or launcher-owned"
    (( (8#${ancestor_mode} & 8#022) == 0 )) ||
      fail "${label} path ancestry must not be group/world writable"
    [[ "${ancestor}" == "/" ]] && break
    ancestor="$(dirname -- "${ancestor}")"
  done
}

for tool in awk cat dirname grep id openssl realpath sha256sum stat; do
  require_tool "${tool}"
done

require_trusted_public_key "trusted public key" "${TRUSTED_PUBLIC_KEY}"
require_canonical_file "release manifest" "${MANIFEST}"

bundle_dir="$(dirname -- "${MANIFEST}")"
manifest_name="$(basename -- "${MANIFEST}")"
sums="${bundle_dir}/SHA256SUMS"
signature="${bundle_dir}/SHA256SUMS.sig"
manifest_checksum="${MANIFEST}.sha256"

case "${TRUSTED_PUBLIC_KEY}" in
  "${bundle_dir}"/*)
    fail "trusted public key must be provisioned outside the release bundle"
    ;;
esac

require_canonical_file "SHA256SUMS" "${sums}"
require_canonical_file "SHA256SUMS.sig" "${signature}"
require_canonical_file "release manifest checksum" "${manifest_checksum}"

key_description="$(openssl pkey -pubin -in "${TRUSTED_PUBLIC_KEY}" -text_pub -noout 2>/dev/null)" ||
  fail "trusted public key is not a readable public key"
grep -Fqx "ED25519 Public-Key:" <<<"${key_description}" ||
  fail "trusted public key must be an Ed25519 public key"

if ! openssl pkeyutl -verify -rawin -pubin -inkey "${TRUSTED_PUBLIC_KEY}" \
  -sigfile "${signature}" -in "${sums}" >/dev/null 2>&1; then
  fail "SHA256SUMS.sig signature verification failed"
fi

if ! sums_entry="$(awk -v name="${manifest_name}" '
  {
    if (NF != 2 || $1 !~ /^[0-9a-f]+$/ || length($1) != 64) {
      malformed = 1
    }
    if ($2 == name) {
      count++
      entry = $1
    }
  }
  END {
    if (malformed || count != 1) exit 1
    print entry
  }
' "${sums}")"; then
  fail "SHA256SUMS must contain exactly one valid checksum entry for ${manifest_name}"
fi

manifest_digest="$(sha256sum -- "${MANIFEST}" | awk '{print $1}')"
[[ "${manifest_digest}" == "${sums_entry}" ]] ||
  fail "release manifest does not match its SHA256SUMS entry"

expected_manifest_checksum="$(cd -- "${bundle_dir}" && sha256sum -- "${manifest_name}")"
actual_manifest_checksum="$(cat -- "${manifest_checksum}")"
[[ "${actual_manifest_checksum}" == "${expected_manifest_checksum}" ]] ||
  fail "release manifest does not match its independent .sha256 checksum"

echo "controller release bundle verification passed: ${manifest_name}"
