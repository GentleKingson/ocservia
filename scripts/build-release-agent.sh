#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/env.sh
# shellcheck disable=SC1091
source "${ROOT}/scripts/env.sh"
: "${VERSION:?}" "${PACKAGE_ARCH:?}" "${OUTPUT_DIR:?}" "${SOURCE_DATE_EPOCH:?}"
bash "${ROOT}/scripts/build-agent-binaries.sh"
bash "${ROOT}/scripts/package-agent.sh"
bash "${ROOT}/scripts/package-native-agent.sh"
