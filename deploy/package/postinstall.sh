#!/usr/bin/env bash
# Rendered by scripts/package-native-agent.sh: @VERSION@/@ARCH@ are replaced
# with the concrete package version and amd64|arm64 architecture.
set -euo pipefail

package_root="/usr/share/ocservia-agent"
version="@VERSION@"
package_arch="@ARCH@"

# Debian and rpm both enforce package architecture, but refuse a mismatched
# host before anything is staged or installed, mirroring verify-agent-package.
case "$(uname -m)" in
  x86_64) host_arch=amd64 ;;
  aarch64) host_arch=arm64 ;;
  *)
    echo "ocservia-agent: unsupported host architecture $(uname -m)" >&2
    exit 1
    ;;
esac
if [[ "${package_arch}" != "${host_arch}" ]]; then
  echo "ocservia-agent: package architecture ${package_arch} does not match host architecture ${host_arch}" >&2
  exit 1
fi

fingerprint="$(cat -- "${package_root}/trusted-release-key.sha256")"
if [[ ! "${fingerprint}" =~ ^[0-9a-f]{64}$ ]]; then
  echo "ocservia-agent: embedded release key fingerprint is malformed" >&2
  exit 1
fi

archive="${package_root}/ocservia-agent-${version}-linux-${package_arch}.tar.gz"
verified_root="$(AGENT_TRUSTED_KEY_SHA256="${fingerprint}" \
  "${package_root}/verify-agent-package.sh" \
  "${archive}" "${archive}.sha256" "${archive}.sha256.sig" \
  "${package_root}/release-signing.pub.pem")"

# install-agent.sh remains the only install-layout authority; the native
# package just hands the verified staging scripts control.
if [[ -x /usr/libexec/ocservia/ocservia-agent ]]; then
  if [[ -e /usr/lib/systemd/system/ocservia-agent.service.d/10-production-relays.conf ]]; then
    INSTALL_PRODUCTION_RELAYS=true "${verified_root}/scripts/upgrade-agent.sh"
  else
    "${verified_root}/scripts/upgrade-agent.sh"
  fi
else
  "${verified_root}/scripts/install-agent.sh"
fi

rm -rf -- "${verified_root%%/extracted/*}"
echo "ocservia-agent ${version} installed; /etc/ocservia-agent/agent.env is required before enabling ocservia-agent.service"
