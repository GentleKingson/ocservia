#!/usr/bin/env bash
# Freeze the G6 evidence secret-scan configuration: the default rule set stays
# fully active, only the public run-scoped relay-pre-fault idempotency key is
# exempted, and real credentials in the very same evidence files must still
# fail closed.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/env.sh
source "${ROOT}/scripts/env.sh"
CONFIG="${ROOT}/scripts/g6-secret-scan.toml"
WORKFLOW="${ROOT}/.github/workflows/g6-harness-core.yml"

command -v gitleaks >/dev/null || {
  echo "gitleaks is required to verify the G6 evidence scan configuration" >&2
  exit 1
}

grep -qF 'useDefault = true' "${CONFIG}" || {
  echo "the G6 evidence scan config must extend the default rule set" >&2
  exit 1
}
grep -qF 'g6-relay-pre-fault-[0-9]+-[0-9]+-fd-b' "${CONFIG}" || {
  echo "the G6 evidence scan allowlist must name the public relay-pre-fault key" >&2
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

fixture="$(mktemp -d)"
trap 'rm -rf "${fixture}"' EXIT
mkdir -p "${fixture}/outbox/relay-pre-fault"
printf '%s\n' '{"idempotency_key":"g6-relay-pre-fault-32613255503-1-fd-b"}' \
  >"${fixture}/outbox/relay-pre-fault/relay-a-command-enqueue.jsonl"
printf '%s\n' '{"idempotency_key": "g6-relay-pre-fault-32613255503-1-fd-b"}' \
  >"${fixture}/outbox/relay-pre-fault/relay-a-command-proof.json"

# The unconfigured detector must still reproduce the live generic-api-key
# finding on this exact fixture shape.
if (cd "${fixture}" && gitleaks dir --no-banner --redact --no-color .) >/dev/null 2>&1; then
  echo "the fixture no longer reproduces the generic-api-key finding" >&2
  exit 1
fi

# With the pinned configuration the public idempotency key passes.
gitleaks dir --no-banner --redact --no-color --config "${CONFIG}" "${fixture}" >/dev/null 2>&1 || {
  echo "the pinned configuration must exempt the public relay-pre-fault key" >&2
  exit 1
}

# A real credential inside the very same evidence files must still fail closed.
printf '%s\n' 'github_token = "ghp_0123456789abcdefghijklmnopqrstuvwxyzAB"' \
  >>"${fixture}/outbox/relay-pre-fault/relay-a-command-proof.json"
printf '%s\n' '-----BEGIN PRIVATE KEY-----' 'MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKgwggSkAgEAAOCAQ8AMIIBCgKCAQEA' '-----END PRIVATE KEY-----' \
  >"${fixture}/outbox/relay-pre-fault/embedded.key"
if gitleaks dir --no-banner --redact --no-color --config "${CONFIG}" "${fixture}" >/dev/null 2>&1; then
  echo "the pinned configuration must still detect real credentials" >&2
  exit 1
fi

echo "g6 secret scan configuration tests passed"
