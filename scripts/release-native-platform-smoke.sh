#!/usr/bin/env bash
# Manual diagnostics only; package/adapter evidence is never Controller E2E.
set -Eeuo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
mode="${1:?host or runtime}"
distro="${2:?ubuntu24.04 or debian13}"
arch="${3:?amd64 or arm64}"
work="${4:?private work directory}"
test_binary="${5:?native adapter libtest ELF}"
products="${6:?Agent package directory}"
case "$distro:$arch" in ubuntu24.04:arm64|debian13:amd64|debian13:arm64) ;; *) exit 2 ;; esac
case "$arch" in
  amd64) kernel=x86_64; machine='Advanced Micro Devices X86-64' ;;
  arm64) kernel=aarch64; machine=AArch64 ;;
esac
[[ "$(uname -m)" == "$kernel" && "$(dpkg --print-architecture)" == "$arch" ]]

check_elf() {
  local path=$1 libraries
  readelf -h "$path" | grep -F "Machine:" | grep -F "$machine"
  libraries="$(ldd "$path")"
  printf '%s\n' "$libraries"
  ! grep -Fq 'not found' <<<"$libraries"
}

if [[ "$mode" == runtime ]]; then
  [[ "$EUID" == 0 ]]
  # shellcheck disable=SC1091
  source /etc/os-release
  [[ "$ID$VERSION_ID" == "$distro" ]]
  python3 "$ROOT/scripts/verify-native-platform-package.py" "$work" "$distro" "$arch" >"$work/package.json"
  package="$work/ocserv_1.5.0-1.${distro}.2_${arch}.deb"
  # Refuse an existing installation; no user data or identity is cleanup input.
  [[ ! -e /etc/ocserv && ! -e /etc/ocservia-agent && ! -e /usr/libexec/ocservia ]]
  touch "$work/install-started"
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq
  apt-get install -y --no-install-recommends "$package" openconnect ssl-cert iproute2 binutils file openssl
  [[ "$(dpkg-query -W -f='${Status}|${Version}|${Architecture}' ocserv)" == "install ok installed|1.5.0-1~${distro}.2|$arch" ]]
  [[ "$(ocserv --version 2>&1 | head -n1)" == 'OpenConnect VPN Server 1.5.0' ]]
  occtl --help
  test -s /etc/ocserv/ocserv.conf
  dpkg-query -L ocserv
  dpkg --audit
  apt-get check
  for binary in /usr/sbin/ocserv /usr/bin/occtl /usr/bin/ocpasswd "$test_binary"; do check_elf "$binary"; done
  ocserv --test-config -c /etc/ocserv/ocserv.conf
  printf 'PACKAGE_OCSERV=PASS\n' >>"$work/checkpoints.txt"

  # Debian containers have no systemd. Inspect and execute the real verified
  # package payload without substituting a systemctl shim or invoking scriptlets.
  agent="$products/ocservia-agent_0.0.0-1_${arch}.deb"
  [[ "$(dpkg-deb -f "$agent" Package Version Architecture)" == \
    "$(printf 'Package: ocservia-agent\nVersion: 0.0.0-1\nArchitecture: %s' "$arch")" ]]
  dpkg-deb -f "$agent" Depends
  apt-get install -y --no-install-recommends openssl python3 util-linux
  apt-get --simulate install "$agent"
  dpkg-deb -x "$agent" "$work/agent-payload"
  payload="$work/agent-payload/usr/share/ocservia-agent"
  archive="$payload/ocservia-agent-0.0.0-linux-${arch}.tar.gz"
  verified="$(bash "$payload/verify-agent-package.sh" "$archive" "$(awk '{print $1}' "$archive.sha256")")"
  for binary in ocservia-agent ocservia-privd ocservia-upgrader; do
    path="$verified/rust/target/release/$binary"
    check_elf "$path"
    [[ "$("$path" --version)" == "$binary 0.0.0" ]]
  done
  for unit in ocservia-agent.service ocservia-privd.service ocservia-agent-retention.timer; do
    test -s "$verified/deploy/systemd/$unit"
  done
  rm -rf -- "${verified%%/extracted/*}" "$work/agent-payload"
  printf 'PACKAGE_AGENT_PAYLOAD=PASS\nNATIVE_RUNTIME=PASS\n' >>"$work/checkpoints.txt"
  # This fixture only listens on loopback in the disposable host/container.
  test -c /dev/net/tun
  RUN_ID="I13-p3-${arch}" ARTIFACT_DIR="$work/i13" OCSERVIA_I13_TEST_BINARY="$test_binary" \
    bash "$ROOT/scripts/i13-native-ocserv-integration.sh"
  grep -Fx 'session_terminate=PASS' "$work/i13/lifecycle-summary.txt"
  printf 'NATIVE_OPERATIONS=PASS\n' >>"$work/checkpoints.txt"
  exit
fi

[[ "$mode" == host && "${GITHUB_ACTIONS:-}" == true && "${RUNNER_ENVIRONMENT:-}" == github-hosted ]]
[[ ! -e "$work" ]]
mkdir -m 700 "$work"
container="ocservia-p3-${GITHUB_RUN_ID:?}-${GITHUB_RUN_ATTEMPT:?}-${arch}"
cleanup() {
  local status=$?
  trap - EXIT
  if [[ "$distro" == debian13 ]]; then
    docker rm -f "$container" >/dev/null 2>&1 || status=1
  elif [[ -f "$work/install-started" ]]; then
    # shellcheck disable=SC2024 # Output is deliberately runner-owned evidence.
    sudo apt-get remove -y ocserv >"$work/package-removal.txt" 2>&1 || status=1
  fi
  # Only fixture paths belonging to this run; the hosted VM itself is disposable.
  sudo rm -rf -- "$work/agent-payload"
  rm -f -- "$work"/*.deb "$work/install-started"
  sudo chown -R "$(id -u):$(id -g)" "$work"
  printf 'cleanup_exit=%s\n' "$status" >"$work/cleanup.txt"
  exit "$status"
}
trap cleanup EXIT
bash "$ROOT/scripts/release-upgrade-native.sh" "$arch" >"$work/native.json"
git -C "$ROOT" rev-parse HEAD >"$work/source-sha.txt"
printf 'run_id=%s\nattempt=%s\ndistribution=%s\n' "$GITHUB_RUN_ID" "$GITHUB_RUN_ATTEMPT" "$distro" >"$work/run.txt"
base=https://github.com/GentleKingson/ocserv-backport/releases/download/1.5.0-2
curl -fsSL --retry 3 https://api.github.com/repos/GentleKingson/ocserv-backport/releases/tags/1.5.0-2 -o "$work/release.json"
curl -fsSL --retry 3 "$base/SHA256SUMS" -o "$work/SHA256SUMS"
curl -fsSL --retry 3 "$base/ocserv_1.5.0-1.${distro}.2_${arch}.deb" -o "$work/ocserv_1.5.0-1.${distro}.2_${arch}.deb"
python3 "$ROOT/scripts/verify-native-platform-package.py" "$work" "$distro" "$arch"
if [[ "$distro" == ubuntu24.04 ]]; then
  sudo bash "$0" runtime "$distro" "$arch" "$work" "$test_binary" "$products" 2>&1 | tee "$work/runtime.log"
else
  # Official docker.io/library/debian:trixie-slim index, resolved 2026-10-15.
  image=docker.io/library/debian@sha256:a29215f6a35e51e22adffa17f89e9d2ef06214e64a2bad10d765c46aea49f11f
  docker pull --platform "linux/$arch" "$image"
  [[ "$(docker image inspect --format '{{.Architecture}}' "$image")" == "$arch" ]]
  docker image inspect --format '{{json .RepoDigests}}' "$image" >"$work/image.json"
  # Only this container receives NET_ADMIN and TUN, without host networking,
  # systemd, Docker socket access, or privileged mode.
  docker run --name "$container" --platform "linux/$arch" --cap-add=NET_ADMIN --device=/dev/net/tun \
    --volume "$ROOT:$ROOT:ro" --volume "$work:$work" --volume "$products:$products:ro" \
    "$image" bash -c 'apt-get update -qq && apt-get install -y --no-install-recommends python3 ca-certificates && exec bash "$@"' \
    platform "$ROOT/scripts/release-native-platform-smoke.sh" runtime "$distro" "$arch" "$work" "$test_binary" "$products" \
    2>&1 | tee "$work/runtime.log"
fi
for checkpoint in PACKAGE_OCSERV PACKAGE_AGENT_PAYLOAD NATIVE_RUNTIME NATIVE_OPERATIONS; do
  grep -Fx "$checkpoint=PASS" "$work/checkpoints.txt"
done
