#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/env.sh
source "${ROOT}/scripts/env.sh"

if (($# > 1)); then
  echo "usage: $0 [base-ref]" >&2
  exit 2
fi
base_ref="${1:-origin/main}"
if ! base_sha="$(git -C "${ROOT}" rev-parse --verify --end-of-options "${base_ref}^{commit}" 2>/dev/null)"; then
  echo "contract comparison baseline is not a commit: ${base_ref}" >&2
  exit 1
fi
if [[ "${base_sha}" == "$(git -C "${ROOT}" rev-parse HEAD)" ]]; then
  echo "contract comparison baseline must differ from the candidate" >&2
  exit 1
fi
temporary="$(mktemp -d)"
trap 'rm -rf "${temporary}"' EXIT INT TERM
if git -C "${ROOT}" cat-file -e "${base_sha}:proto/buf.yaml" 2>/dev/null; then
  mkdir -p "${temporary}/baseline"
  git -C "${ROOT}" archive "${base_sha}" proto | tar -x -C "${temporary}/baseline"
  (cd "${ROOT}" && buf breaking proto --against "${temporary}/baseline/proto")
fi

if git -C "${ROOT}" cat-file -e "${base_sha}:openapi/openapi.yaml" 2>/dev/null; then
  git -C "${ROOT}" show "${base_sha}:openapi/openapi.yaml" >"${temporary}/main.yaml"
  oasdiff breaking --fail-on ERR \
    "${temporary}/main.yaml" "${ROOT}/openapi/openapi.yaml"
fi
