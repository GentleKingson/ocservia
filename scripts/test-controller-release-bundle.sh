#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VERIFIER="${ROOT}/scripts/verify-controller-release-bundle.sh"

command -v openssl >/dev/null 2>&1 || {
  echo "Controller release bundle tests skipped: openssl is unavailable"
  exit 0
}

fixture="$(mktemp -d "${ROOT}/.ocservia-controller-release-test.XXXXXX")"
trap 'rm -rf -- "${fixture}"' EXIT
bundle="${fixture}/bundle"
mkdir -m 700 -- "${bundle}"

trusted_key="${fixture}/trusted.key"
trusted_public_key="${fixture}/trusted.pub.pem"
wrong_key="${fixture}/wrong.key"
wrong_public_key="${fixture}/wrong.pub.pem"
manifest="${bundle}/controller-release.json"

openssl genpkey -algorithm ED25519 -out "${trusted_key}" >/dev/null 2>&1
openssl pkey -in "${trusted_key}" -pubout -out "${trusted_public_key}" >/dev/null 2>&1
openssl genpkey -algorithm ED25519 -out "${wrong_key}" >/dev/null 2>&1
openssl pkey -in "${wrong_key}" -pubout -out "${wrong_public_key}" >/dev/null 2>&1
chmod 600 "${trusted_key}" "${wrong_key}"
chmod 644 "${trusted_public_key}" "${wrong_public_key}"

write_valid_bundle() {
  printf '%s\n' '{"release":"v0.2.0"}' >"${manifest}"
  (cd "${bundle}" && sha256sum controller-release.json >controller-release.json.sha256)
  (cd "${bundle}" && sha256sum controller-release.json >SHA256SUMS)
  openssl pkeyutl -sign -rawin -inkey "${trusted_key}" \
    -in "${bundle}/SHA256SUMS" -out "${bundle}/SHA256SUMS.sig"
  chmod 600 "${manifest}" "${bundle}/controller-release.json.sha256" \
    "${bundle}/SHA256SUMS" "${bundle}/SHA256SUMS.sig"
}

expect_failure() {
  local label="$1" expected="$2" key="${3:-${trusted_public_key}}" output
  output="${fixture}/${label}.log"
  if "${VERIFIER}" "${manifest}" "${key}" >"${output}" 2>&1; then
    echo "expected release bundle verification to fail: ${label}" >&2
    exit 1
  fi
  grep -Fq -- "${expected}" "${output}"
}

write_valid_bundle
"${VERIFIER}" "${manifest}" "${trusted_public_key}" >/dev/null

printf '%s\n' '{"release":"v0.2.0","tampered":true}' >"${manifest}"
expect_failure tampered-manifest 'does not match its SHA256SUMS entry'

write_valid_bundle
printf '%s\n' '0000000000000000000000000000000000000000000000000000000000000000  controller-release.json' \
  >"${bundle}/SHA256SUMS"
expect_failure tampered-sums 'SHA256SUMS.sig signature verification failed'

write_valid_bundle
openssl pkeyutl -sign -rawin -inkey "${wrong_key}" \
  -in "${bundle}/SHA256SUMS" -out "${bundle}/SHA256SUMS.sig"
expect_failure wrong-signature 'SHA256SUMS.sig signature verification failed'

write_valid_bundle
expect_failure wrong-trusted-key 'SHA256SUMS.sig signature verification failed' "${wrong_public_key}"

write_valid_bundle
cp -- "${trusted_public_key}" "${bundle}/release-signing.pub.pem"
expect_failure same-bundle-key 'trusted public key must be provisioned outside the release bundle' \
  "${bundle}/release-signing.pub.pem"

write_valid_bundle
ln -s "${trusted_public_key}" "${fixture}/symlink-key.pub.pem"
expect_failure symlink-key 'trusted public key must be a regular file and not a symlink' \
  "${fixture}/symlink-key.pub.pem"

write_valid_bundle
chmod 666 "${trusted_public_key}"
expect_failure unsafe-key-mode 'trusted public key must not be group/world writable'
chmod 644 "${trusted_public_key}"

write_valid_bundle
printf '%s\n' 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa  another.json' \
  >"${bundle}/SHA256SUMS"
openssl pkeyutl -sign -rawin -inkey "${trusted_key}" \
  -in "${bundle}/SHA256SUMS" -out "${bundle}/SHA256SUMS.sig"
expect_failure missing-manifest-entry 'SHA256SUMS must contain exactly one valid checksum entry'

write_valid_bundle
printf '%s\n%s\n' \
  "$(cat "${bundle}/SHA256SUMS")" \
  "$(cat "${bundle}/SHA256SUMS")" >"${bundle}/SHA256SUMS.duplicate"
mv -- "${bundle}/SHA256SUMS.duplicate" "${bundle}/SHA256SUMS"
openssl pkeyutl -sign -rawin -inkey "${trusted_key}" \
  -in "${bundle}/SHA256SUMS" -out "${bundle}/SHA256SUMS.sig"
expect_failure duplicate-manifest-entry 'SHA256SUMS must contain exactly one valid checksum entry'

write_valid_bundle
printf '%s\n' 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa  controller-release.json' \
  >"${bundle}/controller-release.json.sha256"
expect_failure independent-checksum 'does not match its independent .sha256 checksum'

write_valid_bundle
rm -- "${bundle}/controller-release.json.sha256"
expect_failure missing-manifest-checksum 'release manifest checksum must be a regular file and not a symlink'

write_valid_bundle
rm -- "${bundle}/SHA256SUMS"
expect_failure missing-sums 'SHA256SUMS must be a regular file and not a symlink'

write_valid_bundle
rm -- "${bundle}/SHA256SUMS.sig"
expect_failure missing-signature 'SHA256SUMS.sig must be a regular file and not a symlink'

echo "Controller release bundle tests passed"
