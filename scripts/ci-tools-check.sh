#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source "${ROOT}/scripts/env.sh"
cd "${ROOT}"
: "${CI_SUITES:?selected tool contracts are required}"
for suite in ${CI_SUITES}; do
  case "${suite}" in
    guards)
      bash scripts/test-ci-relevance.sh
      bash scripts/test-required-go-tests.sh
      bash scripts/test-bootstrap-profiles.sh
      ;;
    release)
      bash scripts/test-release-upgrade.sh
      bash scripts/test-controller-release-manifest.sh
      bash scripts/test-controller-release-smoke.sh
      bash scripts/test-release-image-security.sh
      bash scripts/test-stage0-installers.sh
      bash scripts/test-controller-quick-materials.sh
      bash scripts/test-build-cache-credentials.sh
      bash scripts/test-buildx-cache-fallback.sh
      bash scripts/test-secret-scan-config.sh
      ;;
    *) echo "unknown CI tool suite: ${suite}" >&2; exit 2 ;;
  esac
done
