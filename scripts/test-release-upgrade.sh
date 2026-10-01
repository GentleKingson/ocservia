#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"
fixture="$(mktemp -d)"
trap 'rm -rf -- "${fixture}"' EXIT
(
  VERSION=1.0.2
  for PRODUCTION_SIGNER_ACCEPTANCE in false true; do
    # shellcheck disable=SC1090
    source <(sed -n '/^managed_options=/,+1p' scripts/release-business-probe.sh)
    [[ "${managed_options[0]} ${managed_options[1]}" == '--version v1.0.2' ]]
    if [[ "$PRODUCTION_SIGNER_ACCEPTANCE" == true ]]; then
      [[ "${managed_options[2]}" == --root-lifecycle && "${#managed_options[@]}" == 3 ]]
    else
      [[ "${#managed_options[@]}" == 2 ]]
    fi
  done
)
# The auth fixture changes only Controller settings. Never race the unchanged
# transport container against its trust backend, or continue after failed health.
# shellcheck disable=SC1090
source <(sed -n '/^configure_auth_peers() {/,/^}/p' scripts/release-business-probe.sh)
(
  # shellcheck disable=SC2317 # Called by the sourced configure_auth_peers.
  compose() { printf '%s\n' "$*" >>"${fixture}/auth-order"; }
  # shellcheck disable=SC2317 # Called by the sourced configure_auth_peers.
  python3() { printf '%s\n' "python3 $*" >>"${fixture}/auth-order"; }
  configure_auth_peers
)
[[ "$(sed -n '1p' "${fixture}/auth-order")" == 'up -d --no-deps --wait control-plane' ]]
[[ "$(sed -n '2p' "${fixture}/auth-order")" == 'start transportd' ]]
[[ "$(sed -n '3p' "${fixture}/auth-order")" == "python3 ${ROOT}/scripts/release-business-api.py transport_ready" ]]
[[ "$(wc -l <"${fixture}/auth-order")" == 3 ]]
set +e
(
  set -e
  # shellcheck disable=SC2317 # Called by the sourced configure_auth_peers.
  compose() { printf '%s\n' "$*" >>"${fixture}/auth-failed-order"; return 19; }
  configure_auth_peers
)
auth_status=$?
set -e
[[ "${auth_status}" == 19 && "$(wc -l <"${fixture}/auth-failed-order")" == 1 ]]
python3 scripts/test-release-business-smoke.py
node scripts/test-release-upgrade.mjs
bash scripts/test-release-rust-cache.sh
ruby scripts/test-release-workflows.rb
