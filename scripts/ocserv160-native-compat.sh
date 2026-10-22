#!/usr/bin/env bash
# Run inside the isolated compatibility image on a matching native host.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
evidence="${1:?evidence directory}"
: "${EXPECTED_ARCH:?}" "${OCSERVIA_I13_TEST_BINARY:?}"
[[ "$EUID" == 0 && "$(dpkg --print-architecture)" == "$EXPECTED_ARCH" ]]
case "$EXPECTED_ARCH" in
  amd64) kernel=x86_64; machine='Advanced Micro Devices X86-64' ;;
  arm64) kernel=aarch64; machine=AArch64 ;;
  *) exit 2 ;;
esac
[[ "$(uname -m)" == "$kernel" ]]
test -c /dev/net/tun
version="$(ocserv --version 2>&1)"
[[ "${version%%$'\n'*}" == 'OpenConnect VPN Server 1.6.0' ]]
mkdir -p "$evidence"
{
  uname -m
  dpkg --print-architecture
  cat /etc/os-release
  printf '%s\n' "$version"
  openssl version
  dpkg-query -W libgnutls30t64 openconnect
  for binary in /usr/sbin/ocserv /usr/sbin/ocserv-worker /usr/bin/occtl /usr/bin/ocpasswd "$OCSERVIA_I13_TEST_BINARY"; do
    readelf -h "$binary" | grep -F 'Machine:' | grep -F "$machine"
    sha256sum "$binary"
    libraries="$(ldd "$binary")"
    printf '%s\n' "$libraries"
    if grep -Fq 'not found' <<<"$libraries"; then exit 1; fi
  done
} >"$evidence/native.txt"
# Complete-profile tests require a protected ancestry owned by their runtime UID.
work="$(mktemp -d /ocserv160-tests-XXXXXX)"
trap 'rm -rf -- "$work"' EXIT
cd "$work"
"$OCSERVIA_I13_TEST_BINARY" >"$evidence/adapter-tests.txt" 2>&1
mkdir -p "$evidence/i13"
RUN_ID="I13-ocserv160-$EXPECTED_ARCH" ARTIFACT_DIR="$evidence/i13" \
  bash "$ROOT/scripts/i13-native-ocserv-integration.sh" >"$evidence/i13/run.txt" 2>&1
for checkpoint in ocserv_version=1.6.0 native_parser_failure_rollback=PASS \
  malformed_user_group_config_rejected=PASS malformed_default_config_rejected=PASS \
  missing_default_config_rejected=PASS tls13_keyupdate_without_tunnel_rebuild=PASS session_terminate=PASS; do
  grep -Fx "$checkpoint" "$evidence/i13/lifecycle-summary.txt"
done
