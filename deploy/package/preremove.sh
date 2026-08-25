#!/usr/bin/env bash
# Rendered by scripts/package-native-agent.sh: @VERSION@/@ARCH@ are replaced
# with the concrete package version and amd64|arm64 architecture.
set -euo pipefail

package_root="/usr/share/ocservia-agent"
version="@VERSION@"
package_arch="@ARCH@"

# deb passes remove|upgrade|deconfigure|failed-upgrade; rpm passes the number
# of package instances that remain after the operation (0 on erase). Only a
# real removal uninstalls; upgrades and failed upgrades keep installed files.
removal=false
case "${1:-}" in
  remove | 0) removal=true ;;
esac

if [[ "${removal}" != true || ! -x /usr/libexec/ocservia/ocservia-agent ]]; then
  exit 0
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

# Without --purge-state, uninstall-agent.sh preserves identity, state, and
# configuration directories; purging stays an explicit operator decision.
"${verified_root}/scripts/uninstall-agent.sh"
rm -rf -- "${verified_root%%/extracted/*}"
