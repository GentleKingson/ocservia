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

mkdir -p "${fixture}/safe"
printf '%s\n' '-----BEGIN CERTIFICATE-----' 'public test certificate' '-----END CERTIFICATE-----' \
  >"${fixture}/safe/recipient-cert.pem"
printf '%s\n' '{"schema":"ocservia.g6.shared-runtime-envelope.v1"}' \
  >"${fixture}/safe/envelope.json"
printf 'ciphertext\000bytes' >"${fixture}/safe/shared-runtime.cms"
accepts "${fixture}/safe"

mkdir -p "${fixture}/crypto/secrets"
printf 'test-only-secret' >"${fixture}/crypto/secrets/runtime.txt"
openssl req -x509 -newkey rsa:2048 -nodes -sha256 -days 1 -subj /CN=g6-policy-test \
  -keyout "${fixture}/crypto/recipient-private.pem" \
  -out "${fixture}/crypto/recipient-cert.pem" >/dev/null 2>&1
tar -C "${fixture}/crypto" -czf "${fixture}/crypto/runtime.tar.gz" secrets
openssl cms -encrypt -binary -outform DER -aes256 \
  -in "${fixture}/crypto/runtime.tar.gz" \
  -out "${fixture}/safe/shared-runtime.cms" \
  "${fixture}/crypto/recipient-cert.pem"
openssl cms -decrypt -inform DER -in "${fixture}/safe/shared-runtime.cms" \
  -recip "${fixture}/crypto/recipient-cert.pem" \
  -inkey "${fixture}/crypto/recipient-private.pem" \
  -out "${fixture}/crypto/decrypted.tar.gz"
cmp "${fixture}/crypto/runtime.tar.gz" "${fixture}/crypto/decrypted.tar.gz"
accepts "${fixture}/safe"

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
ln -s "${fixture}/safe/recipient-cert.pem" "${fixture}/symlink/redirect"
rejects "${fixture}/symlink" "symlink payload"

echo "g6 checkpoint secret policy tests passed"
