#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fixture="$(mktemp -d)"
project="ocservia-proxy-network-$$"
image="${CONTROLLER_PROXY_TEST_IMAGE:-caddy:2.10.2-alpine@sha256:4c6e91c6ed0e2fa03efd5b44747b625fec79bc9cd06ac5235a779726618e530d}"
compose() { docker compose -p "${project}" -f "${fixture}/compose.json" "$@"; }
cleanup() {
  compose down --remove-orphans >/dev/null 2>&1 || true
  rm -rf -- "${fixture}"
}
trap cleanup EXIT

# Exercise the production network/address expressions without starting the
# production services or touching their secrets, ports, volumes or networks.
docker compose -f "${ROOT}/deploy/production/compose.yaml" config \
  --no-interpolate --no-env-resolution --format json | jq --arg image "${image}" '{
  services: {
    gateway: {image: $image, entrypoint: ["sh", "-c", "sleep 300"],
      networks: {application: .services.gateway.networks.application}},
    observer: {image: $image, entrypoint: ["sh", "-c", "sleep 300"],
      networks: {application: null}, environment: {
        OCSERV_AUTH_TRUSTED_PROXY_CIDRS: .services["control-plane"].environment.OCSERV_AUTH_TRUSTED_PROXY_CIDRS}}
  }, networks: {application: (.networks.application | del(.name))}
}' >"${fixture}/compose.json"

unset OCSERV_APPLICATION_SUBNET OCSERV_APPLICATION_IP_RANGE OCSERV_GATEWAY_APPLICATION_IP OCSERV_AUTH_TRUSTED_PROXY_CIDRS
compose config --format json | jq -e '
  .services.gateway.networks.application.ipv4_address == "172.30.240.2" and
  .services.observer.environment.OCSERV_AUTH_TRUSTED_PROXY_CIDRS == "172.30.240.2/32" and
  .networks.application.internal == true and
  .networks.application.ipam.config[0].subnet == "172.30.240.0/24" and
  .networks.application.ipam.config[0].ip_range == "172.30.240.128/25"
' >/dev/null

export OCSERV_APPLICATION_SUBNET=198.18.80.0/24
export OCSERV_APPLICATION_IP_RANGE=198.18.80.128/25
export OCSERV_GATEWAY_APPLICATION_IP=198.18.80.2
compose config --format json | jq -e '
  .services.gateway.networks.application.ipv4_address == "198.18.80.2" and
  .services.observer.environment.OCSERV_AUTH_TRUSTED_PROXY_CIDRS == "198.18.80.2/32" and
  .networks.application.ipam.config[0].subnet == "198.18.80.0/24" and
  .networks.application.ipam.config[0].ip_range == "198.18.80.128/25"
' >/dev/null
OCSERV_AUTH_TRUSTED_PROXY_CIDRS=203.0.113.9/32 compose config --format json | jq -e \
  '.services.observer.environment.OCSERV_AUTH_TRUSTED_PROXY_CIDRS == "203.0.113.9/32"' >/dev/null
OCSERV_AUTH_TRUSTED_PROXY_CIDRS='' compose config --format json | jq -e \
  '.services.observer.environment.OCSERV_AUTH_TRUSTED_PROXY_CIDRS == ""' >/dev/null

address() {
  docker inspect "$(compose ps -q "$1")" | jq -r '.[0].NetworkSettings.Networks[].IPAddress'
}
# The Controller starts before gateway in production. A dynamic endpoint must
# not claim the reserved static address during that interval.
compose up -d observer
[[ "$(address observer)" == 198.18.80.* && "$(address observer)" != "${OCSERV_GATEWAY_APPLICATION_IP}" ]]
compose up -d gateway
before="$(compose ps -q gateway)"
[[ "$(address gateway)" == "${OCSERV_GATEWAY_APPLICATION_IP}" ]]
compose up -d --force-recreate gateway
[[ "$(compose ps -q gateway)" != "${before}" ]]
[[ "$(address gateway)" == "${OCSERV_GATEWAY_APPLICATION_IP}" ]]
docker inspect "$(compose ps -q observer)" | jq -e --arg cidr "${OCSERV_GATEWAY_APPLICATION_IP}/32" \
  '.[0].Config.Env | index("OCSERV_AUTH_TRUSTED_PROXY_CIDRS=" + $cidr) != null' >/dev/null
echo "Controller proxy network configuration and recreation tests passed"
