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
      bash scripts/test-release-checksum-manifest.sh
      bash scripts/test-controller-release-manifest.sh
      bash scripts/test-controller-release-bundle.sh
      bash scripts/test-controller-release-smoke.sh
      bash scripts/test-stage0-installers.sh
      ;;
    g6)
      bash scripts/test-g6-workflow-contract.sh
      bash scripts/test-g6-evidence-pipeline.sh
      bash scripts/test-g6-formal-authority.sh
      bash scripts/test-g6-release-identity.sh
      bash scripts/test-g6-install-release.sh
      bash scripts/test-g6-checkpoint-secret-policy.sh
      bash scripts/test-g6-cache-credentials.sh
      bash scripts/test-g6-buildx-cache-fallback.sh
      bash scripts/test-g6-readiness-hang-guards.sh
      node scripts/test-g6-pipeline.mjs
      node scripts/test-g6-evidence-builder.mjs
      node scripts/test-g6-evidence-verifier.mjs
      test -z "$(gofmt -l tools/g6-harness)"
      (cd tools/g6-harness && go vet ./... && go test -count=1 ./...)
      ;;
    *) echo "unknown CI tool suite: ${suite}" >&2; exit 2 ;;
  esac
done
