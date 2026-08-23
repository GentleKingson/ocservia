#!/usr/bin/env bash
# Behavioral proof for the G6 evidence secret-scan configuration. Runs from
# scripts/security-check.sh, where the pinned gitleaks binary is guaranteed:
# the exempted public idempotency key must pass with the configuration while
# the unconfigured detector still flags it, and real credentials planted in
# the very same evidence files must still fail closed. Every fixture value is
# generated at runtime so this script itself stays secret-free.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/env.sh
source "${ROOT}/scripts/env.sh"
CONFIG="${ROOT}/scripts/g6-secret-scan.toml"

command -v gitleaks >/dev/null || {
  echo "gitleaks is required to verify the G6 evidence scan configuration" >&2
  exit 1
}
command -v openssl >/dev/null || {
  echo "openssl is required to generate the G6 scan-config private-key fixture" >&2
  exit 1
}

fixture="$(mktemp -d)"
trap 'rm -rf "${fixture}"' EXIT
mkdir -p "${fixture}/outbox/relay-pre-fault"
run_id="32613255503"
attempt="1"
relay_key="g6-relay-pre-fault-${run_id}-${attempt}-fd-b"
printf '{"idempotency_key":"%s"}\n' "${relay_key}" \
  >"${fixture}/outbox/relay-pre-fault/relay-a-command-enqueue.jsonl"
printf '{"idempotency_key": "%s"}\n' "${relay_key}" \
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
token_suffix="$(openssl rand -hex 18)"
printf 'github_token = "ghp_%s"\n' "${token_suffix}" \
  >>"${fixture}/outbox/relay-pre-fault/relay-a-command-proof.json"
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 \
  -out "${fixture}/outbox/relay-pre-fault/embedded.key" >/dev/null 2>&1
if gitleaks dir --no-banner --redact --no-color --config "${CONFIG}" "${fixture}" >/dev/null 2>&1; then
  echo "the pinned configuration must still detect real credentials" >&2
  exit 1
fi

echo "g6 secret scan configuration runtime tests passed"
