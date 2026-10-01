#!/usr/bin/env bash
# Freeze the repository secret-scan configuration, including historical fixtures: the default rule set stays
# fully active, only the public run-scoped relay-pre-fault idempotency key is
# exempted, and the behavioral proof that real credentials still fail closed
# runs wherever the pinned gitleaks binary exists (scripts/security-check.sh).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/env.sh
source "${ROOT}/scripts/env.sh"
CONFIG="${ROOT}/scripts/secret-scan.toml"
SECURITY_CHECK="${ROOT}/scripts/security-check.sh"

grep -qF 'useDefault = true' "${CONFIG}" || {
  echo "the repository scan config must extend the default rule set" >&2
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
grep -qF 'g6-journal-key-[0-9a-f]{32}' "${CONFIG}" || {
  echo "the G6 evidence scan allowlist must name the tagged public journal effect key" >&2
  exit 1
}
grep -qF 'targetRules = ["generic-api-key"]' "${CONFIG}" || {
  echo "the G6 journal exemption must target only the generic-api-key rule" >&2
  exit 1
}
grep -qF 'regexTarget = "line"' "${CONFIG}" || {
  echo "the G6 journal exemption must match the evidence record line, not a path" >&2
  exit 1
}
grep -qF 'idempotency_key":"g6-journal-key-[0-9a-f]{32}' "${CONFIG}" || {
  echo "the G6 journal exemption must bind the exact public key shape to its evidence field" >&2
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
  echo "path-based exemptions are forbidden in the repository scan config" >&2
  exit 1
fi
# Only the reviewed curl command-boundary correction may override a rule.
# No rule disabling, entropy changes, new rule allowlists, or report suppression.
python3 - "${CONFIG}" <<'PY'
import sys
import tomllib

with open(sys.argv[1], "rb") as source:
    config = tomllib.load(source)
assert config["extend"] == {"useDefault": True}
rules = config.get("rules", [])
assert len(rules) == 1 and rules[0]["id"] == "curl-auth-user"
assert set(rules[0]) == {"id", "regex"} and rules[0]["regex"]
PY

# The behavioral exemption proof needs the pinned detector, so it runs inside
# the security scan itself. A missing invocation would leave the allowlist
# unverified, so the wiring is asserted here where no binary is required.
grep -qF 'test-secret-scan-config-runtime.sh' "${SECURITY_CHECK}" || {
  echo "the repository security scan must run the historical scan-config runtime proof" >&2
  exit 1
}
grep -qF -- '--config "${ROOT}/scripts/secret-scan.toml"' "${SECURITY_CHECK}" || {
  echo "the repository security scan must load the pinned gitleaks configuration" >&2
  exit 1
}

echo "repository secret scan configuration tests passed"
