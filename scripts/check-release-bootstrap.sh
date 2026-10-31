#!/usr/bin/env bash
# Verifies that a released Controller bootstrap launcher equals its Git source except for
# the stamped default version.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if (($# != 2)); then
  echo "usage: check-release-bootstrap.sh <stamped-install-controller> <release-tag>" >&2
  exit 2
fi

stamped="$1"
release_tag="$2"
[[ -f "${stamped}" && ! -L "${stamped}" ]] || {
  echo "stamped bootstrap launcher is missing or a symlink: ${stamped}" >&2
  exit 1
}
[[ "$(grep -cxF "readonly DEFAULT_VERSION=\"${release_tag}\"" "${stamped}")" == 1 ]] || {
  echo "stamped bootstrap launcher does not default to ${release_tag}" >&2
  exit 1
}
sed "s|^readonly DEFAULT_VERSION=\"${release_tag}\"\$|readonly DEFAULT_VERSION=\"\"|" "${stamped}" |
  cmp -s - "${ROOT}/deploy/bootstrap/install-controller" || {
  echo "stamped bootstrap launcher differs from deploy/bootstrap/install-controller beyond its default version" >&2
  exit 1
}
