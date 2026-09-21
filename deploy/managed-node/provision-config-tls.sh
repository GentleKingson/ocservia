#!/usr/bin/env bash
set -euo pipefail
umask 077

# Operator-only local provisioning. No Agent/Controller RPC calls this script.
die() { printf '%s\n' "$*" >&2; exit 1; }
[[ ${EUID} == 0 ]] || die 'run as the node root operator'
[[ $# == 5 || $# == 6 ]] || die 'usage: provision-config-tls.sh NODE_UUID REF_UUID VERSION CERTIFICATE PRIVATE_KEY [CA_CERTIFICATE]'
node=$1 ref=$2 version=$3 cert=$4 key=$5 ca=${6:-}
uuid='^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'
[[ ${node} =~ ${uuid} && ${ref} =~ ${uuid} ]] || die 'node and reference must be canonical UUIDv7'
[[ ${version} =~ ^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$ && ${version} != *..* ]] || die 'invalid immutable version'
root=/etc/ocservia-agent/config-tls

ancestry() {
  local path=$1 current=/ part uid mode
  [[ ${path} == /* && ${path} != *'//'* && ${path} != */../* && ${path} != */./* ]] || die 'unsafe absolute path'
  local -a parts
  IFS=/ read -r -a parts <<<"${path#/}"
  for part in "${parts[@]}"; do
    [[ -n ${part} && ${part} != . && ${part} != .. ]] || die 'unsafe path component'
    current=${current%/}/${part}
    [[ -d ${current} && ! -L ${current} ]] || die 'unsafe directory ancestry'
    read -r uid mode < <(stat -c '%u %a' -- "${current}")
    if [[ ${uid} != 0 ]] || (( (8#${mode} & 8#022) != 0 )); then
      die 'ancestry must be root-owned and not group/world writable'
    fi
  done
}

source_file() {
  local file=$1 private=$2 owner mode links
  ancestry "$(dirname -- "${file}")"
  [[ -f ${file} && ! -L ${file} && -s ${file} ]] || die 'source must be a nonempty regular file'
  read -r owner mode links < <(stat -c '%u:%g %a %h' -- "${file}")
  [[ ${owner} == 0:0 && ${links} == 1 ]] || die 'source ownership/link count is unsafe'
  if [[ ${private} == true ]]; then
    [[ ${mode} == 600 ]] || die 'private key must be root:root 0600'
  else
    [[ ${mode} == 600 || ${mode} == 644 ]] || die 'public certificate mode is unsafe'
  fi
  (( $(stat -c '%s' -- "${file}") <= 262144 )) || die 'source exceeds size bound'
}
source_file "${cert}" false
source_file "${key}" true
if [[ -n ${ca} ]]; then source_file "${ca}" false; fi
worker_uid=$(id -u ocservia-vpn) || die 'provision the dedicated ocservia-vpn user first'
worker_gid=$(id -g ocservia-vpn) || die 'provision the dedicated ocservia-vpn group first'
[[ ${worker_uid} != 0 && ${worker_gid} != 0 && ${worker_gid} == "$(getent group ocservia-vpn | cut -d: -f3)" && $(id -G ocservia-vpn) == "${worker_gid}" ]] || die 'ocservia-vpn must have only its dedicated unprivileged group'
ancestry /etc/ocservia-agent
if [[ ! -e ${root} && ! -L ${root} ]]; then install -d -o root -g root -m 700 "${root}"; fi
ancestry "${root}"
[[ $(stat -c '%u:%g:%a' "${root}") == 0:0:700 ]] || die 'unsafe TLS trust directory'
if [[ ! -e ${root}/.lock && ! -L ${root}/.lock ]]; then
  (set -o noclobber; : >"${root}/.lock") || die 'concurrent lock initialization; inspect and retry provisioning'
fi
[[ -f ${root}/.lock && ! -L ${root}/.lock && $(stat -c '%u:%g:%a:%h' "${root}/.lock") == 0:0:600:1 ]] || die 'unsafe TLS lock'
exec 9<"${root}/.lock"
flock -x 9
if [[ ! -e ${root}/${ref} && ! -L ${root}/${ref} ]]; then install -d -o root -g root -m 700 "${root}/${ref}"; fi
ancestry "${root}/${ref}"
[[ ! -e ${root}/${ref}/${version} && ! -L ${root}/${ref}/${version} ]] || die 'immutable version already exists; never replace or remove it'
stage=$(mktemp -d "${root}/${ref}/.provision-XXXXXXXX")
cleanup() { if [[ -n ${stage:-} ]]; then chmod 700 "${stage}"; rm -rf -- "${stage}"; fi; }
trap cleanup EXIT
install -o root -g root -m 400 -- "${cert}" "${stage}/server-cert.pem"
install -o root -g root -m 400 -- "${key}" "${stage}/server-key.pem"
if [[ -n ${ca} ]]; then install -o root -g root -m 400 -- "${ca}" "${stage}/ca-cert.pem"; fi
openssl x509 -in "${stage}/server-cert.pem" -noout -ext basicConstraints | grep -Eq '^[[:space:]]*CA:FALSE$' || die 'server certificate must explicitly be a non-CA leaf'
openssl verify -no-CAfile -no-CApath -no-CAstore -trusted "${stage}/server-cert.pem" -partial_chain -purpose sslserver "${stage}/server-cert.pem" >&2
certificate_hash=$(openssl x509 -in "${stage}/server-cert.pem" -outform DER | sha256sum | cut -d' ' -f1)
spki_hash=$(openssl x509 -in "${stage}/server-cert.pem" -pubkey -noout | openssl pkey -pubin -outform DER | sha256sum | cut -d' ' -f1)
key_spki_hash=$(openssl pkey -in "${stage}/server-key.pem" -pubout -outform DER | sha256sum | cut -d' ' -f1)
[[ ${spki_hash} == "${key_spki_hash}" ]] || die 'certificate/private key mismatch'
ca_hash=
if [[ -n ${ca} ]]; then
  openssl x509 -in "${stage}/ca-cert.pem" -noout -ext basicConstraints | grep -Eq '^[[:space:]]*CA:TRUE(,|$)' || die 'CA certificate must explicitly have CA:TRUE'
  openssl verify -no-CAfile -no-CApath -no-CAstore -trusted "${stage}/ca-cert.pem" -partial_chain "${stage}/ca-cert.pem" >&2
  ca_hash=$(openssl x509 -in "${stage}/ca-cert.pem" -outform DER | sha256sum | cut -d' ' -f1)
fi
jq -n --arg node "${node}" --arg ref "${ref}" --arg version "${version}" --arg cert "${certificate_hash}" --arg spki "${spki_hash}" --arg ca "${ca_hash}" \
  '{node_id:$node,secret_ref_id:$ref,version:$version,certificate_sha256:$cert,spki_sha256:$spki,ca_sha256:$ca}' >"${stage}/manifest.json"
chmod 400 "${stage}/manifest.json"
sync -f "${stage}"
chmod 500 "${stage}"
mv -T -- "${stage}" "${root}/${ref}/${version}"
stage=
sync -f "${root}/${ref}"
jq -n --arg ref "${ref}" --arg version "${version}" --arg key "${node}/${certificate_hash}/${spki_hash}/${ca_hash:-none}" \
  '{secret_ref_id:$ref,provider:"node-local-tls-v1",key:$key,version:$version}'
