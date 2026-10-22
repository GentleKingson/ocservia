#!/usr/bin/env bash
# Test fixture only: install source binaries in a disposable runner.
set -euo pipefail
[[ "$EUID" == 0 && "${OCSERVIA_COMPAT_DISPOSABLE:-}" == 1 ]]
for process in /proc/[0-9]*/comm; do
  if [[ -r "$process" && "$(cat "$process" 2>/dev/null)" == ocserv ]]; then
    echo 'stop the disposable ocserv service before replacing test binaries' >&2
    exit 1
  fi
done
work="$(mktemp -d)"
trap 'rm -rf -- "$work"' EXIT
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y --no-install-recommends build-essential meson ninja-build pkg-config curl ca-certificates xz-utils \
  libgnutls28-dev libev-dev libseccomp-dev libreadline-dev libnl-route-3-dev libtalloc-dev \
  libprotobuf-c-dev protobuf-c-compiler libtasn1-bin libsystemd-dev
curl -fsSL --retry 3 https://www.infradead.org/ocserv/download/ocserv-1.6.0.tar.xz -o "$work/ocserv.tar.xz"
echo "2d3303a0d9e06eabfa08694b9dca3c558fef36418857bebf1b8de57ece5f9f32  $work/ocserv.tar.xz" | sha256sum -c --strict -
tar -xJf "$work/ocserv.tar.xz" -C "$work"
cd "$work/ocserv-1.6.0"
meson setup build --prefix=/usr --sysconfdir=/etc --localstatedir=/var --libexecdir=libexec \
  -Dpam=disabled -Dradius=disabled -Dgssapi=disabled -Doidc-auth=disabled -Dtests=false
ninja -C build -j "${OCSERVIA_BUILD_JOBS:-2}"
install -m 755 build/src/ocserv /usr/sbin/ocserv
install -m 755 build/src/ocserv-worker /usr/sbin/ocserv-worker
install -m 755 build/src/occtl/occtl /usr/bin/occtl
install -m 755 build/src/ocpasswd/ocpasswd /usr/bin/ocpasswd
version="$(/usr/sbin/ocserv --version 2>&1)"
[[ "${version%%$'\n'*}" == 'OpenConnect VPN Server 1.6.0' ]]
printf '%s\n' "$version"
sha256sum /usr/sbin/ocserv /usr/sbin/ocserv-worker /usr/bin/occtl /usr/bin/ocpasswd
