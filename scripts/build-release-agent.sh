#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/env.sh
source "${ROOT}/scripts/env.sh"
: "${VERSION:?}" "${PACKAGE_ARCH:?}" "${OUTPUT_DIR:?}" "${AGENT_SIGNING_KEY:?}" "${SOURCE_DATE_EPOCH:?}"
case "${PACKAGE_ARCH}:$(uname -m)" in amd64:x86_64|arm64:aarch64) ;; *) exit 2 ;; esac
(cd "${ROOT}/rust" && OCSERV_AGENT_RELEASE_VERSION="${VERSION}" cargo build --locked --release \
  --package ocservia-agent --package ocservia-privd --package ocservia-upgrader)
for binary in ocservia-agent ocservia-privd ocservia-upgrader; do
  file "${ROOT}/rust/target/release/${binary}"
  case "${PACKAGE_ARCH}" in amd64) machine='Advanced Micro Devices X86-64' ;; arm64) machine=AArch64 ;; esac
  readelf -h "${ROOT}/rust/target/release/${binary}" | grep -F "${machine}"
done
bash "${ROOT}/scripts/package-agent.sh"
fingerprint="$(openssl pkey -in "${AGENT_SIGNING_KEY}" -pubout -outform DER | sha256sum | awk '{print $1}')"
AGENT_TRUSTED_KEY_SHA256="${fingerprint}" bash "${ROOT}/scripts/package-native-agent.sh"
