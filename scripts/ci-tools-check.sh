#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source "${ROOT}/scripts/env.sh"
cd "${ROOT}"
: "${CI_SUITES:?selected tool contracts are required}"
for suite in ${CI_SUITES}; do
  case "${suite}" in
    guards)
      for script in scripts/ci-*.sh scripts/required-go-tests.sh scripts/bootstrap.sh; do
        bash -n "${script}"
      done
      bash scripts/test-ci-relevance.sh
      bash scripts/test-toolchain-consistency.sh
      bash scripts/check-public-repository.sh
      ;;
    release)
      node scripts/test-release-upgrade.mjs
      bash -n scripts/release-quick-acceptance.sh
      bash scripts/test-controller-release-manifest.sh
      bash scripts/test-release-image-security.sh
      bash scripts/test-release-package-security.sh
      bash scripts/test-controller-quick-materials.sh
      ;;
    cross-vm)
      make real-e2e-check
      ;;
    source-style)
      bash scripts/lint.sh source-style
      ;;
    contracts)
      : "${CONTRACT_BASE_REF:?contract_base_ref is required for contract checks}"
      baseline="${CONTRACT_BASE_REF}"
      if ! git rev-parse --verify --end-of-options "${baseline}^{commit}" >/dev/null 2>&1; then
        git fetch --no-tags -- origin "${baseline}"
        baseline="$(git rev-parse --verify 'FETCH_HEAD^{commit}')"
      fi
      if [[ "${CONTRACT_EVENT:-}" == pull_request ]]; then
        baseline="$(git merge-base "${baseline}" "${CONTRACT_HEAD_SHA:?PR head is required}")"
      fi
      bash scripts/lint.sh contracts
      bash scripts/check-breaking.sh "${baseline}"
      bash scripts/generated-clean.sh
      ;;
    *) echo "unknown CI tool suite: ${suite}" >&2; exit 2 ;;
  esac
done
