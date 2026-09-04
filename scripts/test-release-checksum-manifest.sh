#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CHECKSUM_MANIFEST="${ROOT}/scripts/release-checksum-manifest.sh"
PREPARE_BOOTSTRAPS="${ROOT}/scripts/prepare-bootstrap-release-assets.sh"
fixture="$(mktemp -d)"
trap 'rm -rf -- "${fixture}"' EXIT

prepared="${fixture}/prepared"
mkdir -p "${prepared}"
"${PREPARE_BOOTSTRAPS}" "${prepared}"
cmp -s "${prepared}/controller-bootstrap.sh" "${ROOT}/deploy/production/controller-bootstrap.sh"
cmp -s "${prepared}/managed-node-bootstrap.sh" "${ROOT}/deploy/managed-node/install.sh"
[[ -x "${prepared}/controller-bootstrap.sh" && -x "${prepared}/managed-node-bootstrap.sh" ]] || {
  echo "prepared bootstrap assets must be executable" >&2
  exit 1
}

version=0.2.0
package_files=(
  "ocservia-agent-${version}-linux-amd64.tar.gz"
  "ocservia-agent-${version}-linux-arm64.tar.gz"
  "ocservia-agent_${version}_amd64.deb"
  "ocservia-agent_${version}_arm64.deb"
  "ocservia-agent-${version}-1.x86_64.rpm"
  "ocservia-agent-${version}-1.aarch64.rpm"
)
bootstrap_files=(controller-bootstrap.sh managed-node-bootstrap.sh)
for file in "${package_files[@]}"; do
  printf 'release fixture\n' >"${fixture}/${file}"
done
for file in "${bootstrap_files[@]}"; do
  printf '#!/usr/bin/env bash\necho bootstrap fixture\n' >"${fixture}/${file}"
done

printf '{"release":"v%s"}\n' "${version}" >"${fixture}/controller-release.json"
printf '{"release":"v%s","platform":"amd64"}\n' "${version}" >"${fixture}/controller-release-amd64.json"
printf '{"release":"v%s","platform":"arm64"}\n' "${version}" >"${fixture}/controller-release-arm64.json"
(cd "${fixture}" && sha256sum controller-release.json >controller-release.json.sha256 \
  && sha256sum controller-release-amd64.json >controller-release-amd64.json.sha256 \
  && sha256sum controller-release-arm64.json >controller-release-arm64.json.sha256)

canonical="$("${CHECKSUM_MANIFEST}" "${fixture}" 1 "${package_files[@]}" "${bootstrap_files[@]}")"
printf '%s\n' "${canonical}" >"${fixture}/SHA256SUMS"
openssl genpkey -algorithm ED25519 -out "${fixture}/release-signing.key" >/dev/null 2>&1
openssl pkeyutl -sign -rawin -inkey "${fixture}/release-signing.key" \
  -in "${fixture}/SHA256SUMS" -out "${fixture}/SHA256SUMS.sig"
openssl pkey -in "${fixture}/release-signing.key" -pubout \
  -out "${fixture}/release-signing.pub.pem" >/dev/null 2>&1
openssl pkeyutl -verify -rawin -pubin -inkey "${fixture}/release-signing.pub.pem" \
  -sigfile "${fixture}/SHA256SUMS.sig" -in "${fixture}/SHA256SUMS" >/dev/null

mv "${fixture}/controller-bootstrap.sh" "${fixture}/controller-bootstrap.sh.missing"
if "${CHECKSUM_MANIFEST}" "${fixture}" 1 \
  "${package_files[@]}" "${bootstrap_files[@]}" >/dev/null 2>&1; then
  echo "a missing bootstrap asset must be rejected" >&2
  exit 1
fi
mv "${fixture}/controller-bootstrap.sh.missing" "${fixture}/controller-bootstrap.sh"

printf '{"release":"v%s","tampered":true}\n' "${version}" >"${fixture}/controller-release.json"
if "${CHECKSUM_MANIFEST}" "${fixture}" 1 \
  "${package_files[@]}" "${bootstrap_files[@]}" >/dev/null 2>&1; then
  echo "tampered controller manifest must fail its sidecar checksum" >&2
  exit 1
fi

(cd "${fixture}" && sha256sum controller-release.json >controller-release.json.sha256)
partial_dir="$(mktemp -d)"
for file in controller-release-amd64.json controller-release-arm64.json \
  controller-release-amd64.json.sha256 controller-release-arm64.json.sha256; do
  mv -- "${fixture}/${file}" "${partial_dir}/${file}"
done
if "${CHECKSUM_MANIFEST}" "${fixture}" 1 \
  "${package_files[@]}" "${bootstrap_files[@]}" >/dev/null 2>&1; then
  echo "a partial controller manifest set must be rejected" >&2
  exit 1
fi
for file in controller-release-amd64.json controller-release-arm64.json \
  controller-release-amd64.json.sha256 controller-release-arm64.json.sha256; do
  mv -- "${partial_dir}/${file}" "${fixture}/${file}"
done
rm -rf -- "${partial_dir}"

(cd "${fixture}" && sha256sum controller-release.json >controller-release.json.sha256)
tampered_canonical="$("${CHECKSUM_MANIFEST}" "${fixture}" 1 \
  "${package_files[@]}" "${bootstrap_files[@]}")"
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

printf '{"release":"v%s"}\n' "${version}" >"${fixture}/controller-release.json"
(cd "${fixture}" && sha256sum controller-release.json >controller-release.json.sha256)
restored_manifest="$("${CHECKSUM_MANIFEST}" "${fixture}" 1 \
  "${package_files[@]}" "${bootstrap_files[@]}")"
[[ "${restored_manifest}" == "${canonical}" ]] || {
  echo "restored release fixture did not reproduce canonical SHA256SUMS" >&2
  exit 1
}

printf '# tampered\n' >>"${fixture}/managed-node-bootstrap.sh"
tampered_bootstrap_manifest="$("${CHECKSUM_MANIFEST}" "${fixture}" 1 \
  "${package_files[@]}" "${bootstrap_files[@]}")"
if [[ "${tampered_bootstrap_manifest}" == "${canonical}" ]]; then
  echo "tampered bootstrap bytes must change SHA256SUMS" >&2
  exit 1
fi
if openssl pkeyutl -verify -rawin -pubin -inkey "${fixture}/release-signing.pub.pem" \
  -sigfile "${fixture}/SHA256SUMS.sig" \
  -in <(printf '%s\n' "${tampered_bootstrap_manifest}") >/dev/null 2>&1; then
  echo "tampered bootstrap bytes must fail the SHA256SUMS signature" >&2
  exit 1
fi

echo "Release checksum manifest tests passed"
