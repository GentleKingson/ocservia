#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/env.sh
source "${ROOT}/scripts/env.sh"

RUN_ID="${RUN_ID:?RUN_ID is required}"
if [[ ! "${RUN_ID}" =~ ^I13-[A-Za-z0-9._-]+$ ]]; then
  echo "RUN_ID is invalid" >&2
  exit 1
fi
PREFIX="$(printf '%s' "${RUN_ID}" | tr '[:upper:]_' '[:lower:]-' | tr -cd 'a-z0-9-')"
TMP_BASE="${RUNNER_TEMP:-${TMPDIR:-/tmp}}"
NATIVE_ROOT="${TMP_BASE%/}/ocservia-${PREFIX}-native"
PORT="${OCSERVIA_I13_OCSERV_PORT:-44443}"
PASSWORD="native-password-sentinel"
ARTIFACT_DIR="${ARTIFACT_DIR:-}"

cleanup() {
  local status=$? cleanup_status=0
  trap - EXIT INT TERM
  set +e
  if [[ "${status}" -ne 0 ]]; then
    [[ -f "${NATIVE_ROOT}/server.log" ]] && sed -n '1,160p' "${NATIVE_ROOT}/server.log" >&2
    [[ -f "${NATIVE_ROOT}/client.log" ]] && sed -n '1,160p' "${NATIVE_ROOT}/client.log" >&2
  fi
  if [[ -n "${ARTIFACT_DIR}" ]]; then
    mkdir -p "${ARTIFACT_DIR}" || cleanup_status=1
    [[ -f "${NATIVE_ROOT}/server.log" ]] && cp -f "${NATIVE_ROOT}/server.log" "${ARTIFACT_DIR}/ocserv.log"
    [[ -f "${NATIVE_ROOT}/client.log" ]] && cp -f "${NATIVE_ROOT}/client.log" "${ARTIFACT_DIR}/openconnect.log"
  fi
  if [[ -f "${NATIVE_ROOT}/launcher.pid" ]]; then
    kill "$(<"${NATIVE_ROOT}/launcher.pid")" 2>/dev/null || true
    wait "$(<"${NATIVE_ROOT}/launcher.pid")" 2>/dev/null || true
  fi
  for _ in $(seq 1 50); do
    if ! ss -H -ltn "sport = :${PORT}" | grep -q .; then
      break
    fi
    sleep 0.1
  done
  if ss -H -ltn "sport = :${PORT}" | grep -q .; then
    echo "native Ocserv integration left port ${PORT} listening" >&2
    cleanup_status=1
  fi
  rm -rf "${NATIVE_ROOT}"
  if ((status != 0)); then
    exit "${status}"
  fi
  exit "${cleanup_status}"
}
trap cleanup EXIT INT TERM

if [[ "$(id -u)" -ne 0 ]]; then
  echo "native Ocserv integration requires root" >&2
  exit 1
fi
if ss -H -ltn "sport = :${PORT}" | grep -q .; then
  echo "native Ocserv integration port ${PORT} is already in use" >&2
  exit 1
fi

mkdir -p "${NATIVE_ROOT}/groups"
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 \
  -out "${NATIVE_ROOT}/private.pem" >/dev/null 2>&1
chmod 600 "${NATIVE_ROOT}/private.pem"
openssl pkey -in "${NATIVE_ROOT}/private.pem" -pubout \
  -out "${NATIVE_ROOT}/public.pem" >/dev/null 2>&1
printf '%s' "${PASSWORD}" | openssl pkeyutl -encrypt \
  -pubin -inkey "${NATIVE_ROOT}/public.pem" \
  -pkeyopt rsa_padding_mode:oaep -pkeyopt rsa_oaep_md:sha256 \
  -out "${NATIVE_ROOT}/sealed-password.bin"

OCSERVIA_I13_NATIVE_ROOT="${NATIVE_ROOT}" cargo test \
  --manifest-path "${ROOT}/rust/Cargo.toml" \
  -p ocservia-ocserv-adapter tests::native_user_and_group_operations \
  -- --ignored --exact

cat >"${NATIVE_ROOT}/capture.sh" <<EOF
#!/bin/sh
env >"${NATIVE_ROOT}/client.env"
exit 0
EOF
chmod 700 "${NATIVE_ROOT}/capture.sh"
cat >"${NATIVE_ROOT}/groups/staff" <<EOF
route = 203.0.113.0/24
banner = I13_STAFF_GROUP_APPLIED
EOF
cat >"${NATIVE_ROOT}/ocserv.conf" <<EOF
auth = "plain[passwd=${NATIVE_ROOT}/ocpasswd]"
tcp-port = ${PORT}
udp-port = 0
run-as-user = ocserv
run-as-group = ocserv
listen-host = 127.0.0.1
socket-file = ${NATIVE_ROOT}/ocserv.sock
occtl-socket-file = ${NATIVE_ROOT}/occtl.sock
pid-file = ${NATIVE_ROOT}/ocserv.pid
server-cert = /etc/ssl/certs/ssl-cert-snakeoil.pem
server-key = /etc/ssl/private/ssl-cert-snakeoil.key
isolate-workers = false
max-clients = 4
max-same-clients = 2
keepalive = 30
dpd = 30
mobile-dpd = 30
try-mtu-discovery = false
auth-timeout = 30
min-reauth-time = 30
max-ban-score = 80
ban-reset-time = 60
cookie-timeout = 300
deny-roaming = false
rekey-time = 172800
rekey-method = ssl
use-occtl = true
device = vpns-i13
ipv4-network = 10.250.0.0
ipv4-netmask = 255.255.255.0
dns = 192.0.2.53
config-per-group = ${NATIVE_ROOT}/groups/
EOF

ocserv --test-config -c "${NATIVE_ROOT}/ocserv.conf"
ocserv -c "${NATIVE_ROOT}/ocserv.conf" -f -d 9 >"${NATIVE_ROOT}/server.log" 2>&1 &
printf '%s' "$!" >"${NATIVE_ROOT}/launcher.pid"
for _ in $(seq 1 50); do
  if ss -H -ltn "sport = :${PORT}" | grep -q .; then
    break
  fi
  sleep 0.1
done
ss -H -ltn "sport = :${PORT}" | grep -q .
for _ in $(seq 1 50); do
  if compgen -G "${NATIVE_ROOT}/ocserv.sock.*.0" >/dev/null; then
    break
  fi
  sleep 0.1
done
compgen -G "${NATIVE_ROOT}/ocserv.sock.*.0" >/dev/null

PIN="$(openssl x509 -in /etc/ssl/certs/ssl-cert-snakeoil.pem -pubkey -noout | openssl pkey -pubin -outform DER 2>/dev/null | openssl dgst -sha256 -binary | openssl base64 -A)"
set +e
printf '%s\n' "${PASSWORD}" | timeout 12 openconnect \
  --protocol=anyconnect --user=alice --passwd-on-stdin \
  --servercert "pin-sha256:${PIN}" --script-tun \
  --script "${NATIVE_ROOT}/capture.sh" \
  "https://127.0.0.1:${PORT}" \
  >"${NATIVE_ROOT}/client.log" 2>&1
set -e

grep -q '^CISCO_SPLIT_INC_0_ADDR=203.0.113.0$' "${NATIVE_ROOT}/client.env"
grep -q '^CISCO_SPLIT_INC_0_MASKLEN=24$' "${NATIVE_ROOT}/client.env"
grep -q 'Configured as 10.250.0.' "${NATIVE_ROOT}/client.log"
echo "native Ocserv login applied the staff config-per-group route"
