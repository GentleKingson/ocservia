#!/usr/bin/env bash
# Behavioral proof for the G6 evidence secret-scan configuration. Runs from
# scripts/security-check.sh, where the pinned gitleaks binary is guaranteed:
# the exempted public idempotency key must pass with the configuration while
# the unconfigured detector still flags it, and real credentials planted in
# the very same evidence files must still fail closed. Every fixture value is
# generated at runtime or assembled from scanner-inert parts so this script
# itself stays secret-free.
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

# With the pinned configuration the public idempotency key passes, including
# the tagged journal effect key the evidence builder publishes. The bare
# fixture value is generated at runtime so this script never carries the
# flagged shape as a literal.
tagged_journal_half_a="0123456789abcdef"
tagged_journal_half_b="fedcba9876543210"
tagged_journal_key="g6-journal-key-${tagged_journal_half_a}${tagged_journal_half_b}"
printf '{"record_type":"effect","command_id":"01a02cfa-b3f1-7d58-88eb-7c20bf609ff2","idempotency_key":"%s"}\n' \
  "${tagged_journal_key}" \
  >"${fixture}/outbox/relay-pre-fault/command-trace.jsonl"
# The unconfigured detector must reproduce the live generic-api-key finding on
# the journal record itself, without relying on another allowlisted fixture.
if (cd "${fixture}/outbox/relay-pre-fault" \
  && gitleaks dir --no-banner --redact --no-color command-trace.jsonl) >/dev/null 2>&1; then
  echo "the journal fixture no longer reproduces the generic-api-key finding" >&2
  exit 1
fi
gitleaks dir --no-banner --redact --no-color --config "${CONFIG}" "${fixture}" >/dev/null 2>&1 || {
  echo "the pinned configuration must exempt the tagged public journal effect key" >&2
  exit 1
}

# An untagged bare hex idempotency key is exactly what the tag exists to
# distinguish, so it must still fail closed under the same configuration.
# The fixture value is fixed — every hex digit appears exactly twice, so its
# Shannon entropy is deterministically above the heuristic threshold, which
# a random value is not (about a quarter of random 32-hex strings fall
# below it) — and it is assembled from halves whose lines carry no
# keyword-adjacent literal the scanner could match.
bare_journal_half_a="0f1e2d3c4b5a6978"
bare_journal_half_b="8796a5b4c3d2e1f0"
printf '{"record_type":"effect","command_id":"01a02cfa-b3f1-7d58-88eb-7c20bf609ff2","idempotency_key":"%s"}\n' \
  "${bare_journal_half_a}${bare_journal_half_b}" \
  >"${fixture}/outbox/relay-pre-fault/bare-journal-key.jsonl"
if gitleaks dir --no-banner --redact --no-color --config "${CONFIG}" "${fixture}" >/dev/null 2>&1; then
  echo "the pinned configuration must still detect bare hex idempotency keys" >&2
  exit 1
fi
rm -f "${fixture}/outbox/relay-pre-fault/bare-journal-key.jsonl"

# The crash-scenario command keys are the same public run-scoped class as
# the relay key; the unconfigured detector flagged exactly this shape in a
# live run, so prove the failure first on an isolated fixture, then the
# exemption under the pinned configuration.
mkdir -p "${fixture}/outbox/crash-scenario"
printf '{"idempotency_key":"g6-crash1-%s-%s-fd-b"}\n' "${run_id}" "${attempt}" \
  >"${fixture}/outbox/crash-scenario/command.jsonl"
if (cd "${fixture}/outbox/crash-scenario" \
  && gitleaks dir --no-banner --redact --no-color .) >/dev/null 2>&1; then
  echo "the crash scenario key no longer reproduces the generic-api-key finding" >&2
  exit 1
fi
gitleaks dir --no-banner --redact --no-color --config "${CONFIG}" "${fixture}" >/dev/null 2>&1 || {
  echo "the pinned configuration must exempt the enumerated public scenario command keys" >&2
  exit 1
}

# Raw failure-domain inventories publish image digests under the public
# scanner-inert tag next to instance names whose service labels read as
# keywords. The tagged line passes, and a bare canonical digest beside the
# very same name must still fail closed.
image_digest_hex="0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
printf 'g6-rd-%s-fd-b-api-1\tpublic-image-digest-sha256-%s\n' \
  "${run_id}" "${image_digest_hex}" \
  >"${fixture}/outbox/relay-pre-fault/instances.tsv"
gitleaks dir --no-banner --redact --no-color --config "${CONFIG}" "${fixture}" >/dev/null 2>&1 || {
  echo "the pinned configuration must exempt the tagged public image digest" >&2
  exit 1
}
printf 'g6-rd-%s-fd-b-api-1\tsha256:%s\n' \
  "${run_id}" "${image_digest_hex}" \
  >"${fixture}/outbox/relay-pre-fault/bare-instances.tsv"
if gitleaks dir --no-banner --redact --no-color --config "${CONFIG}" "${fixture}" >/dev/null 2>&1; then
  echo "the pinned configuration must still detect bare image digests beside keyword names" >&2
  exit 1
fi
rm -f "${fixture}/outbox/relay-pre-fault/bare-instances.tsv"

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
