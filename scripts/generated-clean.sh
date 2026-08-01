#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
"${ROOT}/scripts/generate.sh"
git -C "${ROOT}" diff --exit-code -- \
  control-plane/gen/proto \
  rust/crates/contracts/src/generated \
  web/src/api/generated

untracked="$(git -C "${ROOT}" ls-files --others --exclude-standard -- \
  control-plane/gen/proto \
  rust/crates/contracts/src/generated \
  web/src/api/generated)"
if [[ -n "${untracked}" ]]; then
  echo "untracked generated files:" >&2
  printf '%s\n' "${untracked}" >&2
  exit 1
fi
