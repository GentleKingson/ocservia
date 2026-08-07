#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/env.sh
source "${ROOT}/scripts/env.sh"
(cd "${ROOT}/rust" && cargo fmt --all -- --check)
(cd "${ROOT}/rust" && cargo check --workspace --all-targets --all-features)
(cd "${ROOT}/rust" && cargo clippy --workspace --all-targets --all-features -- -D warnings)
(cd "${ROOT}/rust" && cargo test --workspace --all-features -- --test-threads=1)
(cd "${ROOT}/rust" && cargo doc --workspace --no-deps)
(cd "${ROOT}/rust" && cargo audit)
(cd "${ROOT}/rust" && cargo deny check)
