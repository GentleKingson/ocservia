#!/usr/bin/env bash
# Exercise the same runtime archive that the public bootstrap installs.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf -- "${work}"' EXIT
fixture_tag=v2.0.0
"${ROOT}/scripts/prepare-bootstrap-release-assets.sh" "${work}" "${fixture_tag}"
cmp "${work}/install-controller" "${work}/controller-bootstrap.sh"
# shellcheck source=deploy/lib/controller-runtime.sh
source "${ROOT}/deploy/lib/controller-runtime.sh"
runtime_download() { cp -- "${work}/${1##*/}" "$2"; }
fail() { echo "$*" >&2; exit 1; }

runtime_fetch "${work}/installed" "${fixture_tag}" "$(git -C "${ROOT}" rev-parse HEAD)"
runtime_fetch "${work}/installed" "${fixture_tag}"
cmp "${work}/installed/deploy/lib/quick-materials.sh" "${ROOT}/deploy/lib/quick-materials.sh"
if "${work}/installed/deploy/production/install.sh" --quick --non-interactive \
    --controller-domain vpn.example.test --acme-email ops@example.test >"${work}/error" 2>&1; then
  fail "unattended Quick accepted missing custody input"
fi
grep -Fq -- "--non-interactive requires --root-ca-passphrase-file" "${work}/error"
printf '\n# changed\n' >>"${work}/installed/deploy/production/install.sh"
if runtime_fetch "${work}/installed" "${fixture_tag}" >"${work}/error" 2>&1; then
  fail "retained runtime drift was accepted"
fi
grep -Fq "runtime content changed" "${work}/error"
