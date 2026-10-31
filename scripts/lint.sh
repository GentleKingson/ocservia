#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/env.sh
source "${ROOT}/scripts/env.sh"
scope="${1-all}"
if (($# > 1)); then
  echo "usage: $0 [all|common|source-style|contracts]" >&2
  exit 2
fi
case "${scope}" in
  all|common|source-style|contracts) ;;
  *) echo "usage: $0 [all|common|source-style|contracts]" >&2; exit 2 ;;
esac
cd "${ROOT}"
if [[ "${scope}" != contracts ]]; then
  shell_sources=()
  python_sources=()
  while IFS= read -r -d '' path; do
    [[ -f "${path}" ]] || continue
    IFS= read -r shebang <"${path}" || true
    if [[ "${shebang}" =~ ^\#!.*(/|[[:space:]])(ba)?sh([[:space:]]|$) ]]; then
      shell_sources+=("${path}")
    fi
    [[ "${path}" != *.py ]] || python_sources+=("${path}")
  done < <(git ls-files -z -- scripts deploy)
  shellcheck -x "${shell_sources[@]}"
  ruff check --select E4,E7,E9,F -- "${python_sources[@]}"
fi
if [[ "${scope}" != source-style ]]; then
  (cd "${ROOT}/proto" && buf format --diff --exit-code && buf lint)
  (cd "${ROOT}/web" && npx --no-install redocly lint \
    --config ../openapi/.redocly.yaml ../openapi/openapi.yaml)
fi
if [[ "${scope}" == all ]]; then
  (cd "${ROOT}/control-plane" && go vet ./...)
  (cd "${ROOT}/rust" && cargo clippy --workspace --all-targets --all-features -- -D warnings)
  (cd "${ROOT}/web" && npm run format:check && npm run lint && npm run typecheck)
fi
if [[ "${scope}" == all || "${scope}" == common ]]; then
  "${ROOT}/scripts/check-public-repository.sh"
  "${ROOT}/scripts/docs-check.sh"
fi
