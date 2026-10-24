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

# The generator requires root-owned, non-writable ancestry, which /tmp (1777)
# and a sudo caller's HOME do not provide. /var/lib is also traversable, so the
# negative cases below are reachable by the Signer UID.
fixture="$(as_root mktemp -d /var/lib/ocservia-quick-materials-test.XXXXXX)"
as_root chmod 0755 -- "${fixture}"
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
  as_root env PATH="${generator_path:-${PATH}}" OCSERV_SECRET_DIR="${case_dir}/secrets" OCSERV_SIGNER_SECRET_DIR="${case_dir}/signer" \
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
for leftover in "${g}/.quick-ca" "${s}/.quick-ca" "${s}/.quick-stage"; do
  [[ ! -e "${leftover}" ]] || die "${leftover} must be removed"
done
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
# the prepared CA and removes the encrypted key from the host. The recovery
# source lives in the root-only secret directory, never in the Signer directory.
new_case interactive
if printf 'wrong\n' | generate --root-ca-passphrase-file "${case_dir}/passphrase" 2>/dev/null; then
  die "wrong fingerprint confirmation must fail"
fi
[[ ! -e "${case_dir}/signer/issuer-key.pem" && ! -e "${case_dir}/secrets/quick-install.json" ]] ||
  die "unconfirmed custody must not install the issuer"
[[ ! -e "${case_dir}/signer/.quick-ca" ]] || die "CA recovery source must not be in the Signer directory"
expect_stat "${case_dir}/secrets/.quick-ca" 0:0:700:2
as_root test -f "${case_dir}/secrets/.quick-ca/ready" || die "CA recovery source lost its ready marker"
fingerprint="$(as_root cut -d' ' -f1 "${case_dir}/export/root-ca.sha256")"
[[ "$(as_root openssl x509 -in "${case_dir}/secrets/.quick-ca/root-ca.crt" -noout -fingerprint -sha256 |
  sed 's/.*=//; s/://g' | tr 'A-F' 'a-f')" == "${fingerprint}" ]] || die "exported root differs from the prepared root"
printf '%s\n' "${fingerprint: -8}" | generate 2>/dev/null
[[ "$(as_root cut -d' ' -f1 "${case_dir}/export/root-ca.sha256")" == "${fingerprint}" ]] || die "root CA was regenerated"
[[ "$(as_root jq -r .root_ca_sha256 "${case_dir}/secrets/quick-install.json")" == "${fingerprint}" ]] ||
  die "the recovered CA identity changed"
expect_stat "${case_dir}/signer/issuer-key.pem" 65532:65532:400:1
expect_stat "${case_dir}/signer/issuer-chain.pem" 65532:65532:400:1
expect_stat "${case_dir}/export/root-ca.crt" 0:0:444:1
[[ ! -e "${case_dir}/export/root-ca.key.enc" && ! -e "${case_dir}/signer/.quick-ca" \
  && ! -e "${case_dir}/secrets/.quick-ca" && ! -e "${case_dir}/secrets/.quick-stage" ]] ||
  die "root key must be removed from the host"
if as_root grep -rlq 'ENCRYPTED PRIVATE KEY' "${case_dir}"; then die "root key remains on the host"; fi
as_root jq -e '.root_ca_custody == "removed-from-host"' "${case_dir}/secrets/quick-install.json" >/dev/null ||
  die "custody was not recorded"

# Publication is a pure rename of one staged file from the root-only staging
# directory. When it fails the prepared CA is kept and the rerun reuses it.
new_case publish
shim="${case_dir}/shim"
as_root install -d -m 0700 -- "${shim}"
real_rename="$(command -v rename.ul)"
# sudo drops the caller's environment, so the shim carries its paths literally.
as_root tee "${shim}/rename.ul" >/dev/null <<EOF
#!/usr/bin/env bash
printf '%s\n' "\$*" >>'${shim}/log'
[[ "\${3:-}" != */issuer-key.pem ]] || exit 1
exec '${real_rename}' "\$@"
EOF
as_root chmod 0700 -- "${shim}/rename.ul"
if PATH="${shim}:${PATH}" \
  generate --root-ca-passphrase-file "${case_dir}/passphrase" --non-interactive 2>/dev/null; then
  die "a failed publication must fail the run"
fi
expect_stat "${case_dir}/secrets/.quick-ca" 0:0:700:2
as_root test -f "${case_dir}/secrets/.quick-ca/ready" || die "failed publication discarded the CA recovery source"
[[ ! -e "${case_dir}/signer/issuer-key.pem" && ! -e "${case_dir}/secrets/quick-install.json" ]] ||
  die "failed publication must not complete the install"
while IFS= read -r line; do
  read -r -a argv <<<"${line}"
  [[ ${#argv[@]} == 4 && "${argv[0]}" == -- && "${argv[1]}" == "${argv[3]}" && "${argv[1]}" != "${argv[2]}" \
    && "${argv[1]}" == "${case_dir}/secrets/.quick-stage/"* && "${argv[2]}" == /* ]] ||
    die "unexpected rename.ul invocation: ${line}"
done < <(as_root cat "${shim}/log")
fingerprint="$(as_root cut -d' ' -f1 "${case_dir}/export/root-ca.sha256")"
generate --root-ca-passphrase-file "${case_dir}/passphrase" --non-interactive 2>/dev/null
[[ "$(as_root jq -r .root_ca_sha256 "${case_dir}/secrets/quick-install.json")" == "${fingerprint}" ]] ||
  die "the CA identity changed after a failed publication"
[[ "$(as_root cut -d' ' -f1 "${case_dir}/export/root-ca.sha256")" == "${fingerprint}" ]] || die "root CA was regenerated"
expect_stat "${case_dir}/signer/issuer-key.pem" 65532:65532:400:1

# Without rename.ul nothing is generated.
new_case norename
farm="${case_dir}/farm"
as_root install -d -m 0700 -- "${farm}"
for tool in bash env openssl jq sha256sum od; do as_root ln -s -- "$(command -v "${tool}")" "${farm}/${tool}"; done
if err="$(generator_path="${farm}" generate --root-ca-passphrase-file "${case_dir}/passphrase" --non-interactive 2>&1)"; then
  die "a missing rename.ul must fail"
fi
grep -Fq 'rename.ul is required' <<<"${err}" || die "missing rename.ul was not reported: ${err}"
[[ ! -e "${case_dir}/secrets" ]] || die "materials generated without rename.ul"

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

# A CA recovery directory left in the Signer-owned directory by an earlier
# version is preserved and never used. The outer directory is root-owned and
# traversable so UID 65532 really can reach the Signer directory.
new_case donor
if printf 'wrong\n' | generate --root-ca-passphrase-file "${case_dir}/passphrase" 2>/dev/null; then
  die "donor run must stop before custody confirmation"
fi
donor="${case_dir}/secrets/.quick-ca"

new_case legacy
as_root chmod 0755 -- "${case_dir}"
as_root install -d -o 65532 -g 65532 -m 0700 -- "${case_dir}/signer"
as_root setpriv --reuid=65532 --regid=65532 --clear-groups -- test -w "${case_dir}/signer" ||
  die "fixture does not let UID 65532 write the Signer directory"
as_root cp -a -- "${donor}" "${case_dir}/signer/.quick-ca"
as_root chown -R 65532:65532 -- "${case_dir}/signer/.quick-ca"
printf 'sentinel\n' | as_root tee "${case_dir}/signer/.quick-ca/sentinel" >/dev/null
before="$(tree_digest)"
if err="$(generate --root-ca-passphrase-file "${case_dir}/passphrase" --non-interactive 2>&1)"; then
  die "an untrusted CA recovery directory must be refused"
fi
grep -Fq 'cannot be trusted' <<<"${err}" || die "untrusted recovery directory was not reported: ${err}"
[[ -z "$(as_root ls -A "${case_dir}/secrets")" && ! -e "${case_dir}/export" && ! -e "${case_dir}/signer/issuer-key.pem" ]] ||
  die "material was copied or generated despite the untrusted recovery directory"
[[ "$(tree_digest)" == "${before}" ]] || die "the untrusted recovery directory was modified"
expect_stat "${case_dir}/signer/.quick-ca/ready" 65532:65532:600:1

new_case legacy-link
as_root chmod 0755 -- "${case_dir}"
as_root install -d -o 65532 -g 65532 -m 0700 -- "${case_dir}/signer"
as_root cp -a -- "${donor}" "${case_dir}/victim"
printf 'sentinel\n' | as_root tee "${case_dir}/victim/sentinel" >/dev/null
as_root ln -s -- "${case_dir}/victim" "${case_dir}/signer/.quick-ca"
before="$(tree_digest)"
if err="$(generate --root-ca-passphrase-file "${case_dir}/passphrase" --non-interactive 2>&1)"; then
  die "a symlinked CA recovery path must be refused"
fi
grep -Fq 'cannot be trusted' <<<"${err}" || die "symlinked recovery path was not reported: ${err}"
[[ -z "$(as_root ls -A "${case_dir}/secrets")" && ! -e "${case_dir}/export" && ! -e "${case_dir}/signer/issuer-key.pem" ]] ||
  die "material was copied or generated despite the symlinked recovery path"
as_root test -L "${case_dir}/signer/.quick-ca" || die "the legacy recovery symlink was removed"
[[ "$(tree_digest)" == "${before}" ]] || die "the symlink target was modified"

# Directories root writes into need a canonical, root-controlled ancestry.
new_case open-ancestor
as_root chmod 0777 -- "${case_dir}"
if err="$(generate --root-ca-passphrase-file "${case_dir}/passphrase" --non-interactive 2>&1)"; then
  die "a group/world-writable ancestor must be refused"
fi
grep -Fq 'not group/world writable' <<<"${err}" || die "writable ancestor was not reported: ${err}"
[[ ! -e "${case_dir}/secrets" ]] || die "materials created below a writable ancestor"

new_case link-ancestor
real="${case_dir}"
case_dir="${fixture}/link-ancestor-link"
as_root ln -s -- "${real}" "${case_dir}"
if err="$(generate --root-ca-passphrase-file "${real}/passphrase" --non-interactive 2>&1)"; then
  die "a symlinked ancestor must be refused"
fi
grep -Fq 'canonical' <<<"${err}" || die "symlinked ancestor was not reported: ${err}"
[[ ! -e "${real}/secrets" ]] || die "materials created below a symlinked ancestor"

echo "quick materials tests passed"
