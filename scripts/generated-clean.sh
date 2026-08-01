#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
"${ROOT}/scripts/generate.sh"
git -C "${ROOT}" diff --exit-code -- \
  control-plane/gen/proto \
  rust/crates/contracts/src/generated \
  web/src/api/generated
