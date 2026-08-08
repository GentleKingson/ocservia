#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ARTIFACT_DIR="${ARTIFACT_DIR:-}"
if [[ -n "${ARTIFACT_DIR}" ]]; then
  mkdir -p "${ARTIFACT_DIR}"
fi

grep -Fq 'OFFLINE_RECOVERY_RETENTION_SECONDS: u64 = 300' \
  "${ROOT}/rust/crates/command-journal/src/lib.rs"
grep -Fq 'now+OFFLINE_RECOVERY_RETENTION_SECONDS' \
  "${ROOT}/rust/crates/agent/src/main.rs"
grep -Fq 'recovery for at most five minutes' "${ROOT}/docs/development/telemetry.md"
if grep -R -n -i -E --include='*.md' --include='*.go' --include='*.rs' --include='*.sh' \
  --exclude-dir=target --exclude-dir=node_modules \
  'Agent.{0,80}(offline|离线).{0,80}(24h|24 hours|24 小时|86400)|(24h|24 hours|24 小时|86400).{0,80}Agent.{0,80}(offline|离线|recover|恢复)' \
  "${ROOT}/docs" "${ROOT}/control-plane" "${ROOT}/rust/crates"; then
  echo 'legacy 24-hour Agent offline recovery semantics remain' >&2
  exit 1
fi

(cd "${ROOT}/rust" && cargo test --locked -p ocservia-command-journal \
  five_minute_offline_recovery_caps_legacy_telemetry_at_boundary)
(cd "${ROOT}/rust" && cargo test --locked -p ocservia-agent \
  five_minute_offline_recovery_rejects_expired_command_before_effect)

summary=$'boundary_seconds=300\nlegacy_day_expiry=bounded_by_observed_age\nbefore_boundary=offline_buffer_retained\nat_boundary=fresh_reconnect_telemetry_only\nexpired_command=effect_rejected\nreal_time_wait=false'
if [[ -n "${ARTIFACT_DIR}" ]]; then
  printf '%s\n' "${summary}" | tee "${ARTIFACT_DIR}/offline-recovery-summary.txt"
else
  printf '%s\n' "${summary}"
fi
