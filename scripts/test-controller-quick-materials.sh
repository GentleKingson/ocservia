#!/usr/bin/env bash
# Focused checks for deploy/production/quick-materials.sh. Requires root or
# passwordless sudo because the generator sets runtime UIDs.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GENERATOR="${ROOT}/deploy/production/quick-materials.sh"

if ((EUID == 0)); then
  as_root() { "$@"; }
elif sudo -n true >/dev/null 2>&1; then
  as_root() { sudo -n "$@"; }
else
  echo "quick materials tests skipped: root or passwordless sudo is required" >&2
  exit 0
fi

fixture="$(as_root mktemp -d)"
trap 'as_root rm -rf -- "${fixture}"' EXIT
die() { echo "quick materials tests: $*" >&2; exit 1; }

# Each case gets fresh directories; $case_dir is root-owned 0700.
new_case() {
  case_dir="${fixture}/$1"
  as_root install -d -m 0700 -- "${case_dir}"
  printf 'correct horse battery staple\n' | as_root tee "${case_dir}/passphrase" >/dev/null
  as_root chmod 0400 -- "${case_dir}/passphrase"
}

generate() {
  as_root env OCSERV_SECRET_DIR="${case_dir}/secrets" OCSERV_SIGNER_SECRET_DIR="${case_dir}/signer" \
    OCSERV_SIGNER_STATE_DIR="${case_dir}/signer-state" "${GENERATOR}" \
    --root-ca-export-dir "${case_dir}/export" "$@"
}

expect_stat() {
  local path="$1" expected="$2" actual
  actual="$(as_root stat -c '%u:%g:%a:%h' "${path}")"
  [[ "${actual}" == "${expected}" ]] || die "${path}: ${actual} != ${expected}"
}

tree_digest() { as_root find "${case_dir}" -type f -exec sha256sum {} + | sort; }

# Delegated custody: complete materials with the modes compose.sh requires.
new_case delegated
generate --root-ca-passphrase-file "${case_dir}/passphrase" --non-interactive 2>/dev/null
s="${case_dir}/secrets" g="${case_dir}/signer"
expect_stat "${s}" 0:0:700:2
for name in database-owner-url database-app-url session-key audit-checkpoint-key certificate-signer-token \
  postgres-owner-password postgres-app-password postgres-backup-password postgres.pgpass; do
  expect_stat "${s}/${name}" 0:0:444:1
done
expect_stat "${s}/audit-event-key" 65534:65532:400:1
expect_stat "${s}/controller-command-signing-key.pem" 65534:65532:400:1
expect_stat "${s}/controller-command-verification-key.pem" 0:65532:440:1
expect_stat "${s}/relay-access-token" 65532:65532:400:1
expect_stat "${s}/controller-iroh.key" 65532:65532:400:1
expect_stat "${g}" 65532:65532:700:2
expect_stat "${case_dir}/signer-state" 65532:65532:700:2
for name in issuer-chain.pem issuer-key.pem tls-cert.pem tls-key.pem api-token; do
  expect_stat "${g}/${name}" 65532:65532:400:1
done
expect_stat "${g}/tls-ca.pem" 65532:65532:444:1
[[ -z "$(as_root ls -A "${case_dir}/signer-state")" ]] || die "Signer state must stay empty"
[[ ! -e "${g}/.quick-ca" ]] || die "CA staging must be removed"
as_root cmp -s "${g}/api-token" "${s}/certificate-signer-token" || die "Signer token differs"
app_password="$(as_root cat "${s}/postgres-app-password")"
[[ "${app_password}" =~ ^[0-9a-f]{64}$ ]] || die "unexpected password format"
[[ "$(as_root cat "${s}/database-app-url")" == "postgres://ocservia_app:${app_password}@postgres:5432/ocservia?sslmode=disable" ]] ||
  die "unexpected application DSN"
[[ "$(as_root cat "${s}/postgres.pgpass")" == "postgres:5432:*:ocservia_backup:$(as_root cat "${s}/postgres-backup-password")" ]] ||
  die "unexpected pgpass"
[[ "$(as_root openssl pkey -in "${s}/controller-command-signing-key.pem" -pubout)" == "$(as_root cat "${s}/controller-command-verification-key.pem")" ]] ||
  die "verification key does not match the signing key"
[[ "$(as_root cat "${s}/controller-iroh.key")" =~ ^[0-9a-f]{64}$ ]] || die "invalid iroh key"

work="${fixture}/delegated-work"
as_root install -d -m 0700 -- "${work}"
for index in 1 2; do
  as_root awk -v want="${index}" '/BEGIN CERTIFICATE/{n++} n==want' "${g}/issuer-chain.pem" |
    as_root tee "${work}/cert-${index}.pem" >/dev/null
done
as_root mv -- "${work}/cert-1.pem" "${work}/issuer.pem"
as_root mv -- "${work}/cert-2.pem" "${work}/root.pem"
as_root openssl verify -CAfile "${work}/root.pem" "${work}/issuer.pem" >/dev/null || die "issuer chain does not verify"
issuer_text="$(as_root openssl x509 -in "${work}/issuer.pem" -noout -text)"
for expected in 'CA:TRUE, pathlen:0' 'Certificate Sign, CRL Sign' 'X509v3 Subject Key Identifier'; do
  grep -Fq "${expected}" <<<"${issuer_text}" || die "issuer lacks ${expected}"
done
[[ "$(as_root openssl pkey -in "${g}/issuer-key.pem" -pubout)" == "$(as_root openssl x509 -in "${work}/issuer.pem" -noout -pubkey)" ]] ||
  die "issuer key does not match the chain"
as_root openssl verify -CAfile "${g}/tls-ca.pem" -verify_hostname signer "${g}/tls-cert.pem" >/dev/null ||
  die "Signer TLS leaf does not verify for signer"
as_root grep -q 'BEGIN ENCRYPTED PRIVATE KEY' "${case_dir}/export/root-ca.key.enc" || die "root key must be encrypted"
as_root openssl pkey -in "${case_dir}/export/root-ca.key.enc" -passin file:"${case_dir}/passphrase" -noout ||
  die "root key does not decrypt with the passphrase"
as_root cmp -s "${case_dir}/export/root-ca.crt" "${work}/root.pem" || die "exported root differs from the chain root"
if as_root grep -rlq -- '-----BEGIN PRIVATE KEY-----' "${case_dir}/export"; then die "unencrypted key exported"; fi
record="$(as_root cat "${s}/quick-install.json")"
expect_stat "${s}/quick-install.json" 0:0:400:1
jq -e --arg root "$(as_root cut -d' ' -f1 "${case_dir}/export/root-ca.sha256")" \
  '.phase == "materials-complete" and .root_ca_custody == "delegated" and .root_ca_sha256 == $root
   and (.controller_endpoint_id | test("^[0-9a-f]{64}$")) and (.files | length) == 20' <<<"${record}" >/dev/null ||
  die "unexpected completion record"
if as_root grep -rqF -- "${app_password}" "${s}/quick-install.json"; then die "record contains a secret value"; fi

# A completed install is reused byte for byte and never regenerated.
before="$(tree_digest)"
generate --root-ca-passphrase-file "${case_dir}/passphrase" --non-interactive 2>/dev/null
[[ "$(tree_digest)" == "${before}" ]] || die "rerun changed materials"
as_root rm -- "${s}/database-app-url"
if generate --non-interactive 2>/dev/null; then die "missing material after completion must fail"; fi
[[ ! -e "${s}/database-app-url" ]] || die "material regenerated after completion"

# Existing material from an interrupted run is reused.
new_case reuse
as_root install -d -m 0700 -- "${case_dir}/secrets"
printf '%s\n' known-password | as_root tee "${case_dir}/secrets/postgres-app-password" >/dev/null
as_root chmod 0444 -- "${case_dir}/secrets/postgres-app-password"
generate --root-ca-passphrase-file "${case_dir}/passphrase" --non-interactive 2>/dev/null
as_root grep -Fq 'postgres://ocservia_app:known-password@' "${case_dir}/secrets/database-app-url" ||
  die "existing password was not reused"

# Interactive custody: a failed confirmation commits nothing; the rerun reuses
# the prepared CA and removes the encrypted key from the host.
new_case interactive
if printf 'wrong\n' | generate --root-ca-passphrase-file "${case_dir}/passphrase" 2>/dev/null; then
  die "wrong fingerprint confirmation must fail"
fi
[[ ! -e "${case_dir}/signer/issuer-key.pem" && ! -e "${case_dir}/secrets/quick-install.json" ]] ||
  die "unconfirmed custody must not install the issuer"
fingerprint="$(as_root cut -d' ' -f1 "${case_dir}/export/root-ca.sha256")"
printf '%s\n' "${fingerprint: -8}" | generate 2>/dev/null
[[ "$(as_root cut -d' ' -f1 "${case_dir}/export/root-ca.sha256")" == "${fingerprint}" ]] || die "root CA was regenerated"
[[ ! -e "${case_dir}/export/root-ca.key.enc" && ! -e "${case_dir}/signer/.quick-ca" ]] ||
  die "root key must be removed from the host"
if as_root grep -rlq 'ENCRYPTED PRIVATE KEY' "${case_dir}"; then die "root key remains on the host"; fi
as_root jq -e '.root_ca_custody == "removed-from-host"' "${case_dir}/secrets/quick-install.json" >/dev/null ||
  die "custody was not recorded"

# Fail-closed inputs.
new_case ledger
as_root install -d -o 65532 -g 65532 -m 0700 -- "${case_dir}/signer-state"
as_root touch "${case_dir}/signer-state/ledger.db"
if generate --root-ca-passphrase-file "${case_dir}/passphrase" --non-interactive 2>/dev/null; then
  die "an existing ledger without a record must fail"
fi
[[ ! -e "${case_dir}/secrets/session-key" ]] || die "materials generated next to an existing ledger"

new_case token
as_root install -d -o 65532 -g 65532 -m 0700 -- "${case_dir}/signer"
printf 'other-token\n' | as_root tee "${case_dir}/signer/api-token" >/dev/null
if generate --root-ca-passphrase-file "${case_dir}/passphrase" --non-interactive 2>/dev/null; then
  die "a mismatched Signer token must fail"
fi

new_case partial
as_root install -d -o 65532 -g 65532 -m 0700 -- "${case_dir}/signer"
printf 'key\n' | as_root tee "${case_dir}/signer/issuer-key.pem" >/dev/null
if generate --root-ca-passphrase-file "${case_dir}/passphrase" --non-interactive 2>/dev/null; then
  die "a lone issuer key must fail"
fi

new_case passphrase
as_root chmod 0644 -- "${case_dir}/passphrase"
if generate --root-ca-passphrase-file "${case_dir}/passphrase" --non-interactive 2>/dev/null; then
  die "a readable passphrase file must fail"
fi
if generate --non-interactive 2>/dev/null; then die "non-interactive mode requires a passphrase file"; fi
if printf 'first-passphrase\nother-passphrase\n' | generate 2>/dev/null; then die "mismatched passphrases must fail"; fi

echo "quick materials tests passed"
