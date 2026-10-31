#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/env.sh
source "${ROOT}/scripts/env.sh"
(cd "${ROOT}/proto" && buf format -w)
(cd "${ROOT}/control-plane" && gofmt -w ./cmd ./gen ./internal ./migrations)
(cd "${ROOT}/signer" && gofmt -w .)
(cd "${ROOT}/rust" && cargo fmt --all)
(cd "${ROOT}/web" && npm run format)
(cd "${ROOT}" && web/node_modules/.bin/prettier --write \
  scripts/generate-controller-release-manifest.mjs scripts/redact-release-upgrade-log.mjs \
  scripts/release-business-browser.mjs scripts/release-upgrade-oidc-fixture.mjs \
  scripts/test-release-upgrade.mjs .github/actions/build-cache-credentials/index.js)
