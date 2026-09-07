#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fixture="$(mktemp -d "${HOME}/.ocservia-network-upgrade.XXXXXX")"
project="ocservia-network-upgrade-$$"
image="${CONTROLLER_PROXY_TEST_IMAGE:-caddy:2.10.2-alpine@sha256:4c6e91c6ed0e2fa03efd5b44747b625fec79bc9cd06ac5235a779726618e530d}"
repo="${fixture}/repo"
mkdir -p "${repo}/deploy/production" "${repo}/scripts" "${fixture}/bin" "${fixture}/release" "${fixture}/state"
compose() { docker compose -p "${project}" -f "${repo}/deploy/production/compose.yaml" "$@"; }
cleanup() {
  docker rm -f "${project}-foreign" >/dev/null 2>&1 || true
  compose down --volumes --remove-orphans >/dev/null 2>&1 || true
  rm -rf -- "${fixture}"
}
trap cleanup EXIT

# Real Docker networks/volumes and the real guarded lifecycle, but lightweight
# healthy containers instead of the production database and application images.
docker compose -f "${ROOT}/deploy/production/compose.yaml" config \
  --no-interpolate --no-env-resolution --format json | jq --arg image "${image}" '
  . as $production |
  {image: $image, entrypoint: ["sh", "-c", "sleep 600"],
   healthcheck: {test: ["CMD", "true"], interval: "1s", timeout: "1s", retries: 5}} as $service |
  {services: {
    gateway: ($service + {networks: {application: $production.services.gateway.networks.application}}),
    "control-plane": ($service + {networks: {application: null}, environment: {
      OCSERV_AUTH_TRUSTED_PROXY_CIDRS: $production.services["control-plane"].environment.OCSERV_AUTH_TRUSTED_PROXY_CIDRS}}),
    transportd: ($service + {networks: {application: null}, volumes: ["transport-runtime:/state"]}),
    postgres: ($service + {networks: {database: null}, volumes: ["postgres-data:/state"]}),
    backup: ($service + {networks: {database: null}})
  }, networks: {application: ($production.networks.application | del(.name)), database: {internal: true}},
     volumes: {"transport-runtime": {}, "postgres-data": {}}}
' >"${fixture}/target.json"
# v0.4.0 used an internal bridge with automatic IPAM and dynamic app addresses.
jq 'del(.networks.application.ipam, .services.gateway.networks.application.ipv4_address,
  .services["control-plane"].environment)' "${fixture}/target.json" >"${repo}/deploy/production/compose.yaml"
cp "${ROOT}/deploy/production/controller.sh" "${ROOT}/deploy/production/compose.sh" "${repo}/deploy/production/"
cp "${ROOT}/scripts/verify-controller-release-bundle.sh" "${repo}/scripts/"
git -C "${repo}" init -q
git -C "${repo}" add .
git -C "${repo}" -c user.name=Test -c user.email=test@example.invalid commit -qm legacy
legacy_commit="$(git -C "${repo}" rev-parse HEAD)"
compose up -d --wait
legacy_network="$(docker network inspect "${project}_application" --format '{{.Id}}')"
database_network="$(docker network inspect "${project}_database" --format '{{.Id}}')"
postgres="$(compose ps -q postgres)"
backup="$(compose ps -q backup)"
compose exec -T postgres sh -c 'printf preserved > /state/sentinel'
compose exec -T transportd sh -c 'printf preserved > /state/sentinel'
cp "${fixture}/target.json" "${repo}/deploy/production/compose.yaml"
git -C "${repo}" add .
git -C "${repo}" -c user.name=Test -c user.email=test@example.invalid commit -qm target
target_commit="$(git -C "${repo}" rev-parse HEAD)"

export OCSERV_APPLICATION_SUBNET=198.18.81.0/24
export OCSERV_APPLICATION_IP_RANGE=198.18.81.128/25
export OCSERV_GATEWAY_APPLICATION_IP=198.18.81.2
unset OCSERV_AUTH_TRUSTED_PROXY_CIDRS
export NETWORK_TEST_PROJECT="${project}" NETWORK_TEST_COMPOSE="${repo}/deploy/production/compose.yaml"
NETWORK_TEST_DOCKER="$(command -v docker)"
export NETWORK_TEST_DOCKER
cat >"${fixture}/bin/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [[ "${1:-} ${2:-}" == 'network rm' && "${FAIL_NETWORK_RM:-0}" == 1 ]]; then exit 1; fi
exec "${NETWORK_TEST_DOCKER}" "$@"
EOF
cat >"${fixture}/bin/compose.sh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
# Images are already running locally; only activation failures are injected.
[[ "${1:-}" != pull ]] || exit 0
if [[ "${1:-}" == up && "${FAIL_UP:-0}" == 1 ]]; then exit 1; fi
exec docker compose -p "${NETWORK_TEST_PROJECT}" -f "${NETWORK_TEST_COMPOSE}" "$@"
EOF
cat >"${fixture}/bin/smoke.sh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[[ "${1:-}" == --release-file && -f "${2:-}" ]]
EOF
chmod 755 "${fixture}/bin/"*
openssl genpkey -algorithm ED25519 -out "${fixture}/signing.key" >/dev/null 2>&1
openssl pkey -in "${fixture}/signing.key" -pubout -out "${fixture}/signing.pub" >/dev/null 2>&1
arch="$(docker version --format '{{.Server.Arch}}')"
for version in 0.4.0 0.5.0 0.5.1; do
  commit="${target_commit}"
  [[ "${version}" != 0.4.0 ]] || commit="${legacy_commit}"
  jq -n --arg version "${version}" --arg commit "${commit}" --arg image "${image}" --arg arch "${arch}" '{
    manifest_version: 1, release_version: $version, release_tag: ("v" + $version),
    source_commit: $commit, platform: ("linux/" + $arch), database_migration: 1,
    images: {gateway: $image, control: $image, transport: $image, backup: $image, postgres: $image, otel: $image}
  }' >"${fixture}/release/${version}.json"
  (cd "${fixture}/release" && sha256sum "${version}.json") >"${fixture}/release/${version}.json.sha256"
done
(cd "${fixture}/release" && sha256sum ./*.json | sed 's|  ./|  |') >"${fixture}/release/SHA256SUMS"
openssl pkeyutl -sign -rawin -inkey "${fixture}/signing.key" \
  -in "${fixture}/release/SHA256SUMS" -out "${fixture}/release/SHA256SUMS.sig"
cp "${fixture}/release/0.4.0.json" "${fixture}/state/current-release.json"
chmod 700 "${fixture}/state"
chmod 600 "${fixture}/state/current-release.json"

controller() {
  PATH="${fixture}/bin:${PATH}" OCSERV_CONTROLLER_STATE_ROOT="${fixture}/state" \
    OCSERV_CONTROLLER_RELEASE_PUBLIC_KEY="${fixture}/signing.pub" \
    OCSERV_CONTROLLER_COMPOSE_SH="${fixture}/bin/compose.sh" \
    OCSERV_CONTROLLER_SMOKE_SH="${fixture}/bin/smoke.sh" \
    "${repo}/deploy/production/controller.sh" "$@"
}
upgrade() { controller upgrade --release-file "${fixture}/release/0.5.0.json"; }
assert_pending() {
  cmp "${fixture}/release/0.4.0.json" "${fixture}/state/current-release.json"
  test ! -e "${fixture}/state/previous-release.json"
  jq -e '.phase == "failed" and .manifest.release_version == "0.5.0" and
    .previous_manifest.release_version == "0.4.0"' "${fixture}/state/pending-release.json" >/dev/null
}
assert_database() {
  [[ "$(compose ps -q postgres)" == "${postgres}" && "$(compose ps -q backup)" == "${backup}" ]]
  [[ "$(docker network inspect "${project}_database" --format '{{.Id}}')" == "${database_network}" ]]
  [[ "$(compose exec -T postgres cat /state/sentinel)" == preserved ]]
}
expect_upgrade_failure() {
  local message="$1"
  if upgrade >"${fixture}/output.log" 2>&1; then echo "expected failure: ${message}" >&2; exit 1; fi
  grep -F "${message}" "${fixture}/output.log"
  assert_pending
  assert_database
}

docker run -d --name "${project}-foreign" --network "${project}_application" \
  --entrypoint sh "${image}" -c 'sleep 600' >/dev/null
gateway="$(compose ps -q gateway)"
expect_upgrade_failure 'unexpected attachment'
[[ "$(compose ps -q gateway)" == "${gateway}" ]]
docker rm -f "${project}-foreign" >/dev/null

# A stopped app container must also be removed, not left with a stale network ID.
compose stop transportd
FAIL_NETWORK_RM=1 expect_upgrade_failure 'application network removal failed'
[[ "$(docker network inspect "${project}_application" --format '{{.Id}}')" == "${legacy_network}" ]]
[[ -z "$(compose ps -aq gateway control-plane transportd)" ]]
FAIL_UP=1 expect_upgrade_failure 'upgrade activation started but was not confirmed successful'
[[ -z "$(docker network ls -q --filter "name=^${project}_application$")" ]]
upgrade
assert_database
[[ "$(compose exec -T transportd cat /state/sentinel)" == preserved ]]
network="$(docker network inspect "${project}_application" --format '{{.Id}}')"
[[ "${network}" != "${legacy_network}" ]]
docker network inspect "${network}" | jq -e '.[0].IPAM.Config[0] |
  .Subnet == "198.18.81.0/24" and .IPRange == "198.18.81.128/25"' >/dev/null
docker inspect "$(compose ps -q gateway)" | jq -e --arg network "${project}_application" \
  '.[0].NetworkSettings.Networks[$network].IPAddress == "198.18.81.2"' >/dev/null
for service in control-plane transportd; do
  docker inspect "$(compose ps -q "${service}")" | jq -e --arg network "${project}_application" '
    .[0].NetworkSettings.Networks[$network].IPAddress | split(".") |
    .[0:3] == ["198", "18", "81"] and (.[3] | tonumber) >= 128' >/dev/null
done
docker inspect "$(compose ps -q control-plane)" | jq -e \
  '.[0].Config.Env | index("OCSERV_AUTH_TRUSTED_PROXY_CIDRS=198.18.81.2/32") != null' >/dev/null
cmp "${fixture}/release/0.5.0.json" "${fixture}/state/current-release.json"
cmp "${fixture}/release/0.4.0.json" "${fixture}/state/previous-release.json"
test ! -e "${fixture}/state/pending-release.json"
if controller rollback >"${fixture}/output.log" 2>&1; then echo 'expected contract rollback refusal' >&2; exit 1; fi
grep -F 'production deployment descriptor changed' "${fixture}/output.log"

# Later upgrades with the target pool must leave the existing network in place.
controller upgrade --release-file "${fixture}/release/0.5.1.json"
[[ "$(docker network inspect "${project}_application" --format '{{.Id}}')" == "${network}" ]]
assert_database
echo 'Controller legacy application network upgrade, retry, preservation and rollback guard tests passed'
