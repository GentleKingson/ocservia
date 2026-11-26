#!/usr/bin/env bash
set -euo pipefail
export PYTHONDONTWRITEBYTECODE=1
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source "${ROOT}/scripts/env.sh"
cd "${ROOT}"
: "${CI_SUITES:?selected tool contracts are required}"

for suite in ${CI_SUITES}; do
  case "${suite}" in
    source-style|contracts) ;;
    *) echo "unknown CI tool suite: ${suite}" >&2; exit 2 ;;
  esac
done

has_contracts=false
base_sha=""
for suite in ${CI_SUITES}; do
  if [[ "${suite}" == contracts ]]; then
    has_contracts=true
    : "${CONTRACT_BASE_REF:?contract_base_ref is required for contract checks}"
    baseline="${CONTRACT_BASE_REF}"
    if ! git rev-parse --verify --end-of-options "${baseline}^{commit}" >/dev/null 2>&1; then
      git fetch --no-tags -- origin "${baseline}"
      baseline="$(git rev-parse --verify 'FETCH_HEAD^{commit}')"
    fi
    if [[ "${CONTRACT_EVENT:-}" == pull_request ]]; then
      head_sha="${CONTRACT_HEAD_SHA:?PR head is required}"
      if ! git rev-parse --verify --end-of-options "${head_sha}^{commit}" >/dev/null 2>&1; then
        git fetch --no-tags -- origin "${head_sha}"
      fi
      baseline="$(git merge-base "${baseline}" "${head_sha}")"
    fi
    base_sha="$(git rev-parse --verify --end-of-options "${baseline}^{commit}")"
  fi
done

for suite in ${CI_SUITES}; do
  case "${suite}" in
    source-style)
      bash scripts/lint.sh source-style
      ;;
    contracts)
      bash scripts/lint.sh contracts
      bash scripts/check-breaking.sh "${base_sha}"
      ;;
  esac
done

if [[ "${has_contracts}" == true ]]; then
  bash scripts/generated-clean.sh
  if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
    printf 'contract_base_sha=%s\n' "${base_sha}" >>"${GITHUB_OUTPUT}"
  fi
fi
