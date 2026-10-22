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
client_pid=""

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
  if grep -rIEq -- 'BEGIN (RSA )?PRIVATE KEY|(^|:)!?[$][156][$]' "${ARTIFACT_DIR}"; then
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
  if [[ -n "${client_pid}" ]]; then
    kill "${client_pid}" 2>/dev/null || true
    wait "${client_pid}" 2>/dev/null || true
  fi
  if [[ -f "${NATIVE_ROOT}/launcher.pid" ]]; then
    kill "$(<"${NATIVE_ROOT}/launcher.pid")" 2>/dev/null || true
    wait "$(<"${NATIVE_ROOT}/launcher.pid")" 2>/dev/null || true
  fi
  persist_artifacts || cleanup_status=1
  scan_artifacts || cleanup_status=1
  if [[ -n "${ARTIFACT_DIR}" && -d "${ARTIFACT_DIR}" ]]; then
    chown -R "${RUN_AS_USER}:${RUN_AS_GROUP}" "${ARTIFACT_DIR}" || cleanup_status=1
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

# Keep version-specific assertions bounded to the reviewed release.
version_output="$(ocserv --version 2>&1)"
OCSERV_160=false
CAPTURE_SECONDS=10
CLIENT_TIMEOUT=12
REKEY_TIME=172800
DEBUG_LEVEL=2
if [[ "${version_output%%$'\n'*}" == 'OpenConnect VPN Server 1.6.0' ]]; then
  OCSERV_160=true
  CAPTURE_SECONDS=60
  CLIENT_TIMEOUT=65
  REKEY_TIME=2
  DEBUG_LEVEL=3
fi

umask 077
mkdir -p "${NATIVE_ROOT}/groups"
chmod 755 "${NATIVE_ROOT}" "${NATIVE_ROOT}/groups"
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
  # Run a host-built native libtest ELF inside a same-architecture container,
  # without installing a second Rust toolchain there. The ordinary Cargo path
  # remains the default. Never accept a binary which silently runs zero tests.
  if [[ -n "${OCSERVIA_I13_TEST_BINARY:-}" ]]; then
    OCSERVIA_I13_NATIVE_ROOT="${NATIVE_ROOT}" OCSERVIA_I13_NATIVE_PHASE="${phase}" \
      "${OCSERVIA_I13_TEST_BINARY}" tests::native_user_and_group_operations --ignored --exact
    return
  fi
  OCSERVIA_I13_NATIVE_ROOT="${NATIVE_ROOT}" OCSERVIA_I13_NATIVE_PHASE="${phase}" cargo test \
    --locked \
    --manifest-path "${ROOT}/rust/Cargo.toml" \
    -p ocservia-ocserv-adapter tests::native_user_and_group_operations \
    -- --ignored --exact
}

if [[ -n "${OCSERVIA_I13_TEST_BINARY:-}" ]]; then
  [[ "${OCSERVIA_I13_TEST_BINARY}" == /* && -x "${OCSERVIA_I13_TEST_BINARY}" ]]
  [[ "$("${OCSERVIA_I13_TEST_BINARY}" --list --ignored --exact tests::native_user_and_group_operations)" == \
    "$(printf 'tests::native_user_and_group_operations: test\n\n1 test, 0 benchmarks')" ]]
fi

run_native_phase create-p1
test "$(stat -c '%u:%g:%a:%h' -- "${NATIVE_ROOT}/ocpasswd")" = "0:0:600:1" \
  || { echo "native ocpasswd does not have authoritative root-only metadata" >&2; exit 1; }
if setpriv --reuid="$(id -u "${RUN_AS_USER}")" --regid="$(id -g "${RUN_AS_USER}")" \
  --clear-groups test -r "${NATIVE_ROOT}/ocpasswd"; then
  echo "unprivileged Agent-equivalent identity can read ocpasswd hashes" >&2
  exit 1
fi
if setpriv --reuid="$(id -u "${RUN_AS_USER}")" --regid="$(id -g "${RUN_AS_USER}")" \
  --clear-groups test -w "${NATIVE_ROOT}/ocpasswd"; then
  echo "unprivileged Agent-equivalent identity can write ocpasswd hashes" >&2
  exit 1
fi
echo "native ocpasswd root-only metadata passed"

printf '%s\n' '#!/bin/sh' "env >\"${NATIVE_ROOT}/client.env\"" "sleep ${CAPTURE_SECONDS}" >"${NATIVE_ROOT}/capture.sh"
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
  "rekey-time = ${REKEY_TIME}" \
  'rekey-method = ssl' \
  'use-occtl = true' \
  'device = vpns-i13' \
  'ipv4-network = 10.250.0.0' \
  'ipv4-netmask = 255.255.255.0' \
  'dns = 192.0.2.53' \
  "config-per-group = ${NATIVE_ROOT}/groups/" \
  >"${NATIVE_ROOT}/ocserv.conf"

if ${OCSERV_160}; then
  mkdir -p "${NATIVE_ROOT}/users"
  chmod 755 "${NATIVE_ROOT}/users"
  printf '# valid default\n' >"${NATIVE_ROOT}/default-user.conf"
  printf '# valid default\n' >"${NATIVE_ROOT}/default-group.conf"
  printf '%s\n' "config-per-user = ${NATIVE_ROOT}/users/" \
    "default-user-config = ${NATIVE_ROOT}/default-user.conf" \
    "default-group-config = ${NATIVE_ROOT}/default-group.conf" >>"${NATIVE_ROOT}/ocserv.conf"
fi
ocserv --test-config -c "${NATIVE_ROOT}/ocserv.conf"
cp "${NATIVE_ROOT}/ocserv.conf" "${NATIVE_ROOT}/invalid.conf"
printf '\nauth = "invalid-auth-backend"\n' >>"${NATIVE_ROOT}/invalid.conf"
if ocserv --test-config -c "${NATIVE_ROOT}/invalid.conf" >/dev/null 2>&1; then
  echo "native Ocserv accepted an invalid authentication configuration" >&2
  exit 1
fi
rm "${NATIVE_ROOT}/invalid.conf"
ocserv -c "${NATIVE_ROOT}/ocserv.conf" -f -d "${DEBUG_LEVEL}" >"${NATIVE_ROOT}/server.log" 2>&1 &
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
  printf '%s\n' "${password}" | timeout 15 openconnect --no-proxy -v --authenticate --non-inter \
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
run_native_phase config-rollback

reject_supplemental_config() {
  local label=$1 before status
  before=$(wc -l <"${NATIVE_ROOT}/server.log")
  rm -f "${NATIVE_ROOT}/client.env"
  set +e
  printf '%s\n' "${P3}" | timeout 10 openconnect --no-proxy --non-inter \
    --protocol=anyconnect --user=alice --passwd-on-stdin \
    --servercert "pin-sha256:${PIN}" --script-tun --script "${NATIVE_ROOT}/capture.sh" \
    "https://127.0.0.1:${PORT}" >"${NATIVE_ROOT}/auth-${label}.log" 2>&1
  status=$?
  set -e
  test "$status" -ne 0
  test "$status" -ne 124
  test ! -f "${NATIVE_ROOT}/client.env"
  tail -n "+$((before + 1))" "${NATIVE_ROOT}/server.log" | grep -q 'error reading additional configuration'
  server_ready
  echo "${label}=PASS"
}
if ${OCSERV_160}; then
  for kind in user group; do
    if [[ "$kind" == user ]]; then file="${NATIVE_ROOT}/users/alice"; else file="${NATIVE_ROOT}/groups/staff"; fi
    if [[ -f "$file" ]]; then cp "$file" "${file}.saved"; fi
    printf '[unterminated-section\n' >"$file"
    reject_supplemental_config "malformed-${kind}"
    rm "$file"
    printf '[unterminated-section\n' >"${NATIVE_ROOT}/default-${kind}.conf"
    reject_supplemental_config "malformed-default-${kind}"
    rm "${NATIVE_ROOT}/default-${kind}.conf"
    reject_supplemental_config "missing-default-${kind}"
    printf '# valid default\n' >"${NATIVE_ROOT}/default-${kind}.conf"
    if [[ -f "${file}.saved" ]]; then mv "${file}.saved" "$file"; fi
  done
fi

rm -f "${NATIVE_ROOT}/client.env"
printf '%s\n' "${P3}" | timeout "${CLIENT_TIMEOUT}" openconnect --no-proxy \
  --protocol=anyconnect --user=alice --passwd-on-stdin \
  --servercert "pin-sha256:${PIN}" --script-tun \
  --script "${NATIVE_ROOT}/capture.sh" \
  "https://127.0.0.1:${PORT}" \
  >"${NATIVE_ROOT}/route.log" 2>&1 &
client_pid=$!
for _ in $(seq 1 100); do
  [[ -f "${NATIVE_ROOT}/client.env" ]] && break
  sleep 0.1
done
server_ready
test -f "${NATIVE_ROOT}/client.env"
grep -q '^CISCO_SPLIT_INC_0_ADDR=203.0.113.0$' "${NATIVE_ROOT}/client.env"
grep -q '^CISCO_SPLIT_INC_0_MASKLEN=24$' "${NATIVE_ROOT}/client.env"
grep -q 'Configured as 10.250.0.' "${NATIVE_ROOT}/route.log"
if ${OCSERV_160}; then
  # Upstream checks the internal rekey deadline every 30 seconds.
  for _ in $(seq 1 450); do
    if [[ $(grep -c 'TLS 1.3 session keys refreshed' "${NATIVE_ROOT}/server.log") -ge 2 ]]; then break; fi
    sleep 0.1
  done
  test "$(grep -c 'TLS 1.3 session keys refreshed' "${NATIVE_ROOT}/server.log")" -ge 2
  test "$(grep -c 'assigning tun device' "${NATIVE_ROOT}/server.log")" = 1
  echo 'tls13_keyupdate_without_tunnel_rebuild=PASS'
fi
occtl -s "${NATIVE_ROOT}/occtl.sock" --json show users >"${NATIVE_ROOT}/sessions.json"
occtl -s "${NATIVE_ROOT}/occtl.sock" --json show ip bans >"${NATIVE_ROOT}/bans.json"
run_native_phase occtl
set +e
wait "${client_pid}"
route_status=$?
set -e
client_pid=""
run_native_phase concurrency

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
  'invalid_config_rejected=PASS' \
  'native_parser_failure_rollback=PASS' \
  'occtl_output_parse=PASS' \
  'session_terminate=PASS' \
  'concurrent_user_group_password_replay=PASS' \
  "config_per_group_client_exit=${route_status}" \
  >"${NATIVE_ROOT}/lifecycle-summary.txt"
if ${OCSERV_160}; then
  printf '%s\n' 'ocserv_version=1.6.0' \
    'malformed_user_group_config_rejected=PASS' \
    'malformed_default_config_rejected=PASS' \
    'missing_default_config_rejected=PASS' \
    'tls13_keyupdate_without_tunnel_rebuild=PASS' >>"${NATIVE_ROOT}/lifecycle-summary.txt"
fi
persist_artifacts
scan_artifacts
echo "native Ocserv P1/P2/P3 authentication lifecycle passed"
