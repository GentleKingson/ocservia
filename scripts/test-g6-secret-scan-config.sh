#!/usr/bin/env bash
# Freeze the G6 evidence secret-scan configuration: the default rule set stays
# fully active, only the public run-scoped relay-pre-fault idempotency key is
# exempted, and the behavioral proof that real credentials still fail closed
# runs wherever the pinned gitleaks binary exists (scripts/security-check.sh).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/env.sh
source "${ROOT}/scripts/env.sh"
CONFIG="${ROOT}/scripts/g6-secret-scan.toml"
WORKFLOW="${ROOT}/.github/workflows/g6-harness-core.yml"
SECURITY_CHECK="${ROOT}/scripts/security-check.sh"

grep -qF 'useDefault = true' "${CONFIG}" || {
  echo "the G6 evidence scan config must extend the default rule set" >&2
  exit 1
}
grep -qF 'g6-relay-pre-fault-[0-9]+-[0-9]+-fd-b' "${CONFIG}" || {
  echo "the G6 evidence scan allowlist must name the public relay-pre-fault key" >&2
  exit 1
}
grep -qF 'g6-(?:load|relay-pre-fault|relay-failover|path-direct-recovery|crash[0-9]+|window)-[0-9]+-[0-9]+-fd-b(?:-[a-z0-9]+)*' "${CONFIG}" || {
  echo "the G6 evidence scan allowlist must name the enumerated public scenario command-key class" >&2
  exit 1
}
grep -qF 'g6-journal-key-[0-9a-f]+' "${CONFIG}" || {
  echo "the G6 evidence scan allowlist must name the tagged public journal effect key" >&2
  exit 1
}
grep -qF '01a02cfab3f17d5888eb7c20bf609ff2' "${CONFIG}" || {
  echo "the scan allowlist must name the synthetic bare fixture constant committed by history" >&2
  exit 1
}
grep -qF 'MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKgwggSkAgEAAOCAQ8AMIIBCgKCAQEA' "${CONFIG}" || {
  echo "the scan allowlist must name the public RSA-2048 SPKI header constant committed by history" >&2
  exit 1
}
if grep -qE '^[[:space:]]*paths[[:space:]]*=' "${CONFIG}"; then
  echo "path-based exemptions are forbidden in the G6 evidence scan config" >&2
  exit 1
fi
if grep -qE '^[[:space:]]*\[\[?rules\]?\]' "${CONFIG}"; then
  echo "the G6 evidence scan config must not replace or add detection rules" >&2
  exit 1
fi

total_scans="$(grep -c 'gitleaks dir' "${WORKFLOW}" || true)"
configured_scans="$(grep 'gitleaks dir' "${WORKFLOW}" | grep -cF -- '--config "${GITHUB_WORKSPACE}/scripts/g6-secret-scan.toml"' || true)"
total_scans="${total_scans:-0}"
configured_scans="${configured_scans:-0}"
[[ "${total_scans}" -eq 6 && "${configured_scans}" -eq 6 ]] || {
  echo "every published-evidence G6 scan must load the pinned gitleaks configuration" >&2
  exit 1
}

# The behavioral exemption proof needs the pinned detector, so it runs inside
# the security scan itself. A missing invocation would leave the allowlist
# unverified, so the wiring is asserted here where no binary is required.
grep -qF 'test-g6-secret-scan-config-runtime.sh' "${SECURITY_CHECK}" || {
  echo "the repository security scan must run the G6 scan-config runtime proof" >&2
  exit 1
}
grep -qF -- '--config "${ROOT}/scripts/g6-secret-scan.toml"' "${SECURITY_CHECK}" || {
  echo "the repository security scan must load the pinned gitleaks configuration" >&2
  exit 1
}

echo "g6 secret scan configuration tests passed"
