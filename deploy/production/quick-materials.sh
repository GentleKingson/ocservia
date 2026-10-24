#!/usr/bin/env bash
# Quick install material generator for the integrated, bundled PostgreSQL,
# root-lifecycle Controller. Run as root before the first activation.
#
# It creates every missing Controller and Signer secret with the exact
# ownership and modes compose.sh validates, plus an offline root CA and an
# online issuing intermediate for the Signer. The root CA private key is only
# ever written encrypted. Existing files are reused; a rerun after an
# interruption completes the missing ones. Once quick-install.json records
# completion, or the Signer ledger exists, nothing is generated again and any
# missing file fails closed.
#
# Everything root re-opens (staged files and the CA recovery source) lives in
# directories under the root-only secret directory, whose ancestors must be
# canonical and root-controlled. Files reach their target by a pure rename, with
# no copy fallback. A CA recovery directory left by an earlier version inside the
# Signer-owned directory is never trusted: it is preserved and the run refuses
# to continue until the operator deals with it.
#
# Relay and Gateway TLS identities are not generated here.
#
# Usage (environment: OCSERV_SECRET_DIR, OCSERV_SIGNER_SECRET_DIR,
# OCSERV_SIGNER_STATE_DIR):
#   quick-materials.sh [--root-ca-passphrase-file PATH]
#     [--root-ca-export-dir PATH] [--non-interactive]
set -euo pipefail
umask 077

fail() { echo "quick-materials: $*" >&2; exit 1; }
note() { echo "quick-materials: $*" >&2; }

passphrase_file=""
export_dir="/root/ocservia-root-ca-export"
interactive=true
while (($#)); do
  case "$1" in
    --root-ca-passphrase-file) (($# >= 2)) || fail "missing value for $1"; passphrase_file="$2"; shift 2 ;;
    --root-ca-export-dir) (($# >= 2)) || fail "missing value for $1"; export_dir="$2"; shift 2 ;;
    --non-interactive) interactive=false; shift ;;
    *) fail "unknown argument: $1" ;;
  esac
done

((EUID == 0)) || fail "must run as root"
for command in openssl jq sha256sum od rename.ul; do
  command -v "${command}" >/dev/null 2>&1 || fail "${command} is required (rename.ul: util-linux, or util-linux-extra on Ubuntu 26.04)"
done
secret_dir="${OCSERV_SECRET_DIR:-}"
signer_dir="${OCSERV_SIGNER_SECRET_DIR:-}"
signer_state="${OCSERV_SIGNER_STATE_DIR:-}"
for path in "${secret_dir}" "${signer_dir}" "${signer_state}" "${export_dir}"; do
  [[ "${path}" =~ ^/[A-Za-z0-9._/-]+$ ]] || fail "directories must be absolute plain paths: '${path}'"
done
record="${secret_dir}/quick-install.json"

# Require an existing canonical directory whose every ancestor is root-owned and
# not group/world writable, so nobody else can replace a path component.
require_protected_ancestry() {
  local ancestor="$1" ancestor_uid ancestor_mode
  [[ -d "${ancestor}" && "$(realpath -e -- "${ancestor}")" == "${ancestor}" ]] ||
    fail "${ancestor} must be an existing canonical directory without symlink ancestry"
  while true; do
    IFS=: read -r ancestor_uid ancestor_mode < <(stat -c '%u:%a' "${ancestor}")
    if [[ "${ancestor_uid}" != 0 ]] || (( (8#${ancestor_mode} & 8#022) != 0 )); then
      fail "${ancestor} must be root-owned and not group/world writable"
    fi
    [[ "${ancestor}" == / ]] && break
    ancestor="$(dirname -- "${ancestor}")"
  done
}

# Create a missing private directory, or require an existing one to match.
private_dir() {
  local path="$1" owner="$2"
  require_protected_ancestry "$(dirname -- "${path}")"
  if [[ ! -e "${path}" && ! -L "${path}" ]]; then
    install -d -o "${owner%:*}" -g "${owner#*:}" -m 0700 -- "${path}"
  fi
  [[ -d "${path}" && ! -L "${path}" && "$(stat -c '%u:%g:%a' "${path}")" == "${owner}:700" ]] ||
    fail "${path} must be a ${owner} mode-0700 directory"
}

private_dir "${secret_dir}" 0:0
private_dir "${signer_dir}" 65532:65532
private_dir "${signer_state}" 65532:65532

# Staged files are prepared with their final owner and mode in a root-only
# directory, then renamed into place. Neither the staging directory nor the
# CA recovery source sits in a directory a service user can write.
stage="${secret_dir}/.quick-stage"
staging="${secret_dir}/.quick-ca"
legacy_staging="${signer_dir}/.quick-ca"

# Publish a finished file with its final owner and mode by a pure rename. A
# failure (including a cross-device target) keeps the prepared material.
place() {
  local source="$1" target="$2" owner="$3" mode="$4" temporary
  temporary="$(mktemp "${stage}/place.XXXXXX")"
  cat -- "${source}" >"${temporary}"
  chown "${owner}" -- "${temporary}"
  chmod "${mode}" -- "${temporary}"
  rename.ul -- "${temporary}" "${target}" "${temporary}" ||
    fail "atomic publication failed; keep prepared materials"
}

# Write stdin to a missing target; existing targets are reused.
put() {
  local target="$1" owner="$2" mode="$3"
  cat >"${work}/put"
  if [[ ! -e "${target}" && ! -L "${target}" ]]; then
    place "${work}/put" "${target}" "${owner}" "${mode}"
  fi
  rm -f -- "${work}/put"
}

need() {
  local target="$1"
  [[ -f "${target}" && ! -L "${target}" && -s "${target}" ]] || fail "${target} is missing"
}

random_hex() { openssl rand -hex 32; }

# Ed25519 seed (last 32 bytes of the private DER) as lowercase hex.
seed_hex() { openssl pkey -in "$1" -outform DER | tail -c 32 | od -An -v -tx1 | tr -d ' \n'; }

endpoint_id() {
  local seed
  seed="$(cat -- "$1")"
  [[ "${seed}" =~ ^[0-9a-f]{64}$ ]] || fail "$1 is not a hex Ed25519 seed"
  # PKCS#8 Ed25519 prefix followed by the seed.
  printf '%b' "$(printf '302e020100300506032b657004220420%s' "${seed}" | sed 's/../\\x&/g')" |
    openssl pkey -inform DER -pubout -outform DER | tail -c 32 | od -An -v -tx1 | tr -d ' \n'
}

controller_files=(database-owner-url database-app-url session-key audit-checkpoint-key certificate-signer-token
  postgres-owner-password postgres-app-password postgres-backup-password postgres.pgpass audit-event-key controller-command-signing-key.pem
  controller-command-verification-key.pem relay-access-token controller-iroh.key)
signer_files=(issuer-chain.pem issuer-key.pem tls-cert.pem tls-key.pem api-token tls-ca.pem)

require_all() {
  local name
  for name in "${controller_files[@]}"; do need "${secret_dir}/${name}"; done
  for name in "${signer_files[@]}"; do need "${signer_dir}/${name}"; done
}

if [[ -e "${record}" || -L "${record}" ]]; then
  need "${record}"
  [[ "$(jq -r .phase "${record}")" == materials-complete ]] || fail "${record} is not a completed record"
  require_all
  rm -rf -- "${staging}" "${stage}"
  if [[ -e "${legacy_staging}" || -L "${legacy_staging}" ]]; then
    note "left ${legacy_staging} untouched; inspect and remove it deliberately"
  fi
  note "materials already complete; nothing generated"
  exit 0
fi
if [[ -e "${signer_state}/ledger.db" || -L "${signer_state}/ledger.db" ]]; then
  fail "the Signer ledger exists without a quick install record; refusing to generate materials"
fi
if [[ -e "${legacy_staging}" || -L "${legacy_staging}" ]]; then
  fail "${legacy_staging} is a CA recovery directory in the Signer-owned directory and cannot be trusted; it is preserved and not used. Inspect it as root, then remove it or move it deliberately, and rerun"
fi

rm -rf -- "${stage}"
install -d -o 0 -g 0 -m 0700 -- "${stage}"
work="$(mktemp -d "${stage}/work.XXXXXX")"
trap 'rm -rf -- "${work}"' EXIT

# Controller secrets.
for name in session-key audit-checkpoint-key certificate-signer-token \
  postgres-owner-password postgres-app-password postgres-backup-password; do
  random_hex | put "${secret_dir}/${name}" 0:0 0444
done
for role in owner app; do
  printf 'postgres://ocservia_%s:%s@postgres:5432/ocservia?sslmode=disable\n' "${role}" \
    "$(tr -d '\n' <"${secret_dir}/postgres-${role}-password")" |
    put "${secret_dir}/database-${role}-url" 0:0 0444
done
printf 'postgres:5432:*:ocservia_backup:%s\n' "$(tr -d '\n' <"${secret_dir}/postgres-backup-password")" |
  put "${secret_dir}/postgres.pgpass" 0:0 0444
random_hex | put "${secret_dir}/audit-event-key" 65534:65532 0400
random_hex | put "${secret_dir}/relay-access-token" 65532:65532 0400
if [[ ! -e "${secret_dir}/controller-command-signing-key.pem" ]]; then
  openssl genpkey -algorithm ED25519 -out "${work}/command.pem" 2>/dev/null
  place "${work}/command.pem" "${secret_dir}/controller-command-signing-key.pem" 65534:65532 0400
fi
openssl pkey -in "${secret_dir}/controller-command-signing-key.pem" -pubout |
  put "${secret_dir}/controller-command-verification-key.pem" 0:65532 0440
if [[ ! -e "${secret_dir}/controller-iroh.key" ]]; then
  openssl genpkey -algorithm ED25519 -out "${work}/iroh.pem" 2>/dev/null
  seed_hex "${work}/iroh.pem" >"${work}/iroh.key"
  place "${work}/iroh.key" "${secret_dir}/controller-iroh.key" 65532:65532 0400
fi
put "${signer_dir}/api-token" 65532:65532 0400 <"${secret_dir}/certificate-signer-token"
cmp -s -- "${signer_dir}/api-token" "${secret_dir}/certificate-signer-token" ||
  fail "Signer api-token differs from certificate-signer-token"

# Signer HTTPS identity from a one-shot TLS CA whose key is discarded.
tls_present=0
for name in tls-cert.pem tls-key.pem tls-ca.pem; do
  if [[ -e "${signer_dir}/${name}" ]]; then tls_present=$((tls_present + 1)); fi
done
if ((tls_present != 3)); then
  # Nothing trusts a partial set before the first activation; replace it.
  openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "${work}/tls-ca.key"
  openssl req -x509 -new -key "${work}/tls-ca.key" -sha256 -days 1830 -subj "/CN=ocservia Signer TLS CA" \
    -addext basicConstraints=critical,CA:TRUE,pathlen:0 -addext keyUsage=critical,keyCertSign,cRLSign \
    -out "${work}/tls-ca.pem"
  openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "${work}/tls-key.pem"
  openssl req -new -key "${work}/tls-key.pem" -subj /CN=signer -out "${work}/tls.csr"
  printf 'basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature\nextendedKeyUsage=serverAuth\nsubjectAltName=DNS:signer\n' \
    >"${work}/tls.ext"
  openssl x509 -req -in "${work}/tls.csr" -CA "${work}/tls-ca.pem" -CAkey "${work}/tls-ca.key" \
    -set_serial "0x$(openssl rand -hex 16)" -sha256 -days 1826 -extfile "${work}/tls.ext" \
    -out "${work}/tls-cert.pem" 2>/dev/null
  rm -f -- "${work}/tls-ca.key"
  place "${work}/tls-key.pem" "${signer_dir}/tls-key.pem" 65532:65532 0400
  place "${work}/tls-cert.pem" "${signer_dir}/tls-cert.pem" 65532:65532 0400
  place "${work}/tls-ca.pem" "${signer_dir}/tls-ca.pem" 65532:65532 0444
fi

# Offline root CA and online issuing intermediate. The staging directory is the
# commit point: once it holds the "ready" marker the same CA is reused. It is
# removed only after the completion record is written.
if [[ -e "${staging}" || -L "${staging}" ]]; then
  [[ -d "${staging}" && ! -L "${staging}" && "$(stat -c '%u:%g:%a' "${staging}")" == 0:0:700 ]] ||
    fail "${staging} must be a root:root mode-0700 directory"
fi
read_passphrase() {
  local first second
  if [[ -n "${passphrase_file}" ]]; then
    [[ -f "${passphrase_file}" && ! -L "${passphrase_file}" ]] || fail "root CA passphrase file must be a regular file"
    [[ "$(stat -c '%u' "${passphrase_file}")" == 0 && $((8#$(stat -c '%a' "${passphrase_file}") & 8#077)) == 0 ]] ||
      fail "root CA passphrase file must be root-owned and not group/world accessible"
    IFS= read -r passphrase <"${passphrase_file}" || [[ -n "${passphrase:-}" ]] || fail "root CA passphrase file is empty"
  elif [[ "${interactive}" == true ]]; then
    IFS= read -rsp "Root CA passphrase: " first || fail "no root CA passphrase given"
    echo >&2
    IFS= read -rsp "Repeat root CA passphrase: " second || fail "no root CA passphrase given"
    echo >&2
    [[ "${first}" == "${second}" ]] || fail "root CA passphrases differ"
    passphrase="${first}"
  else
    fail "--non-interactive requires --root-ca-passphrase-file"
  fi
  ((${#passphrase} >= 12)) || fail "root CA passphrase must have at least 12 characters"
}

if [[ -f "${staging}/ready" ]]; then
  note "reusing the prepared root CA from an interrupted run"
elif [[ -e "${signer_dir}/issuer-chain.pem" && -e "${signer_dir}/issuer-key.pem" ]]; then
  :
elif [[ -e "${signer_dir}/issuer-chain.pem" || -e "${signer_dir}/issuer-key.pem" ]]; then
  fail "only one of issuer-chain.pem and issuer-key.pem exists; restore the pair or remove it"
else
  read_passphrase
  rm -rf -- "${staging}"
  install -d -o 0 -g 0 -m 0700 -- "${staging}"
  openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -aes-256-cbc -pass fd:3 \
    -out "${staging}/root-ca.key.enc" 3< <(printf '%s' "${passphrase}")
  openssl req -x509 -new -key "${staging}/root-ca.key.enc" -passin fd:3 -sha256 -days 7305 \
    -subj "/CN=ocservia Offline Root CA" -addext basicConstraints=critical,CA:TRUE,pathlen:1 \
    -addext keyUsage=critical,keyCertSign,cRLSign -out "${staging}/root-ca.crt" 3< <(printf '%s' "${passphrase}")
  openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "${staging}/issuer-key.pem"
  openssl req -new -key "${staging}/issuer-key.pem" -subj "/CN=ocservia Online Issuing CA" -out "${work}/issuer.csr"
  printf 'basicConstraints=critical,CA:TRUE,pathlen:0\nkeyUsage=critical,keyCertSign,cRLSign\nsubjectKeyIdentifier=hash\nauthorityKeyIdentifier=keyid:always\n' \
    >"${work}/issuer.ext"
  openssl x509 -req -in "${work}/issuer.csr" -CA "${staging}/root-ca.crt" -CAkey "${staging}/root-ca.key.enc" \
    -passin fd:3 -set_serial "0x$(openssl rand -hex 16)" -sha256 -days 1826 -extfile "${work}/issuer.ext" \
    -out "${staging}/issuer.crt" 3< <(printf '%s' "${passphrase}") 2>/dev/null
  cat -- "${staging}/issuer.crt" "${staging}/root-ca.crt" >"${staging}/issuer-chain.pem"
  unset passphrase
  : >"${staging}/ready"
fi

custody=""
if [[ -f "${staging}/ready" ]]; then
  fingerprint="$(openssl x509 -in "${staging}/root-ca.crt" -noout -fingerprint -sha256 | sed 's/.*=//; s/://g' |
    tr 'A-F' 'a-f')"
  private_dir "${export_dir}" 0:0
  place "${staging}/root-ca.crt" "${export_dir}/root-ca.crt" 0:0 0444
  place "${staging}/root-ca.key.enc" "${export_dir}/root-ca.key.enc" 0:0 0400
  printf '%s  root-ca.crt\n' "${fingerprint}" >"${work}/fingerprint"
  place "${work}/fingerprint" "${export_dir}/root-ca.sha256" 0:0 0444
  if [[ "${interactive}" == true ]]; then
    cat >&2 <<EOF
The offline root CA was written to ${export_dir}:
  root-ca.crt      public certificate
  root-ca.key.enc  passphrase-encrypted private key
  SHA-256          ${fingerprint}
Copy root-ca.key.enc and root-ca.crt off this host and keep the passphrase
separately. The encrypted key is then deleted from this host.
EOF
    IFS= read -rp "Type the last 8 characters of the SHA-256 fingerprint to confirm: " answer ||
      fail "root CA custody was not confirmed; rerun to continue"
    [[ "${answer,,}" == "${fingerprint: -8}" ]] || fail "root CA custody was not confirmed; rerun to continue"
    rm -f -- "${export_dir}/root-ca.key.enc"
    custody=removed-from-host
  else
    custody=delegated
    note "root CA custody is delegated: move ${export_dir}/root-ca.key.enc off this host"
  fi
  place "${staging}/issuer-key.pem" "${signer_dir}/issuer-key.pem" 65532:65532 0400
  place "${staging}/issuer-chain.pem" "${signer_dir}/issuer-chain.pem" 65532:65532 0400
fi

require_all
[[ -n "${custody}" ]] || custody=preexisting
root_fingerprint="$(awk '/BEGIN CERTIFICATE/{n++} n==2' "${signer_dir}/issuer-chain.pem" | openssl x509 -noout -fingerprint -sha256 | sed 's/.*=//; s/://g' |
  tr 'A-F' 'a-f')"
{
  for name in "${controller_files[@]}"; do printf '%s\t%s\n' "${name}" "$(sha256sum <"${secret_dir}/${name}" | cut -d' ' -f1)"; done
  for name in "${signer_files[@]}"; do printf 'signer/%s\t%s\n' "${name}" "$(sha256sum <"${signer_dir}/${name}" | cut -d' ' -f1)"; done
} | jq -Rn --arg custody "${custody}" --arg root "${root_fingerprint}" \
  --arg endpoint "$(endpoint_id "${secret_dir}/controller-iroh.key")" \
  '{version: 1, phase: "materials-complete", root_ca_custody: $custody, root_ca_sha256: $root,
    controller_endpoint_id: $endpoint,
    files: ([inputs | split("\t") | {key: .[0], value: .[1]}] | from_entries)}' >"${work}/record"
place "${work}/record" "${record}" 0:0 0400
rm -rf -- "${staging}" "${stage}"
note "materials complete; record: ${record}"
