#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/env.sh
source "${ROOT}/scripts/env.sh"
# shellcheck source=scripts/go-test-environment.sh
source "${ROOT}/scripts/go-test-environment.sh"

GO_TEST_TMPDIR="$(private_go_tmpdir go-test-tmp)"
trap 'rm -rf -- "${GO_TEST_TMPDIR}"' EXIT
export TMPDIR="${GO_TEST_TMPDIR}"

(cd "${ROOT}/control-plane" && go test ./...)
(cd "${ROOT}/signer" && go test ./...)
(cd "${ROOT}/rust" && cargo test --workspace --all-features --lib --bins)
(cd "${ROOT}/web" && npm test)
