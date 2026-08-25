#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ASSET_DIR="${ASSET_DIR:?ASSET_DIR is required}"
VERSION="${VERSION:?VERSION is required}"
AGENT_TRUSTED_KEY_SHA256="${AGENT_TRUSTED_KEY_SHA256:-}"
WRITE_SHA256SUMS="${WRITE_SHA256SUMS:-}"

if [[ ! "${VERSION}" =~ ^[0-9]+\.[0-9]+\.[0-9]+([+-][0-9A-Za-z.-]+)?$ ]]; then
  echo "VERSION must be SemVer" >&2
  exit 2
fi
if [[ -n "${AGENT_TRUSTED_KEY_SHA256}" && ! "${AGENT_TRUSTED_KEY_SHA256}" =~ ^[0-9a-f]{64}$ ]]; then
  echo "AGENT_TRUSTED_KEY_SHA256 must be 64 lowercase hexadecimal characters" >&2
  exit 2
fi
for tool in dpkg-deb file openssl rpm rpm2cpio cpio; do
  command -v "${tool}" >/dev/null 2>&1 || {
    echo "required tool is missing: ${tool}" >&2
    exit 1
  }
done
ASSET_DIR="$(cd -- "${ASSET_DIR}" && pwd)"

tar_amd64="ocservia-agent-${VERSION}-linux-amd64.tar.gz"
tar_arm64="ocservia-agent-${VERSION}-linux-arm64.tar.gz"
deb_amd64="ocservia-agent_${VERSION}_amd64.deb"
deb_arm64="ocservia-agent_${VERSION}_arm64.deb"
rpm_amd64="ocservia-agent-${VERSION}-1.x86_64.rpm"
rpm_arm64="ocservia-agent-${VERSION}-1.aarch64.rpm"
package_files=(
  "${tar_amd64}" "${tar_arm64}" "${deb_amd64}" "${deb_arm64}" "${rpm_amd64}" "${rpm_arm64}"
  "${tar_amd64}.sha256" "${tar_amd64}.sha256.sig" "${tar_amd64}.sha256.pub.pem"
  "${tar_arm64}.sha256" "${tar_arm64}.sha256.sig" "${tar_arm64}.sha256.pub.pem"
)

for file in "${package_files[@]}"; do
  if [[ ! -f "${ASSET_DIR}/${file}" || -L "${ASSET_DIR}/${file}" || ! -s "${ASSET_DIR}/${file}" ]]; then
    echo "release asset is missing or empty: ${file}" >&2
    exit 1
  fi
done

work="$(mktemp -d "${TMPDIR:-/tmp}/ocservia-asset-validation.XXXXXX")"
cleanup() { sudo rm -rf -- "${work}"; }
trap cleanup EXIT INT TERM

der_fingerprint_of() {
  openssl pkey -pubin -in "$1" -outform DER 2>/dev/null | sha256sum | awk '{print $1}'
}

verify_embedded_payload() {
  local package_arch="$1" archive="$2" pub="$3" fingerprint="$4"
  local deb rpm extract archive_member embedded_tar deb_arch rpm_arch_actual

  case "${package_arch}" in
    amd64) deb="${deb_amd64}" rpm="${rpm_amd64}" ;;
    arm64) deb="${deb_arm64}" rpm="${rpm_arm64}" ;;
  esac
  extract="${work}/extract-${package_arch}"
  archive_member="usr/share/ocservia-agent/${archive}"

  deb_arch="$(dpkg-deb -f "${ASSET_DIR}/${deb}" Architecture)"
  [[ "${deb_arch}" == "${package_arch}" ]] \
    || { echo "${deb} declares Architecture ${deb_arch}, expected ${package_arch}" >&2; exit 1; }
  install -d -- "${extract}/deb"
  dpkg-deb -x "${ASSET_DIR}/${deb}" "${extract}/deb"
  embedded_tar="${extract}/deb/${archive_member}"
  [[ "$(sha256sum -- "${embedded_tar}" | awk '{print $1}')" == "$(sha256sum -- "${ASSET_DIR}/${archive}" | awk '{print $1}')" ]] \
    || { echo "${deb} does not embed the published ${archive} bytes" >&2; exit 1; }
  cmp -s -- "${extract}/deb/usr/share/ocservia-agent/release-signing.pub.pem" "${pub}" \
    || { echo "${deb} embeds a different signing public key" >&2; exit 1; }
  [[ "$(cat -- "${extract}/deb/usr/share/ocservia-agent/trusted-release-key.sha256")" == "${fingerprint}" ]] \
    || { echo "${deb} embeds a different trusted key fingerprint" >&2; exit 1; }
  cmp -s -- "${extract}/deb/usr/share/ocservia-agent/verify-agent-package.sh" \
    "${ROOT}/scripts/verify-agent-package.sh" \
    || { echo "${deb} embeds a different verifier script" >&2; exit 1; }

  rpm_arch_actual="$(rpm -qp --qf '%{ARCH}' --nosignature "${ASSET_DIR}/${rpm}" 2>/dev/null)"
  case "${package_arch}:${rpm_arch_actual}" in
    amd64:x86_64 | arm64:aarch64) ;;
    *)
      echo "${rpm} declares architecture ${rpm_arch_actual}, expected the ${package_arch} mapping" >&2
      exit 1
      ;;
  esac
  mkdir -p -- "${extract}/rpm"
  (cd "${extract}/rpm" && rpm2cpio "${ASSET_DIR}/${rpm}" | cpio -idm --quiet)
  embedded_tar="${extract}/rpm/${archive_member}"
  [[ -f "${embedded_tar}" ]] \
    || { echo "${rpm} does not embed ${archive}" >&2; exit 1; }
  [[ "$(sha256sum -- "${embedded_tar}" | awk '{print $1}')" == "$(sha256sum -- "${ASSET_DIR}/${archive}" | awk '{print $1}')" ]] \
    || { echo "${rpm} does not embed the published ${archive} bytes" >&2; exit 1; }
  cmp -s -- "${extract}/rpm/usr/share/ocservia-agent/release-signing.pub.pem" "${pub}" \
    || { echo "${rpm} embeds a different signing public key" >&2; exit 1; }
  [[ "$(cat -- "${extract}/rpm/usr/share/ocservia-agent/trusted-release-key.sha256")" == "${fingerprint}" ]] \
    || { echo "${rpm} embeds a different trusted key fingerprint" >&2; exit 1; }
  cmp -s -- "${extract}/rpm/usr/share/ocservia-agent/verify-agent-package.sh" \
    "${ROOT}/scripts/verify-agent-package.sh" \
    || { echo "${rpm} embeds a different verifier script" >&2; exit 1; }
}

verify_arch_triple() {
  local package_arch="$1" archive="$2" elf_word="$3"
  local pub="${ASSET_DIR}/${archive}.sha256.pub.pem"
  local fingerprint rootfs package_root file_output

  fingerprint="$(der_fingerprint_of "${pub}")"
  if [[ -n "${AGENT_TRUSTED_KEY_SHA256}" && "${fingerprint}" != "${AGENT_TRUSTED_KEY_SHA256}" ]]; then
    echo "${archive} was signed by a key other than the pinned release key" >&2
    exit 1
  fi
  rootfs="${work}/rootfs-${package_arch}"
  sudo install -d -o root -g root -m 0700 -- "${rootfs}"
  sudo install -d -o root -g root -m 0700 -- "${rootfs}/var/lib"
  package_root="$(sudo env DESTDIR="${rootfs}" AGENT_TRUSTED_KEY_SHA256="${fingerprint}" \
    "${ROOT}/scripts/verify-agent-package.sh" \
    "${ASSET_DIR}/${archive}" "${ASSET_DIR}/${archive}.sha256" \
    "${ASSET_DIR}/${archive}.sha256.sig" "${pub}")"
  sudo grep -Fxq "arch=${package_arch}" "${package_root}/MANIFEST" \
    || { echo "${archive} MANIFEST does not declare arch=${package_arch}" >&2; exit 1; }
  file_output="$(sudo file -b "${package_root}/rust/target/release/ocservia-agent")"
  [[ "${file_output}" == *"${elf_word}"* ]] \
    || { echo "${archive} carries a ${package_arch} package but ${file_output}" >&2; exit 1; }
  verify_embedded_payload "${package_arch}" "${archive}" "${pub}" "${fingerprint}"
}

verify_arch_triple amd64 "${tar_amd64}" "x86-64"
verify_arch_triple arm64 "${tar_arm64}" "aarch64"
echo "signed archive triples, package architectures, and embedded payloads validated"

mapfile -t ordered < <(printf '%s\n' "${package_files[@]:0:6}" | LC_ALL=C sort)
canonical_manifest="$(cd -- "${ASSET_DIR}" && sha256sum -- "${ordered[@]}")"
if [[ ! -f "${ASSET_DIR}/SHA256SUMS" ]]; then
  if [[ "${WRITE_SHA256SUMS}" == "1" ]]; then
    printf '%s\n' "${canonical_manifest}" >"${ASSET_DIR}/SHA256SUMS"
  else
    echo "SHA256SUMS is missing (set WRITE_SHA256SUMS=1 to generate it)" >&2
    exit 1
  fi
fi
[[ "$(cat -- "${ASSET_DIR}/SHA256SUMS")" == "${canonical_manifest}" ]] \
  || { echo "SHA256SUMS does not canonically cover exactly the six release packages" >&2; exit 1; }
if [[ -f "${ASSET_DIR}/SHA256SUMS.sig" ]]; then
  verified=false
  for pub in "${ASSET_DIR}/${tar_amd64}.sha256.pub.pem" "${ASSET_DIR}/${tar_arm64}.sha256.pub.pem"; do
    if openssl pkeyutl -verify -rawin -pubin -inkey "${pub}" \
      -sigfile "${ASSET_DIR}/SHA256SUMS.sig" -in "${ASSET_DIR}/SHA256SUMS" >/dev/null 2>&1; then
      verified=true
    fi
  done
  [[ "${verified}" == true ]] \
    || { echo "SHA256SUMS.sig does not verify against any published signing key" >&2; exit 1; }
  echo "SHA256SUMS signature verified"
fi
echo "release package set validation passed for version ${VERSION}"
