#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CHECKSUM_MANIFEST="${ROOT}/scripts/release-checksum-manifest.sh"
fixture="$(mktemp -d)"
trap 'rm -rf -- "${fixture}"' EXIT

version=0.2.0
package_files=(
  "ocservia-agent-${version}-linux-amd64.tar.gz"
  "ocservia-agent-${version}-linux-arm64.tar.gz"
  "ocservia-agent_${version}_amd64.deb"
  "ocservia-agent_${version}_arm64.deb"
  "ocservia-agent-${version}-1.x86_64.rpm"
  "ocservia-agent-${version}-1.aarch64.rpm"
)
for file in "${package_files[@]}"; do
  printf 'release fixture\n' >"${fixture}/${file}"
done

printf '{"release":"v%s"}\n' "${version}" >"${fixture}/controller-release.json"
(cd "${fixture}" && sha256sum controller-release.json >controller-release.json.sha256)

canonical="$("${CHECKSUM_MANIFEST}" "${fixture}" 1 "${package_files[@]}")"
printf '%s\n' "${canonical}" >"${fixture}/SHA256SUMS"
openssl genpkey -algorithm ED25519 -out "${fixture}/release-signing.key" >/dev/null 2>&1
openssl pkeyutl -sign -rawin -inkey "${fixture}/release-signing.key" \
  -in "${fixture}/SHA256SUMS" -out "${fixture}/SHA256SUMS.sig"
openssl pkey -in "${fixture}/release-signing.key" -pubout \
  -out "${fixture}/release-signing.pub.pem" >/dev/null 2>&1
openssl pkeyutl -verify -rawin -pubin -inkey "${fixture}/release-signing.pub.pem" \
  -sigfile "${fixture}/SHA256SUMS.sig" -in "${fixture}/SHA256SUMS" >/dev/null

printf '{"release":"v%s","tampered":true}\n' "${version}" >"${fixture}/controller-release.json"
if "${CHECKSUM_MANIFEST}" "${fixture}" 1 "${package_files[@]}" >/dev/null 2>&1; then
  echo "tampered controller manifest must fail its sidecar checksum" >&2
  exit 1
fi

(cd "${fixture}" && sha256sum controller-release.json >controller-release.json.sha256)
tampered_canonical="$("${CHECKSUM_MANIFEST}" "${fixture}" 1 "${package_files[@]}")"
printf '%s\n' "${tampered_canonical}" >"${fixture}/tampered-SHA256SUMS"
if cmp -s "${fixture}/SHA256SUMS" "${fixture}/tampered-SHA256SUMS"; then
  echo "tampered controller manifest must change signed SHA256SUMS" >&2
  exit 1
fi
if openssl pkeyutl -verify -rawin -pubin -inkey "${fixture}/release-signing.pub.pem" \
  -sigfile "${fixture}/SHA256SUMS.sig" -in "${fixture}/tampered-SHA256SUMS" >/dev/null 2>&1; then
  echo "tampered controller manifest must fail the SHA256SUMS signature" >&2
  exit 1
fi

echo "Release checksum manifest tests passed"
