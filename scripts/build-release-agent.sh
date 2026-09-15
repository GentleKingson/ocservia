#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/env.sh
source "${ROOT}/scripts/env.sh"
: "${VERSION:?}" "${PACKAGE_ARCH:?}" "${OUTPUT_DIR:?}" "${AGENT_SIGNING_KEY:?}" "${SOURCE_DATE_EPOCH:?}"
bash "${ROOT}/scripts/build-agent-binaries.sh"
bash "${ROOT}/scripts/package-agent.sh"
fingerprint="$(openssl pkey -in "${AGENT_SIGNING_KEY}" -pubout -outform DER | sha256sum | awk '{print $1}')"
AGENT_TRUSTED_KEY_SHA256="${fingerprint}" bash "${ROOT}/scripts/package-native-agent.sh"
