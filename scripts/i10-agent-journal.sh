#!/usr/bin/env bash
set -euo pipefail
ulimit -c 0

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/env.sh
source "${ROOT}/scripts/env.sh"

cd "${ROOT}/rust"
cargo test -p ocservia-command-journal --all-features
cargo test -p ocservia-agent --all-features \
  duplicate_delivery_ack_loss_and_restart_execute_once
cargo test -p ocservia-agent --all-features \
  every_crash_boundary_reconciles_before_safe_retry
cargo test -p ocservia-agent --all-features \
  process_abort_crash_matrix_recovers_exactly_once
cargo test -p ocservia-agent --all-features \
  command_validation_fails_before_side_effect
cargo test -p ocservia-agent --all-features \
  same_key_different_payload_is_rejected_without_second_effect
cargo test -p ocservia-agent --all-features \
  identity_conflicts_never_execute_a_second_effect
cargo test -p ocservia-agent --all-features \
  safe_retry_requires_explicit_effect_absence_reconciliation
cargo test -p ocservia-agent --all-features \
  semantic_payload_hash_matches_cross_language_golden_vectors
