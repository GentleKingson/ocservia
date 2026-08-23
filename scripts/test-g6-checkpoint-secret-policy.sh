#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
POLICY="${ROOT}/scripts/g6-checkpoint-secret-policy.sh"
fixture="$(mktemp -d)"
trap 'rm -rf "${fixture}"' EXIT

rejects() {
  local path="${1:?fixture path is required}"
  if "${POLICY}" "${path}" >/dev/null 2>&1; then
    echo "checkpoint secret policy accepted forbidden fixture: ${2:?fixture name is required}" >&2
    exit 1
  fi
}

accepts() {
  local path="${1:?fixture path is required}"
  "${POLICY}" "${path}"
}

# The fixtures mirror the real cross-domain outbox layouts: fd-b publishes
# only its public certificate, fd-a publishes only the encrypted runtime and
# its envelope.
mkdir -p "${fixture}/shared" "${fixture}/recipient-key"
printf '%s\n' '-----BEGIN CERTIFICATE-----' 'public test certificate' '-----END CERTIFICATE-----' \
  >"${fixture}/recipient-key/recipient-cert.pem"
printf '%s\n' '{"schema":"ocservia.g6.shared-runtime-envelope.v1"}' \
  >"${fixture}/shared/envelope.json"
printf 'ciphertext\000bytes' >"${fixture}/shared/shared-runtime.cms"
accepts "${fixture}/shared"
accepts "${fixture}/recipient-key"

mkdir -p "${fixture}/crypto/secrets"
printf 'test-only-secret' >"${fixture}/crypto/secrets/runtime.txt"
openssl req -x509 -newkey rsa:2048 -nodes -sha256 -days 1 -subj /CN=g6-policy-test \
  -keyout "${fixture}/crypto/recipient-private.pem" \
  -out "${fixture}/crypto/recipient-cert.pem" >/dev/null 2>&1
tar -C "${fixture}/crypto" -czf "${fixture}/crypto/runtime.tar.gz" secrets
openssl cms -encrypt -binary -outform DER -aes256 \
  -in "${fixture}/crypto/runtime.tar.gz" \
  -out "${fixture}/shared/shared-runtime.cms" \
  "${fixture}/crypto/recipient-cert.pem"
openssl cms -decrypt -inform DER -in "${fixture}/shared/shared-runtime.cms" \
  -recip "${fixture}/crypto/recipient-cert.pem" \
  -inkey "${fixture}/crypto/recipient-private.pem" \
  -out "${fixture}/crypto/decrypted.tar.gz"
cmp "${fixture}/crypto/runtime.tar.gz" "${fixture}/crypto/decrypted.tar.gz"
accepts "${fixture}/shared"

# The composite action adds the typed manifest after this scan, so a fully
# assembled bundle with the manifest must also stay acceptable.
mkdir -p "${fixture}/assembled"
cp "${fixture}/shared/shared-runtime.cms" "${fixture}/shared/envelope.json" \
  "${fixture}/assembled/"
printf '%s\n' '{"schema":"ocservia.g6.checkpoint-manifest.v1"}' \
  >"${fixture}/assembled/checkpoint-manifest.json"
accepts "${fixture}/assembled"

mkdir -p "${fixture}/extra-shared-file"
cp "${fixture}/shared/shared-runtime.cms" "${fixture}/shared/envelope.json" \
  "${fixture}/extra-shared-file/"
printf 'opaque runtime notes' >"${fixture}/extra-shared-file/runtime-notes.txt"
rejects "${fixture}/extra-shared-file" "extra shared payload"

mkdir -p "${fixture}/missing-envelope"
cp "${fixture}/shared/shared-runtime.cms" "${fixture}/missing-envelope/"
rejects "${fixture}/missing-envelope" "shared bundle without envelope"

mkdir -p "${fixture}/extra-recipient-file"
cp "${fixture}/recipient-key/recipient-cert.pem" "${fixture}/extra-recipient-file/"
printf 'opaque enrollment notes' >"${fixture}/extra-recipient-file/enrollment.txt"
rejects "${fixture}/extra-recipient-file" "extra recipient payload"

mkdir -p "${fixture}/mixed-handoff"
cp "${fixture}/recipient-key/recipient-cert.pem" "${fixture}/shared/shared-runtime.cms" \
  "${fixture}/shared/envelope.json" "${fixture}/mixed-handoff/"
rejects "${fixture}/mixed-handoff" "mixed handoff bundle"

for name in owner-password dev-auth-token requester-session-cookie relay-leaf.key \
  command-signing.pem controller.key tunnel-pg-a.key; do
  mkdir -p "${fixture}/name-${name}"
  printf 'fixture' >"${fixture}/name-${name}/${name}"
  rejects "${fixture}/name-${name}" "${name}"
done

mkdir -p "${fixture}/private-pem"
printf '%s\n' '-----BEGIN PRIVATE KEY-----' 'fixture' '-----END PRIVATE KEY-----' \
  >"${fixture}/private-pem/opaque.pem"
rejects "${fixture}/private-pem" "private PEM"

mkdir -p "${fixture}/plaintext-token"
printf '%s\n' 'token=fixture-token' >"${fixture}/plaintext-token/metadata.txt"
rejects "${fixture}/plaintext-token" "plaintext token"

mkdir -p "${fixture}/symlink"
ln -s "${fixture}/recipient-key/recipient-cert.pem" "${fixture}/symlink/redirect"
rejects "${fixture}/symlink" "symlink payload"

echo "g6 checkpoint secret policy tests passed"
