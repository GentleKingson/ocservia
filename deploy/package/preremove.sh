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

# A real removal tears the production relay contract down, so it also retires
# an unconsumed production request — for example one left by an install whose
# postinst failed — instead of letting it surprise a later plain install.
# Upgrades and failed upgrades keep the request retryable.
if [[ "${removal}" == true ]]; then
  rm -f -- /etc/ocservia/agent-install-production-relays
fi

if [[ "${removal}" != true || ! -x /usr/libexec/ocservia/ocservia-agent ]]; then
  exit 0
fi

archive="${package_root}/ocservia-agent-${version}-linux-${package_arch}.tar.gz"
expected_digest="$(awk '{print $1}' "${archive}.sha256")"
verified_root="$("${package_root}/verify-agent-package.sh" "${archive}" "${expected_digest}")"

# Without --purge-state, uninstall-agent.sh preserves identity, state, and
# configuration directories; purging stays an explicit operator decision.
"${verified_root}/scripts/uninstall-agent.sh"
rm -rf -- "${verified_root%%/extracted/*}"
