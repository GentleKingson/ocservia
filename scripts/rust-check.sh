#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/env.sh
source "${ROOT}/scripts/env.sh"
(cd "${ROOT}/rust" && cargo fmt --all -- --check)
(cd "${ROOT}/rust" && cargo check --workspace --all-targets --all-features)
(cd "${ROOT}/rust" && cargo clippy --workspace --all-targets --all-features -- -D warnings)
(cd "${ROOT}/rust" && cargo test --workspace --all-features -- --test-threads=1)
rustfmt --edition 2024 --check \
  "${ROOT}/rust/vendor/iroh/src/endpoint.rs" \
  "${ROOT}/rust/vendor/iroh/src/socket.rs" \
  "${ROOT}/rust/vendor/iroh/src/socket/remote_map.rs" \
  "${ROOT}/rust/vendor/iroh/src/socket/remote_map/remote_state.rs" \
  "${ROOT}/rust/vendor/iroh/src/socket/transports.rs" \
  "${ROOT}/rust/vendor/iroh/src/socket/transports/relay.rs" \
  "${ROOT}/rust/vendor/iroh/src/socket/transports/relay/actor.rs"
(cd "${ROOT}/rust" && cargo doc --workspace --no-deps)
(cd "${ROOT}/rust" && cargo audit)
(cd "${ROOT}/rust" && cargo deny check)
