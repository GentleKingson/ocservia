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
ARTIFACT_DIR="${ARTIFACT_DIR:-}"
RUN_AS_USER="${SUDO_USER:-nobody}"
RUN_AS_GROUP="$(id -gn "${RUN_AS_USER}")"
P1=""
P2=""
P3=""

server_ready() {
  [[ -f "${NATIVE_ROOT}/launcher.pid" ]] || return 1
  kill -0 "$(<"${NATIVE_ROOT}/launcher.pid")" 2>/dev/null || return 1
  ss -H -ltn "sport = :${PORT}" | grep -q .
}

persist_artifacts() {
  [[ -n "${ARTIFACT_DIR}" ]] || return 0
  mkdir -p "${ARTIFACT_DIR}"
  [[ -f "${NATIVE_ROOT}/server.log" ]] && cp -f "${NATIVE_ROOT}/server.log" "${ARTIFACT_DIR}/ocserv.log"
  for log in "${NATIVE_ROOT}"/auth-*.log "${NATIVE_ROOT}/route.log"; do
    [[ -f "${log}" ]] && cp -f "${log}" "${ARTIFACT_DIR}/$(basename "${log}")"
  done
  [[ -f "${NATIVE_ROOT}/lifecycle-summary.txt" ]] && cp -f "${NATIVE_ROOT}/lifecycle-summary.txt" "${ARTIFACT_DIR}/lifecycle-summary.txt"
}

scan_artifacts() {
  [[ -n "${ARTIFACT_DIR}" && -d "${ARTIFACT_DIR}" ]] || return 0
  local secret
  for secret in "${P1}" "${P2}" "${P3}"; do
    if [[ -n "${secret}" ]] && grep -rIFq -- "${secret}" "${ARTIFACT_DIR}"; then
      echo "native artifact contains a plaintext canary" >&2
      rm -rf "${ARTIFACT_DIR}"
      return 1
    fi
  done
  if grep -rIEq -- 'BEGIN (RSA )?PRIVATE KEY|(^|:)!?[$]6[$]' "${ARTIFACT_DIR}"; then
    echo "native artifact contains private-key or ocpasswd hash material" >&2
    rm -rf "${ARTIFACT_DIR}"
    return 1
  fi
  echo "native canary artifact scan passed"
}

cleanup() {
  local status=$? cleanup_status=0
  trap - EXIT INT TERM
  set +e
  if [[ -f "${NATIVE_ROOT}/launcher.pid" ]]; then
    kill "$(<"${NATIVE_ROOT}/launcher.pid")" 2>/dev/null || true
    wait "$(<"${NATIVE_ROOT}/launcher.pid")" 2>/dev/null || true
  fi
  persist_artifacts || cleanup_status=1
  scan_artifacts || cleanup_status=1
  if [[ -n "${ARTIFACT_DIR}" && -d "${ARTIFACT_DIR}" ]]; then
    chmod -R u=rwX,go=rX "${ARTIFACT_DIR}" || cleanup_status=1
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
  unset P1 P2 P3
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

umask 077
mkdir -p "${NATIVE_ROOT}/groups"
P1="$(openssl rand -base64 36 | tr -d '\n')"
P2="$(openssl rand -base64 36 | tr -d '\n')"
P3="$(openssl rand -base64 36 | tr -d '\n')"
test -n "${P1}"
test -n "${P2}"
test -n "${P3}"
test "${P1}" != "${P2}"
test "${P1}" != "${P3}"
test "${P2}" != "${P3}"

openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 \
  -out "${NATIVE_ROOT}/private.pem" >/dev/null 2>&1
chmod 600 "${NATIVE_ROOT}/private.pem"
openssl pkey -in "${NATIVE_ROOT}/private.pem" -pubout \
  -out "${NATIVE_ROOT}/public.pem" >/dev/null 2>&1
for index in 1 2 3; do
  case "${index}" in
    1) password=${P1} ;;
    2) password=${P2} ;;
    3) password=${P3} ;;
  esac
  printf '%s' "${password}" | openssl pkeyutl -encrypt \
    -pubin -inkey "${NATIVE_ROOT}/public.pem" \
    -pkeyopt rsa_padding_mode:oaep -pkeyopt rsa_oaep_md:sha256 \
    -out "${NATIVE_ROOT}/sealed-p${index}.bin"
done
unset password

run_native_phase() {
  local phase=$1
  OCSERVIA_I13_NATIVE_ROOT="${NATIVE_ROOT}" OCSERVIA_I13_NATIVE_PHASE="${phase}" cargo test \
    --manifest-path "${ROOT}/rust/Cargo.toml" \
    -p ocservia-ocserv-adapter tests::native_user_and_group_operations \
    -- --ignored --exact
}

run_native_phase create-p1

printf '%s\n' '#!/bin/sh' "env >\"${NATIVE_ROOT}/client.env\"" 'exit 0' >"${NATIVE_ROOT}/capture.sh"
chmod 700 "${NATIVE_ROOT}/capture.sh"
printf '%s\n' 'route = 203.0.113.0/24' 'banner = I13_STAFF_GROUP_APPLIED' >"${NATIVE_ROOT}/groups/staff"
printf '%s\n' \
  "auth = \"plain[passwd=${NATIVE_ROOT}/ocpasswd]\"" \
  "tcp-port = ${PORT}" \
  'udp-port = 0' \
  "run-as-user = ${RUN_AS_USER}" \
  "run-as-group = ${RUN_AS_GROUP}" \
  'listen-host = 127.0.0.1' \
  "socket-file = ${NATIVE_ROOT}/ocserv.sock" \
  "occtl-socket-file = ${NATIVE_ROOT}/occtl.sock" \
  "pid-file = ${NATIVE_ROOT}/ocserv.pid" \
  'server-cert = /etc/ssl/certs/ssl-cert-snakeoil.pem' \
  'server-key = /etc/ssl/private/ssl-cert-snakeoil.key' \
  'tls-priorities = "NORMAL:%SERVER_PRECEDENCE:%COMPAT:-VERS-SSL3.0:-VERS-TLS1.0:-VERS-TLS1.1"' \
  'isolate-workers = false' \
  'max-clients = 4' \
  'max-same-clients = 2' \
  'keepalive = 30' \
  'dpd = 30' \
  'mobile-dpd = 30' \
  'try-mtu-discovery = false' \
  'auth-timeout = 30' \
  'min-reauth-time = 30' \
  'max-ban-score = 1000' \
  'ban-reset-time = 60' \
  'cookie-timeout = 300' \
  'deny-roaming = false' \
  'rekey-time = 172800' \
  'rekey-method = ssl' \
  'use-occtl = true' \
  'device = vpns-i13' \
  'ipv4-network = 10.250.0.0' \
  'ipv4-netmask = 255.255.255.0' \
  'dns = 192.0.2.53' \
  "config-per-group = ${NATIVE_ROOT}/groups/" \
  >"${NATIVE_ROOT}/ocserv.conf"

ocserv --test-config -c "${NATIVE_ROOT}/ocserv.conf"
ocserv -c "${NATIVE_ROOT}/ocserv.conf" -f -d 2 >"${NATIVE_ROOT}/server.log" 2>&1 &
printf '%s' "$!" >"${NATIVE_ROOT}/launcher.pid"
for _ in $(seq 1 50); do
  if server_ready; then
    break
  fi
  sleep 0.1
done
server_ready
for _ in $(seq 1 50); do
  if compgen -G "${NATIVE_ROOT}/ocserv.sock.*.0" >/dev/null; then
    break
  fi
  sleep 0.1
done
compgen -G "${NATIVE_ROOT}/ocserv.sock.*.0" >/dev/null

PIN="$(openssl x509 -in /etc/ssl/certs/ssl-cert-snakeoil.pem -pubkey -noout | openssl pkey -pubin -outform DER 2>/dev/null | openssl dgst -sha256 -binary | openssl base64 -A)"

authenticate() {
  local label=$1 password=$2 expectation=$3
  local output="${NATIVE_ROOT}/auth-${label}.out"
  local log="${NATIVE_ROOT}/auth-${label}.log"
  local server_before status
  server_ready
  server_before="$(wc -l <"${NATIVE_ROOT}/server.log")"
  set +e
  printf '%s\n' "${password}" | timeout 15 openconnect -v --authenticate --non-inter \
    --protocol=anyconnect --user=alice --passwd-on-stdin \
    --servercert "pin-sha256:${PIN}" "https://127.0.0.1:${PORT}" \
    >"${output}" 2>"${log}"
  status=$?
  set -e
  server_ready
  if [[ "${expectation}" == success ]]; then
    test "${status}" = 0
    grep -q '^COOKIE=' "${output}"
  else
    test "${status}" -ne 0
    test "${status}" -ne 124
    if ! grep -Eiq 'authentication.*fail|failed to obtain.*cookie|HTTP/[0-9.]+ 401|login.*fail|password.*(invalid|incorrect)' "${log}" \
      && ! tail -n "+$((server_before + 1))" "${NATIVE_ROOT}/server.log" | grep -Eiq 'authentication.*fail|failed.*auth|invalid.*password|login.*fail'; then
      echo "negative native login did not prove authentication rejection" >&2
      return 1
    fi
  fi
  rm -f "${output}"
}

authenticate create-p1 "${P1}" success
run_native_phase rotate-p2
authenticate old-p1 "${P1}" failure
authenticate current-p2 "${P2}" success
run_native_phase disable
authenticate disabled-p2 "${P2}" failure
run_native_phase rotate-disabled-p3
authenticate disabled-old-p2 "${P2}" failure
authenticate disabled-current-p3 "${P3}" failure
run_native_phase enable
authenticate enabled-old-p2 "${P2}" failure
authenticate enabled-current-p3 "${P3}" success
run_native_phase group-apply

rm -f "${NATIVE_ROOT}/client.env"
set +e
printf '%s\n' "${P3}" | timeout 12 openconnect \
  --protocol=anyconnect --user=alice --passwd-on-stdin \
  --servercert "pin-sha256:${PIN}" --script-tun \
  --script "${NATIVE_ROOT}/capture.sh" \
  "https://127.0.0.1:${PORT}" \
  >"${NATIVE_ROOT}/route.log" 2>&1
route_status=$?
set -e
server_ready
if [[ "${route_status}" -ne 0 && "${route_status}" -ne 124 ]]; then
  echo "native config-per-group login failed" >&2
  exit 1
fi
grep -q '^CISCO_SPLIT_INC_0_ADDR=203.0.113.0$' "${NATIVE_ROOT}/client.env"
grep -q '^CISCO_SPLIT_INC_0_MASKLEN=24$' "${NATIVE_ROOT}/client.env"
grep -q 'Configured as 10.250.0.' "${NATIVE_ROOT}/route.log"

printf '%s\n' \
  'create_p1_login=PASS' \
  'rotate_p1_to_p2=PASS' \
  'old_p1_rejected=PASS' \
  'p2_login=PASS' \
  'disable_rejection=PASS' \
  'disabled_rotate_p3=PASS' \
  'enable_p3_login=PASS' \
  'old_p2_rejected=PASS' \
  'group_apply=PASS' \
  'config_per_group=PASS' \
  >"${NATIVE_ROOT}/lifecycle-summary.txt"
persist_artifacts
scan_artifacts
echo "native Ocserv P1/P2/P3 authentication lifecycle passed"
