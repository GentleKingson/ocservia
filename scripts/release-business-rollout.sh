#!/usr/bin/env bash
# Manual integration final stage, run by release-business-probe.sh on its
# disposable runner: a real one-node browser canary rollout to the next Agent
# patch version. It replaces the installed Agent, so nothing may follow it.
set -Eeuo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
: "${ARTIFACT_DIR:?}" "${VERSION:?}" "${OCSERVIA_BUSINESS_TEST_WORK_DIR:?}" "${OCSERVIA_BUSINESS_TEST_NODE_ID:?}" "${PACKAGE_ARCH:?}"
# shellcheck source=scripts/env.sh
# shellcheck disable=SC1091
source "$ROOT/scripts/env.sh"
work="${OCSERVIA_BUSINESS_TEST_WORK_DIR}"
compose() { "${ROOT}/deploy/production/compose.sh" "$@"; }
IFS=. read -r major minor patch <<<"$VERSION"
export ROLLOUT_VERSION="${major}.${minor}.$((patch + 1))"
rollout="${work}/rollout"
mkdir -m 700 "$rollout"
(export VERSION="$ROLLOUT_VERSION" OUTPUT_DIR="$rollout"
  env -u BUILDX_BUILDER bash "$ROOT/scripts/build-agent-binaries.sh"
  bash "$ROOT/scripts/package-agent.sh") >"${ARTIFACT_DIR}/rollout-build.log" 2>&1
package="${rollout}/ocservia-agent-${ROLLOUT_VERSION}-linux-${PACKAGE_ARCH}.tar.gz"
# The upgrader never downloads: it reads only this root-owned spool.
sudo install -d -o root -g root -m 0700 /var/lib/ocservia-upgrade/package-spool
sudo install -o root -g root -m 0644 "$package" /var/lib/ocservia-upgrade/package-spool/
jq -n --arg version "$ROLLOUT_VERSION" --arg arch "$PACKAGE_ARCH" \
  --arg digest "$(sha256sum "$package" | cut -d' ' -f1)" \
  '{releases: [{version: $version, architecture: $arch, package_sha256: $digest}]}' >"${rollout}/catalog.json"
chmod 644 "${rollout}/catalog.json"
# The shipped launcher mounts no release catalog. Diagnostics only: render the
# launcher's own model, add the catalog, and recreate just the Controller.
export OCSERV_RECOMMENDED_AGENT_VERSION="$ROLLOUT_VERSION"
compose config --format json >"${rollout}/compose.json"
jq -n --arg catalog "${rollout}/catalog.json" '{services: {"control-plane": {
  environment: {OCSERV_AGENT_RELEASE_MANIFEST: "/etc/ocservia/agent-releases.json"},
  volumes: [{type: "bind", source: $catalog, target: "/etc/ocservia/agent-releases.json", read_only: true}]}}}' \
  >"${rollout}/catalog-override.json"
COMPOSE_DISABLE_ENV_FILE=1 docker compose --env-file /dev/null -p ocservia-production \
  -f "${rollout}/compose.json" -f "${rollout}/catalog-override.json" \
  up -d --no-build --no-deps --force-recreate --wait control-plane
python3 "${ROOT}/scripts/release-business-api.py" rollout_ready
NODE_EXTRA_CA_CERTS="${work}/ca.crt" node "${ROOT}/scripts/release-business-browser.mjs" rollout
python3 "${ROOT}/scripts/release-business-api.py" rollout_verify
