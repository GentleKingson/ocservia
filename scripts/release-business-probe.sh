#!/usr/bin/env bash
# Disposable hosted runner only. Smoke and extended profiles retain separate scope.
# shellcheck disable=SC2024 # sudo reads protected state; redirects are runner-owned.
set -Eeuo pipefail
# Imported acceptance helpers must not dirty the exact-source lifecycle checkout.
export PYTHONDONTWRITEBYTECODE=1
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
: "${ARTIFACT_DIR:?}" "${VERSION:?}"
SOURCE_COMMIT="$(git -C "$ROOT" rev-parse HEAD)"
export SOURCE_COMMIT
# shellcheck source=scripts/env.sh
# shellcheck disable=SC1091
source "$ROOT/scripts/env.sh"
: "${BUSINESS_TERMINAL:=revocation}"
export BUSINESS_TERMINAL
[[ "$BUSINESS_TERMINAL" == revocation || "$BUSINESS_TERMINAL" == uninstall ]]
: "${BUSINESS_PROFILE:=smoke}"
export BUSINESS_PROFILE
: "${PRODUCTION_SIGNER_ACCEPTANCE:=false}"
: "${BUSINESS_RUN_RESILIENCE:=false}"
[[ "$BUSINESS_RUN_RESILIENCE" == true || "$BUSINESS_RUN_RESILIENCE" == false ]]
resilience_result=SKIPPED
if [[ "$BUSINESS_RUN_RESILIENCE" == true ]]; then
  resilience_result=FAIL
fi
relay_ipv4_rule=false
relay_ipv6_rule=false
[[ "${BUSINESS_PROFILE}" == smoke || "${BUSINESS_PROFILE}" == extended ]]
bash "$ROOT/scripts/release-business-environment.sh"
[[ "${VERSION}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ && "${SOURCE_COMMIT}" =~ ^[0-9a-f]{40}$ ]]
[[ -z "$(git -C "${ROOT}" status --porcelain)" && "$(ps -p 1 -o comm=)" == systemd ]]
[[ ! -e /etc/ocservia-agent && ! -e /usr/libexec/ocservia && ! -e /etc/ocservia ]]
if getent passwd ocserv-agent >/dev/null; then
  echo 'refusing an existing Agent account' >&2
  exit 2
fi
[[ -z "$(docker ps -aq --filter label=com.docker.compose.project=ocservia-production)" ]]
[[ -z "$(docker volume ls -q --filter label=com.docker.compose.project=ocservia-production)" ]]
umask 077
mkdir -m 700 "${ARTIFACT_DIR}"
export OCSERVIA_BUSINESS_TEST_WORK_DIR
OCSERVIA_BUSINESS_TEST_WORK_DIR="$(mktemp -d "${HOME}/.ocservia-business-test-XXXXXX")"
work="${OCSERVIA_BUSINESS_TEST_WORK_DIR}"
stage=preflight
started="$(date -u +%FT%TZ)"
stage_started="$(date +%s)"
CONTROLLER_ARCH="${CONTROLLER_ARCH:-amd64}"
[[ "${CONTROLLER_ARCH}" == amd64 || "${CONTROLLER_ARCH}" == arm64 ]]
export PACKAGE_ARCH="${CONTROLLER_ARCH}" CONTROLLER_ARCH
export SOURCE_DATE_EPOCH OUTPUT_DIR="${PRODUCTS_DIR:-${work}/products}"
IMAGES_DIR="${IMAGES_DIR:-}"
# Artifact mode is all-or-nothing; missing inputs never fall back to a build.
if [[ -n "${PRODUCTS_DIR:-}${IMAGES_DIR}" ]]; then
  [[ -n "${PRODUCTS_DIR:-}" && -n "$IMAGES_DIR" ]]
  [[ -s "$PRODUCTS_DIR/product-identity.json" && -s "$IMAGES_DIR/product-identity.json" ]]
fi
export DEPLOYMENT_CONFIG_DIR="${work}/bundle"
SOURCE_DATE_EPOCH="$(git -C "${ROOT}" log -1 --format=%ct)"
export BUILDX_BUILDER="business-${GITHUB_RUN_ID:?}-${GITHUB_RUN_ATTEMPT:?}"
registry="${BUILDX_BUILDER}-registry"
export OCSERVIA_BUSINESS_TEST_RELAY_CONTAINER="${BUILDX_BUILDER}-relay"
signer_net_container="${BUILDX_BUILDER}-signer-net"
export OCSERVIA_BUSINESS_TEST_SIGNER_NET_CONTAINER="$signer_net_container" OCSERVIA_BUSINESS_TEST_SIGNER_CONTAINER="${BUILDX_BUILDER}-signer"
signer_pid=
export OCSERV_SECRET_DIR="${work}/secrets" OCSERV_BACKUP_DIR="${work}/backup"
export OCSERV_CONTROLLER_STATE_ROOT="${work}/state"
export OCSERV_PUBLIC_HOST=localhost OCSERV_HTTPS_ADDRESS=127.0.0.1
export OCSERV_CONTROLLER_PUBLIC_URL=https://localhost OCSERV_PUBLIC_ORIGIN=https://localhost
export OCSERV_LOCAL_AUTH_ENABLED=true OCSERV_AUDIT_EVENT_KEY_ID=business-test OCSERV_BACKUP_INTERVAL_SECONDS=86400
export OCSERV_CERTIFICATE_SIGNER_URL=https://signer.unavailable.invalid
if [[ "$PRODUCTION_SIGNER_ACCEPTANCE" == true ]]; then
  [[ "$EUID" == 0 ]]
  gateway="$(docker network inspect bridge --format '{{(index .IPAM.Config 0).Gateway}}')"
  if [[ -n "${INTEGRATED_PUBLIC_ADDRESS:-}" ]]; then
    [[ "${INTEGRATED_DISPOSABLE_CONTAINER:-}" == true ]]
    python3 -c 'import ipaddress,sys; assert ipaddress.ip_address(sys.argv[1]).version == 4 and ipaddress.ip_address(sys.argv[1]).is_global' "$INTEGRATED_PUBLIC_ADDRESS"
    gateway="$INTEGRATED_PUBLIC_ADDRESS"
  fi
  export OCSERV_DEPLOYMENT_MODE=integrated OCSERV_HTTPS_ADDRESS=0.0.0.0
  export OCSERV_PUBLIC_HOST="controller.${gateway}.sslip.io" OCSERV_RELAY_PUBLIC_HOST="relay.${gateway}.sslip.io"
  export OCSERV_CONTROLLER_PUBLIC_URL="https://${OCSERV_PUBLIC_HOST}" OCSERV_PUBLIC_ORIGIN="https://${OCSERV_PUBLIC_HOST}"
  export OCSERV_RELAY_URL_A="https://${OCSERV_RELAY_PUBLIC_HOST}"
  export OCSERV_CERTIFICATE_SIGNER_URL=https://signer:9443/sign
  export OCSERV_RELAY_SECRET_DIR="${work}/relay-secrets"
  export OCSERV_SIGNER_SECRET_DIR="${work}/production-signer/secrets" OCSERV_SIGNER_STATE_DIR="${work}/production-signer/data"
fi
unset OCSERV_OTEL_BACKEND_ENDPOINT
unset OCSERV_CONTROLLER_COMPOSE_SH OCSERV_CONTROLLER_SMOKE_SH OCSERV_MANAGED_NODE_SYSROOT OCSERV_MANAGED_NODE_OS_RELEASE
compose() { "${ROOT}/deploy/production/compose.sh" "$@"; }
record() { printf '%s\n' "$1" >>"${ARTIFACT_DIR}/checkpoints.txt"; }
next_stage() {
  local now
  now="$(date +%s)"
  printf '%s\t%s\n' "${stage}" "$((now - stage_started))" >>"${ARTIFACT_DIR}/timings.tsv"
  stage="$1"
  stage_started="${now}"
}
cleanup() {
  local code=$?
  trap - EXIT ERR
  set +e
  local failed_stage="${stage}"
  next_stage cleanup
  jq -Rn '[inputs | split("\t") | {stage:.[0],seconds:(.[1]|tonumber)}]' \
    <"${ARTIFACT_DIR}/timings.tsv" >"${ARTIFACT_DIR}/timings.json"
  if [[ -f "${work}/private.log" ]]; then
    {
      compose logs --no-color
      for service in control-plane transportd; do
        container="$(compose ps -a -q "${service}")"
        if [[ -n "${container}" ]]; then
          docker inspect --format '{{json .State}}' "${container}"
        fi
      done
      sudo journalctl --no-pager -o short-iso-precise -u ocservia-agent -u ocservia-privd -u ocserv
      if [[ "$PRODUCTION_SIGNER_ACCEPTANCE" == true ]]; then
        docker logs "$OCSERVIA_BUSINESS_TEST_SIGNER_CONTAINER"
        sudo journalctl --no-pager -o short-iso-precise -u ocservia-business-test-crl
      fi
    } >>"${work}/private.log" 2>&1
  fi
  # Never export environment, cookies, raw databases, passwords or raw logs.
  if [[ -f "${work}/private.log" ]]; then
    sudo "$(command -v node)" "${ROOT}/scripts/redact-release-upgrade-log.mjs" \
      "${work}/private.log" "${work}/private" "${work}/sanitized.log" &&
      sudo install -o "$(id -u)" -g "$(id -g)" -m 600 "${work}/sanitized.log" "${ARTIFACT_DIR}/probe.log"
  fi
  [[ -f "${ARTIFACT_DIR}/checkpoints.txt" ]] || : >"${ARTIFACT_DIR}/checkpoints.txt"
  jq -n --arg version "${VERSION}" --arg start "${started}" \
    --arg end "$(date -u +%FT%TZ)" --arg stage "${failed_stage}" --argjson code "${code}" \
    --arg source "$SOURCE_COMMIT" --arg profile "${BUSINESS_PROFILE}" --arg terminal "$BUSINESS_TERMINAL" \
    --arg ocserv_fixture "${OCSERV_PACKAGE:-distribution}" \
    --arg resilience "$resilience_result" --argjson resilience_requested "$BUSINESS_RUN_RESILIENCE" \
    --argjson resilience_scenarios "$(if [[ -f "${ARTIFACT_DIR}/api-checkpoints.jsonl" ]]; then jq -s '[.[] | select(.name | startswith("resilience_")) | {name,status,time}]' "${ARTIFACT_DIR}/api-checkpoints.jsonl"; else printf '[]'; fi)" \
    --arg arch "$CONTROLLER_ARCH" \
    --argjson disposable_container "${INTEGRATED_DISPOSABLE_CONTAINER:-false}" \
    --arg run "${GITHUB_RUN_ID}" --arg attempt "${GITHUB_RUN_ATTEMPT}" \
    --rawfile checkpoints "${ARTIFACT_DIR}/checkpoints.txt" \
    --slurpfile timings "${ARTIFACT_DIR}/timings.json" \
    '{source_commit:$source,version:$version,run_id:$run,run_attempt:$attempt,profile:$profile,terminal:$terminal,
      ocserv_fixture:$ocserv_fixture,
      execution_environment:(if $disposable_container then "BuildServer isolated Docker/systemd" else "GitHub-hosted runner" end),
      started_at:$start,finished_at:$end,exit_code:$code,last_stage:$stage,
      timings:$timings[0],passed_checkpoints:($checkpoints | split("\n") | map(select(length > 0))),
      probe_status:(if $code == 0 then "PASS" else "FAIL" end),
      scope:(if $profile == "smoke" then "business-smoke" else "integration" end),
      resilience_requested:$resilience_requested,
      resilience_result:(if $code != 0 and $resilience == "PASS" then "FAIL" else $resilience end),
      resilience_scenarios:$resilience_scenarios,
      planned_topology:{hosts:1,architecture:$arch,native_systemd_node:true,relays:1,relay_redundancy:false},
      operator_mode:"single_admin",
      limitations:[],
      not_applicable:["cross-host resilience and performance assessment"]}' >"${ARTIFACT_DIR}/result.json"
  sudo systemctl stop ocservia-agent ocservia-privd ocserv >/dev/null 2>&1
  if [[ "$relay_ipv4_rule" == true ]]; then
    sudo iptables -D OUTPUT -m owner --uid-owner "$(id -u ocserv-agent)" -p udp ! --dport 53 -j REJECT
  fi
  if [[ "$relay_ipv6_rule" == true ]]; then
    sudo ip6tables -D OUTPUT -m owner --uid-owner "$(id -u ocserv-agent)" -p udp ! --dport 53 -j REJECT
  fi
  if [[ "$PRODUCTION_SIGNER_ACCEPTANCE" == true ]]; then
    sudo systemctl stop ocservia-business-test-crl >/dev/null 2>&1
    docker rm -f "$OCSERVIA_BUSINESS_TEST_SIGNER_CONTAINER" >/dev/null 2>&1
  fi
  sudo ip netns pids business-test-client 2>/dev/null | xargs -r sudo kill
  sudo ip netns del business-test-client >/dev/null 2>&1
  compose down --volumes --remove-orphans >/dev/null 2>&1
  docker rm -f "${registry}" "${OCSERVIA_BUSINESS_TEST_RELAY_CONTAINER}" "${signer_net_container}" >/dev/null 2>&1
  if [[ -n "${signer_pid}" ]]; then kill "${signer_pid}" 2>/dev/null; wait "${signer_pid}" 2>/dev/null; fi
  docker buildx rm "${BUILDX_BUILDER}" >/dev/null 2>&1
  # Host installation is confined to this ephemeral GitHub runner, destroyed
  # by Actions after the job. Remove this task's private material now.
  sudo rm -rf -- "${work}"
  exit "${code}"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'printf "probe failed at line %s, stage %s\n" "${LINENO}" "${stage}" >&2' ERR
mkdir -m 700 "${work}/private" "${work}/secrets" "${work}/state" "${work}/backup" "${work}/bundle"
bash "${ROOT}/scripts/release-upgrade-native.sh" "$CONTROLLER_ARCH" >"${ARTIFACT_DIR}/native.json"
{ uname -a; cat /etc/os-release; docker version; docker compose version; node --version; } >"${ARTIFACT_DIR}/environment.txt"
next_stage dependency_setup
bash "${ROOT}/scripts/bootstrap.sh" npm-security
if [[ -z "${PRODUCTS_DIR:-}" ]]; then
  bash "${ROOT}/scripts/bootstrap.sh" native-packages
  # The native builder drops capabilities and needs root-owned task caches.
  if [[ "$EUID" == 0 ]]; then
    mkdir -p "$ROOT/rust/target"
    chown -R 0:0 "$ROOT/.cache" "$ROOT/.tools" "$ROOT/rust/target"
  fi
  next_stage agent_package_build
  env -u BUILDX_BUILDER bash "${ROOT}/scripts/build-release-agent.sh" >"${ARTIFACT_DIR}/agent-build.log" 2>&1
  next_stage controller_image_build
  bash "${ROOT}/scripts/build-release-controller.sh" >"${ARTIFACT_DIR}/controller-build.log" 2>&1
  IMAGES_DIR="$OUTPUT_DIR"
else
  next_stage product_consumption
  cp "$PRODUCTS_DIR/product-identity.json" "$ARTIFACT_DIR/agent-product-identity.json"
  cp "$IMAGES_DIR/product-identity.json" "$ARTIFACT_DIR/controller-product-identity.json"
fi
for name in gateway control transport backup edge relay signer; do
  docker load -i "$IMAGES_DIR/$name-linux-$CONTROLLER_ARCH.tar"
  # Unrelated binfmt handlers could otherwise run a foreign image emulated.
  [[ "$(docker image inspect --format '{{.Os}}/{{.Architecture}}' "ghcr.io/gentlekingson/ocservia/$name:$VERSION-linux-$CONTROLLER_ARCH")" == "linux/$CONTROLLER_ARCH" ]]
done
export OCSERVIA_BUSINESS_TEST_SIGNER_IMAGE="ghcr.io/gentlekingson/ocservia/signer:$VERSION-linux-$CONTROLLER_ARCH"
docker tag "ghcr.io/gentlekingson/ocservia/relay:$VERSION-linux-$CONTROLLER_ARCH" "${BUILDX_BUILDER}-relay"
docker run --rm --entrypoint /usr/local/bin/iroh-relay "${BUILDX_BUILDER}-relay" --version >"${ARTIFACT_DIR}/relay-version.txt"
record products_ready
next_stage dependency_install
sudo apt-get update -qq
case "${OCSERV_PACKAGE:-distribution}" in
  distribution) ocserv_package=ocserv ;;
  backport-1.5.0)
    # The pinned ocserv-backport release asset brings `occtl terminate` (>= 1.4.2).
    ocserv_version=1.5.0-1~ubuntu24.04.2
    case "${CONTROLLER_ARCH}" in
      amd64) ocserv_sha256=5a15dd005267d5a20a249f2e97417c36464a02cb03a47b4220f51a100302fc89 ;;
      arm64) ocserv_sha256=33f64b7464a8f47420952a883b287c4fce7d4df430412487b85d879ae7bfb173 ;;
    esac
    ocserv_package="${work}/ocserv_1.5.0-1.ubuntu24.04.2_${CONTROLLER_ARCH}.deb"
    curl -fsSL --retry 3 -o "${ocserv_package}" \
      "https://github.com/GentleKingson/ocserv-backport/releases/download/1.5.0-2/ocserv_1.5.0-1.ubuntu24.04.2_${CONTROLLER_ARCH}.deb"
    echo "${ocserv_sha256}  ${ocserv_package}" | sha256sum -c --strict -
    [[ "$(dpkg-deb -f "${ocserv_package}" Package Version Architecture)" == \
      "$(printf 'Package: ocserv\nVersion: %s\nArchitecture: %s' "${ocserv_version}" "${CONTROLLER_ARCH}")" ]]
    ;;
  *) echo 'OCSERV_PACKAGE must be distribution or backport-1.5.0' >&2; exit 2 ;;
esac
sudo apt-get install -y --no-install-recommends "${ocserv_package}" openconnect vpnc-scripts sqlite3 iputils-ping
sudo systemctl stop ocserv
if [[ "${OCSERV_PACKAGE:-distribution}" == backport-1.5.0 ]]; then
  [[ "$(dpkg-query -W -f='${Version}' ocserv)" == "${ocserv_version}" ]]
  [[ "$(ocserv --version 2>&1 | head -n1)" == 'OpenConnect VPN Server 1.5.0' ]]
  record ocserv_backport_package
fi
{ dpkg-query -W ocserv openconnect libgnutls30t64 openssl systemd; ocserv --version; } >>"${ARTIFACT_DIR}/environment.txt" 2>&1
# Subsequent output can contain task-only secret data. Keep it private until
# the exact-value redactor runs; the uploaded results never contain cookies.
exec >"${work}/private.log" 2>&1
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=business-test-ca \
  -addext basicConstraints=critical,CA:TRUE -addext keyUsage=critical,keyCertSign,cRLSign \
  -keyout "${work}/private/ca.key" -out "${work}/ca.crt"
chmod 444 "${work}/ca.crt"
gateway="$(docker network inspect bridge --format '{{(index .IPAM.Config 0).Gateway}}')"
openssl req -new -newkey rsa:2048 -nodes -subj /CN=localhost \
  -keyout "${work}/private/tls.key" -out "${work}/tls.csr"
printf 'basicConstraints=critical,CA:FALSE\nsubjectAltName=DNS:localhost,DNS:%s,IP:127.0.0.1,IP:%s,IP:10.207.0.1,IP:172.30.240.1,IP:172.30.240.3\nextendedKeyUsage=serverAuth\n' "$OCSERV_PUBLIC_HOST" "${gateway}" >"${work}/leaf.ext"
openssl x509 -req -days 1 -in "${work}/tls.csr" -CA "${work}/ca.crt" -CAkey "${work}/private/ca.key" \
  -CAcreateserial -extfile "${work}/leaf.ext" -out "${OCSERV_SECRET_DIR}/tls.crt"
cp "${work}/private/tls.key" "${OCSERV_SECRET_DIR}/tls.key"
export CURL_CA_BUNDLE="${work}/ca.crt"
if [[ "$PRODUCTION_SIGNER_ACCEPTANCE" != true ]]; then
  export OCSERV_RELAY_URL_A="https://${gateway}:3443"
fi
for name in postgres-owner-password postgres-app-password postgres-backup-password session-key audit-checkpoint-key \
  audit-event-key certificate-signer-token relay-access-token; do
  openssl rand -hex 32 >"${work}/private/${name}"
  cp "${work}/private/${name}" "${OCSERV_SECRET_DIR}/${name}"
done
openssl genpkey -algorithm ED25519 -out "${work}/private/controller-command-signing-key.pem"
cp "${work}/private/controller-command-signing-key.pem" "${OCSERV_SECRET_DIR}/controller-command-signing-key.pem"
openssl pkey -in "${work}/private/controller-command-signing-key.pem" -pubout -out "${work}/command.pub.pem"
openssl pkey -in "${work}/private/controller-command-signing-key.pem" -outform DER | tail -c 32 | \
  od -An -v -tx1 | tr -d ' \n' >"${OCSERV_SECRET_DIR}/controller-iroh.key"
OCSERV_CONTROLLER_ENDPOINT_ID="$(openssl pkey -pubin -in "${work}/command.pub.pem" -outform DER | tail -c 32 | od -An -v -tx1 | tr -d ' \n')"
export OCSERV_CONTROLLER_ENDPOINT_ID
for role in owner app; do
  printf 'postgres://ocservia_%s:%s@postgres:5432/ocservia?sslmode=disable\n' "${role}" \
    "$(cat "${work}/private/postgres-${role}-password")" >"${OCSERV_SECRET_DIR}/database-${role}-url"
done
printf 'postgres:5432:*:ocservia_backup:%s\n' "$(cat "${work}/private/postgres-backup-password")" >"${OCSERV_SECRET_DIR}/postgres.pgpass"
chmod 444 "${OCSERV_SECRET_DIR}/"*
sudo chown 65534:65532 "${OCSERV_SECRET_DIR}/audit-event-key" "${OCSERV_SECRET_DIR}/controller-command-signing-key.pem"
sudo chown 65532:65532 "${OCSERV_SECRET_DIR}/controller-iroh.key" "${OCSERV_SECRET_DIR}/relay-access-token"
sudo chmod 400 "${OCSERV_SECRET_DIR}/audit-event-key" "${OCSERV_SECRET_DIR}/controller-command-signing-key.pem" \
  "${OCSERV_SECRET_DIR}/controller-iroh.key"
sudo chmod 440 "${OCSERV_SECRET_DIR}/relay-access-token"
sudo install -o root -g 65532 -m 440 "${work}/command.pub.pem" "${OCSERV_SECRET_DIR}/controller-command-verification-key.pem"
sudo install -o root -g root -m 444 "${work}/ca.crt" "${OCSERV_SECRET_DIR}/relay-ca.pem"
sudo chown 999:999 "${work}/backup"
registry_prefix=localhost:5000
registry_tag="v$VERSION"
if [[ "$PRODUCTION_SIGNER_ACCEPTANCE" == true ]]; then
  mkdir -m 700 "$OCSERV_RELAY_SECRET_DIR"
  openssl req -new -newkey rsa:2048 -nodes -subj "/CN=$OCSERV_RELAY_PUBLIC_HOST" \
    -keyout "$work/private/relay-tls.key" -out "$work/relay.csr"
  printf 'subjectAltName=DNS:%s\nextendedKeyUsage=serverAuth\n' "$OCSERV_RELAY_PUBLIC_HOST" >"$work/relay.ext"
  openssl x509 -req -days 1 -in "$work/relay.csr" -CA "$work/ca.crt" -CAkey "$work/private/ca.key" \
    -CAcreateserial -extfile "$work/relay.ext" -out "$OCSERV_RELAY_SECRET_DIR/tls.crt"
  install -m 444 "$work/private/relay-tls.key" "$OCSERV_RELAY_SECRET_DIR/tls.key"
  chmod 444 "$OCSERV_RELAY_SECRET_DIR/tls.crt"
  python3 "$ROOT/scripts/release-production-signer.py" prepare
fi
docker run -d --name "${registry}" -p 127.0.0.1:5000:5000 registry:2
args=()
initial_args=()
roles=(gateway control transport backup)
manifest_options=()
if [[ "$PRODUCTION_SIGNER_ACCEPTANCE" == true ]]; then
  roles+=(edge relay signer)
  manifest_options+=(--manifest-version 2)
fi
for name in "${roles[@]}"; do
  target="${registry_prefix}/${name}:${registry_tag}"
  docker tag "ghcr.io/gentlekingson/ocservia/${name}:${VERSION}-linux-${CONTROLLER_ARCH}" "${target}"
  docker push "${target}"
  ref="$(docker image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' "${target}" |
    grep "^${registry_prefix}/${name}@sha256:")"
  args+=(--image "${name}=${ref}")
  export "OCSERV_${name^^}_IMAGE=${ref}"
  if [[ "$PRODUCTION_SIGNER_ACCEPTANCE" == true && "$BUSINESS_PROFILE" == extended ]]; then
    initial_target="${registry_prefix}/initial-${name}:${registry_tag}"
    docker tag "ghcr.io/gentlekingson/ocservia/${name}:${VERSION}-linux-${CONTROLLER_ARCH}" "${initial_target}"
    docker push "${initial_target}"
    initial_ref="$(docker image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' "${initial_target}" |
      grep "^${registry_prefix}/initial-${name}@sha256:")"
    initial_args+=(--image "${name}=${initial_ref}")
  fi
done
export OCSERVIA_BUSINESS_TEST_RELAY_IMAGE="${OCSERV_RELAY_IMAGE:-${BUILDX_BUILDER}-relay}"
export OCSERVIA_BUSINESS_TEST_SIGNER_IMAGE="${OCSERV_SIGNER_IMAGE:-$OCSERVIA_BUSINESS_TEST_SIGNER_IMAGE}"
# Existing third-party runtime dependency versions.
export OCSERV_POSTGRES_IMAGE OCSERV_OTEL_IMAGE
OCSERV_POSTGRES_IMAGE=docker.io/library/postgres@sha256:1c59e2c3c818eaa0f0628f695b36e7c9e362d6b219b36a54a32df645cbd7e1af
OCSERV_OTEL_IMAGE=docker.io/otel/opentelemetry-collector@sha256:0c066d4388070dad8dc9961d9f23649e85a226620e6b359334e4a6c7f9d73b23
manifest="${DEPLOYMENT_CONFIG_DIR}/controller-release-${CONTROLLER_ARCH}.json"
node "${ROOT}/scripts/generate-controller-release-manifest.mjs" --output "${manifest}" \
  --release-version "${VERSION}" --release-tag "v${VERSION}" --source-commit "${SOURCE_COMMIT}" \
  --migration-dir "${ROOT}/control-plane/migrations" --platform "linux/${CONTROLLER_ARCH}" \
  "${manifest_options[@]}" "${args[@]}" --image "postgres=${OCSERV_POSTGRES_IMAGE}" --image "otel=${OCSERV_OTEL_IMAGE}"
next_stage controller_install
if [[ "$PRODUCTION_SIGNER_ACCEPTANCE" == true && "$BUSINESS_PROFILE" == extended ]]; then
  # Exercise a real configuration upgrade between two digest-pinned
  # repositories for the same built images. This is not historical compatibility.
  initial_config="${work}/initial-deployment.json"
  node "${ROOT}/scripts/generate-controller-release-manifest.mjs" --output "${initial_config}" \
    --release-version "${VERSION}" --release-tag "v${VERSION}" --source-commit "${SOURCE_COMMIT}" \
    --migration-dir "${ROOT}/control-plane/migrations" --platform "linux/${CONTROLLER_ARCH}" \
    "${manifest_options[@]}" "${initial_args[@]}" --image "postgres=${OCSERV_POSTGRES_IMAGE}" --image "otel=${OCSERV_OTEL_IMAGE}"
  "${ROOT}/deploy/production/controller.sh" install --release-file "$initial_config"
  python3 "$ROOT/scripts/release-integrated-acceptance.py" rejected_upgrade
  "${ROOT}/deploy/production/controller.sh" upgrade --release-file "$manifest"
  record integrated_first_install_and_configuration_upgrade
else
  "${ROOT}/deploy/production/controller.sh" install --release-file "$manifest"
fi
if [[ "$PRODUCTION_SIGNER_ACCEPTANCE" == true ]]; then
  OCSERVIA_BUSINESS_TEST_SIGNER_CONTAINER="$(compose ps -q signer)"
  OCSERVIA_BUSINESS_TEST_RELAY_CONTAINER="$(compose ps -q relay)"
fi
curl --fail --silent --show-error "$OCSERV_CONTROLLER_PUBLIC_URL/api/v1/version" | \
  jq -e --arg sha "${SOURCE_COMMIT}" --arg v "${VERSION}" '.commit == $sha and .version == $v' >/dev/null
record controller_lifecycle
if [[ "$PRODUCTION_SIGNER_ACCEPTANCE" == true ]]; then
  python3 "$ROOT/scripts/release-integrated-acceptance.py" entry
  record integrated_entry_and_internal_tls
fi
export OCSERVIA_BUSINESS_TEST_WORKSPACE_ID=00000000-0000-7000-8000-000000000071
compose exec -T postgres psql -XAt -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia <<SQL
INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES
 ('${OCSERVIA_BUSINESS_TEST_WORKSPACE_ID}','Business Test','business-test',now(),now()),
 ('00000000-0000-7000-8000-000000000072','Other','business-test-other',now(),now());
SQL
openssl rand -hex 32 >"${work}/private/requester-password"
sudo install -o 65534 -g 65532 -m 400 "${work}/private/requester-password" "${OCSERV_SECRET_DIR}/requester-password"
compose run --rm --no-deps -e OCSERV_LOCAL_BOOTSTRAP_USERNAME=business-test-requester \
  -e "OCSERV_LOCAL_BOOTSTRAP_WORKSPACE_ID=${OCSERVIA_BUSINESS_TEST_WORKSPACE_ID}" \
  -e OCSERV_LOCAL_BOOTSTRAP_PASSWORD_FILE=/run/secrets/requester-password \
  -v "${OCSERV_SECRET_DIR}/requester-password:/run/secrets/requester-password:ro" \
  control-plane --bootstrap-local-admin
next_stage local_auth
python3 "${ROOT}/scripts/release-business-api.py" local
record real_local_auth
if [[ "${BUSINESS_PROFILE}" == extended ]]; then
  if [[ "$PRODUCTION_SIGNER_ACCEPTANCE" != true ]]; then
    signer_address=172.30.240.3
    export OCSERV_CERTIFICATE_SIGNER_URL="https://${signer_address}:19444/sign"
    docker run -d --name "${signer_net_container}" --read-only --cap-drop ALL --security-opt no-new-privileges:true \
      --network ocservia-production_application --ip "${signer_address}" \
      node:24.18.1-bookworm-slim@sha256:235600a8101ab264e117b1768e925532262668dc9b581ef1dd7d96ced463b8e7 \
      sleep infinity
    # The internal application network has no host gateway; run the mock signer
    # inside its dedicated network container when testing without production Signer.
    provider_pid="$(docker inspect --format '{{.State.Pid}}' "${signer_net_container}")"
    sudo nsenter --target "${provider_pid}" --net -- setpriv --reuid="$(id -u)" --regid="$(id -g)" --clear-groups \
      python3 "${ROOT}/scripts/release-business-signer.py" "${work}" "${signer_address}" &
    signer_pid=$!
    # --no-deps makes ordering our responsibility. The launcher stops both peers;
    # wait for the trust backend before starting the unchanged transport container.
    compose up -d --no-deps --wait control-plane
    compose start transportd
    python3 "${ROOT}/scripts/release-business-api.py" transport_ready
    python3 "${ROOT}/scripts/release-business-api.py" trust_controller
  else
    python3 "${ROOT}/scripts/release-business-api.py" transport_ready
  fi
  compose exec -T postgres psql -XAt -U ocservia_owner -d ocservia -c 'SHOW server_version' >>"${ARTIFACT_DIR}/environment.txt"
else
  python3 "${ROOT}/scripts/release-business-api.py" transport_ready
fi
next_stage native_node
# Install the locally built native package and exercise official node
# convergence/preparation without publishing any Release or tag.
deb="${OUTPUT_DIR}/ocservia-agent_${VERSION}-1_${CONTROLLER_ARCH}.deb"
sudo install -d -m 755 /etc/ocservia
sudo touch /etc/ocservia/agent-install-production-relays
sudo dpkg -i "${deb}"
sudo install -o root -g root -m 444 "${work}/ca.crt" /etc/ocservia-agent/relay-ca.pem
export CONTROLLER_ENDPOINT_ID="${OCSERV_CONTROLLER_ENDPOINT_ID}" RELAY_URL_A="${OCSERV_RELAY_URL_A}"
export RELAY_ACCESS_TOKEN_SOURCE="${OCSERV_SECRET_DIR}/relay-access-token"
export CONTROLLER_COMMAND_VERIFICATION_KEY_SOURCE="${work}/command.pub.pem"
export USER_PASSWORD_SEAL_KEY_ID=business-test-user-seal P12_PASSWORD_SEAL_KEY_ID=business-test-p12-seal ENROLLMENT_ENVIRONMENT=production
managed_options=(--version "v${VERSION}")
if [[ "$PRODUCTION_SIGNER_ACCEPTANCE" == true ]]; then managed_options+=(--root-lifecycle); fi
# The managed installer preflight reads the real native ocserv resources.
# Prepare and start this task-owned VPN fixture before enrollment, then bind
# its TLS material to the enrolled node through the existing ConfigPlan path.
sudo install -m 600 "${work}/private/tls.key" /etc/ocserv/business-test.key
sudo install -m 644 "${OCSERV_SECRET_DIR}/tls.crt" /etc/ocserv/business-test.crt
sudo install -m 600 /dev/null /etc/ocserv/ocpasswd
sudo groupadd --system ocservia-vpn
sudo useradd --system --no-create-home --gid ocservia-vpn --shell /usr/sbin/nologin ocservia-vpn
write_node_config() {
cat >"${work}/ocserv.conf" <<EOF
auth = "plain[passwd=/etc/ocserv/ocpasswd]"
tcp-port = 44443
udp-port = 0
run-as-user = ocservia-vpn
run-as-group = ocservia-vpn
socket-file = /run/ocserv.socket
server-cert = $1
server-key = $2
max-clients = 4
max-same-clients = 2
cookie-timeout = 300
device = vpns
ipv4-network = 10.208.0.0/24
dns = 1.1.1.1
route = default
use-occtl = true
EOF
sudo install -m 600 "${work}/ocserv.conf" /etc/ocserv/ocserv.conf
}
write_node_config /etc/ocserv/business-test.crt /etc/ocserv/business-test.key
sudo ocserv --test-config -c /etc/ocserv/ocserv.conf
sudo systemctl daemon-reload
sudo systemctl restart ocserv
sudo /usr/libexec/ocservia/ocservia-privd --host-preflight
# Enrollment crosses the sole Relay, so it must be up before the first
# Smoke installer run consumes its bootstrap token.
if [[ "$PRODUCTION_SIGNER_ACCEPTANCE" != true ]]; then
mkdir -m 700 "${work}/relay"
cp "${OCSERV_SECRET_DIR}/tls.crt" "${work}/relay/relay.crt"
cp "${work}/private/tls.key" "${work}/relay/relay.key"
cp "${work}/private/relay-access-token" "${work}/relay/relay-token"
sudo chown -R 65532:65532 "${work}/relay"
docker run -d --name "${OCSERVIA_BUSINESS_TEST_RELAY_CONTAINER}" --read-only --cap-drop ALL \
  --security-opt no-new-privileges:true -p "${gateway}:3443:3443" \
  -e IROH_RELAY_ACCESS_TOKEN_FILE=/run/relay-secrets/relay-token \
  -e RUST_LOG=info,iroh_relay::server::clients=debug \
  -v "${work}/relay:/run/relay-secrets:ro" \
  -v "${ROOT}/deploy/test-fixtures/relay.toml:/etc/iroh-relay/relay.toml:ro" \
  "${OCSERVIA_BUSINESS_TEST_RELAY_IMAGE:-${BUILDX_BUILDER}-relay}" --config-path /etc/iroh-relay/relay.toml
fi
sudo install -m 644 "${work}/ca.crt" /usr/local/share/ca-certificates/ocservia-business-test-ca.crt
sudo update-ca-certificates
# Token-only installation requires the Relay to be reachable for enrollment and initial session establishment.
python3 "${ROOT}/scripts/release-business-api.py" bootstrap_token
sudo env USER_PASSWORD_SEAL_KEY_ID="${USER_PASSWORD_SEAL_KEY_ID}" P12_PASSWORD_SEAL_KEY_ID="${P12_PASSWORD_SEAL_KEY_ID}" \
  bash "${ROOT}/deploy/managed-node/install.sh" --non-interactive \
  --join-token-file "${work}/bootstrap/join-token" "${managed_options[@]}" >"${ARTIFACT_DIR}/managed-enrollment.log"
rm -f -- "${work}/bootstrap/join-token"
grep -Fxq ONLINE "${ARTIFACT_DIR}/managed-enrollment.log"
export OCSERVIA_BUSINESS_TEST_NODE_ID
OCSERVIA_BUSINESS_TEST_NODE_ID="$(sed -nE 's/^NODE_ID: ([0-9a-f-]{36})$/\1/p' "${ARTIFACT_DIR}/managed-enrollment.log")"
[[ "${OCSERVIA_BUSINESS_TEST_NODE_ID}" =~ ^[0-9a-f-]{36}$ ]]
sudo test ! -e /etc/ocservia-agent/enrollment-token
export OCSERVIA_BUSINESS_TEST_AGENT_ENDPOINT_ID
OCSERVIA_BUSINESS_TEST_AGENT_ENDPOINT_ID="$(sudo -u ocserv-agent /usr/libexec/ocservia/ocservia-agent --controller "${CONTROLLER_ENDPOINT_ID}" --prepare-enrollment)"
printf '%s\n' "${OCSERVIA_BUSINESS_TEST_NODE_ID}" >"${work}/signer-node"
printf '%s\n' "${OCSERVIA_BUSINESS_TEST_WORKSPACE_ID}" >"${work}/signer-workspace"
printf '%s\n' "${OCSERVIA_BUSINESS_TEST_AGENT_ENDPOINT_ID}" >"${work}/signer-endpoint"
record signed_native_package_and_managed_prepare
export OCSERVIA_BUSINESS_TEST_TRANSPORT_CONTAINER
OCSERVIA_BUSINESS_TEST_TRANSPORT_CONTAINER="$(compose ps -q transportd)"
[[ -n "${OCSERVIA_BUSINESS_TEST_TRANSPORT_CONTAINER}" ]]
sudo openssl pkey -pubin -in "${OCSERV_SECRET_DIR}/controller-command-verification-key.pem" -noout
[[ "$(sudo stat -c '%u:%g:%a:%h' "${OCSERV_SECRET_DIR}/controller-command-verification-key.pem")" == 0:65532:440:1 ]]
docker inspect "${OCSERVIA_BUSINESS_TEST_TRANSPORT_CONTAINER}" | jq -e '.[0].Mounts | all(.[]; .Destination != "/run/secrets/controller_command_signing_key")' >/dev/null
docker exec "${OCSERVIA_BUSINESS_TEST_TRANSPORT_CONTAINER}" test ! -e /run/secrets/controller_command_signing_key
printf 'SPKI public key, regular one-link root:65532 0440; no Controller signing-key mount\n' >"${ARTIFACT_DIR}/transport-key-boundary.txt"
[[ "$(sudo stat -c '%u:%g:%a:%h' "${OCSERV_SECRET_DIR}/relay-ca.pem")" == 0:0:444:1 ]]
[[ "$(sudo stat -c '%u:%g:%a:%h' /etc/ocservia-agent/relay-ca.pem)" == 0:0:444:1 ]]
# Compose uses init:true, so PID 1 is Docker's init rather than transportd.
docker exec "${OCSERVIA_BUSINESS_TEST_TRANSPORT_CONTAINER}" sh -c '
  found=false
  for executable in /proc/[0-9]*/exe; do
    if [ "$(readlink "$executable")" = /usr/local/bin/ocservia-transportd ]; then
      cat "${executable%exe}cmdline"
      found=true
    fi
  done
  [ "$found" = true ]
' | tr '\0' '\n' >"${ARTIFACT_DIR}/transport-argv.txt"
grep -Fx /run/secrets/relay_ca "${ARTIFACT_DIR}/transport-argv.txt"
printf 'Additional public Relay CA: one-link root:root 0444 on Controller and node; official transport launcher flag present\n' >"${ARTIFACT_DIR}/relay-ca-boundary.txt"
sudo openssl pkey -in /etc/ocservia-agent/user-password-seal-private.pem -pubout >"${work}/user.pub.pem"
sudo openssl pkey -in /etc/ocservia-agent/p12-password-seal-private.pem -pubout >"${work}/p12.pub.pem"
cmp "${ROOT}/deploy/production/systemd/agent-relays.sh" /usr/libexec/ocservia/ocservia-agent-relays
# Reject the forbidden legacy fixture override; it must never return.
[[ ! -e /etc/systemd/system/ocservia-agent.service.d/99-t07-ca.conf ]]
identity_digest() {
  sudo sha256sum /var/lib/ocservia-agent/identity/endpoint.key /var/lib/ocservia-agent/identity/controller.endpoint
}
enrolled_identity="$(identity_digest)"
if [[ "${BUSINESS_PROFILE}" == smoke ]]; then
  python3 "${ROOT}/scripts/release-business-api.py" bootstrap_verify
  # A rerun validates identity and active services without re-enrolling.
  bash "${ROOT}/deploy/managed-node/install.sh" --non-interactive --resume-install "${managed_options[@]}" >"${ARTIFACT_DIR}/managed-rerun.log"
  grep -Fxq ONLINE "${ARTIFACT_DIR}/managed-rerun.log"
  grep -Fxq "NODE_ID: ${OCSERVIA_BUSINESS_TEST_NODE_ID}" "${ARTIFACT_DIR}/managed-rerun.log"
  [[ "$(identity_digest)" == "${enrolled_identity}" ]]
  record bootstrap_token_enrollment_and_rerun_identity
fi
record official_managed_enrollment_and_unchanged_launchers
# Prevent direct UDP connectivity from masking the single-Relay outage.
sudo iptables -I OUTPUT -m owner --uid-owner "$(id -u ocserv-agent)" -p udp ! --dport 53 -j REJECT
relay_ipv4_rule=true
if [[ -n "$(cat /proc/net/if_inet6)" ]]; then
  sudo ip6tables -I OUTPUT -m owner --uid-owner "$(id -u ocserv-agent)" -p udp ! --dport 53 -j REJECT
  relay_ipv6_rule=true
fi
next_stage config_tls
python3 "${ROOT}/scripts/release-business-api.py" config_prepare
config_ref="$(jq -r .id "${work}/config-reference.json")"
write_node_config "/etc/ocservia-agent/config-tls/${config_ref}/v1/server-cert.pem" "/etc/ocservia-agent/config-tls/${config_ref}/v1/server-key.pem"
sudo ocserv --test-config -c /etc/ocserv/ocserv.conf
sudo systemctl restart ocserv
if [[ "$PRODUCTION_SIGNER_ACCEPTANCE" == true ]]; then
  python3 "$ROOT/scripts/release-production-signer.py" import
  record production_signer_active_binding
fi
bash "${ROOT}/deploy/managed-node/install.sh" --non-interactive --resume-install "${managed_options[@]}" >"${ARTIFACT_DIR}/managed-active.log"
grep -Fxq ONLINE "${ARTIFACT_DIR}/managed-active.log"
grep -Fxq "NODE_ID: ${OCSERVIA_BUSINESS_TEST_NODE_ID}" "${ARTIFACT_DIR}/managed-active.log"
[[ "$(identity_digest)" == "${enrolled_identity}" ]]
record native_node_services
if [[ "${BUSINESS_PROFILE}" == extended ]]; then
  next_stage certificate
  python3 "${ROOT}/scripts/release-business-api.py" certificate
  record real_certificate_lifecycle
fi
next_stage browser
npm --prefix "${ROOT}/web" ci --ignore-scripts
(cd "${ROOT}/web" && npx playwright install --with-deps chromium)
sudo apt-get install -y --no-install-recommends libnss3-tools
mkdir -p "${HOME}/.pki/nssdb"
if [[ ! -f "${HOME}/.pki/nssdb/cert9.db" ]]; then certutil -N --empty-password -d "sql:${HOME}/.pki/nssdb"; fi
certutil -A -d "sql:${HOME}/.pki/nssdb" -n ocservia-business-test-ca -t C,, -i "${work}/ca.crt"
python3 "${ROOT}/scripts/release-business-api.py" browser_prepare
NODE_EXTRA_CA_CERTS="${work}/ca.crt" node "${ROOT}/scripts/release-business-browser.mjs"
python3 "${ROOT}/scripts/release-business-api.py" browser_verify
record real_browser_subset
if [[ "${BUSINESS_PROFILE}" == smoke ]]; then
  next_stage signer_vpn_user
  python3 "${ROOT}/scripts/release-business-api.py" smoke_user
  record config_apply_and_vpn_user
fi
next_stage vpn_client_setup
sudo ip netns add business-test-client
sudo ip link add biztest-host type veth peer name biztest-peer
sudo ip link set biztest-peer netns business-test-client
sudo ip addr add 10.207.0.1/30 dev biztest-host
sudo ip link set biztest-host up
sudo ip netns exec business-test-client ip addr add 10.207.0.2/30 dev biztest-peer
sudo ip netns exec business-test-client ip link set biztest-peer up
sudo ip netns exec business-test-client ip link set lo up
cat >"${work}/vpn-script" <<'EOF'
#!/bin/sh
set -eu
if [ "$reason" = connect ]; then
  ip link set "$TUNDEV" up
  ip addr add "$INTERNAL_IP4_ADDRESS/24" dev "$TUNDEV"
fi
EOF
chmod 700 "${work}/vpn-script"
next_stage business_vpn
python3 "${ROOT}/scripts/release-business-api.py" vpn_after_config_apply
record real_vpn_after_config_apply
if [[ "${BUSINESS_PROFILE}" == extended ]]; then
  next_stage configuration_rollback
  python3 "${ROOT}/scripts/release-business-api.py" configuration
  record config_plan_automatic_rollback
  next_stage vpn_after_rollback
  python3 "${ROOT}/scripts/release-business-api.py" vpn_after_rollback
  record real_vpn_after_rollback
  next_stage supplemental_business
  python3 "${ROOT}/scripts/release-business-api.py" business
  record real_vpn_business_and_recovery
fi
if [[ "$BUSINESS_RUN_RESILIENCE" == true ]]; then
  next_stage single_instance_recovery
  python3 "${ROOT}/scripts/release-business-api.py" resilience
  resilience_result=PASS
  record single_instance_recovery
fi
if [[ "${BUSINESS_PROFILE}" == extended ]]; then
  if [[ "$PRODUCTION_SIGNER_ACCEPTANCE" == true ]]; then
    next_stage integrated_recovery
    python3 "$ROOT/scripts/release-integrated-acceptance.py" recovery
    record integrated_recreation_and_authorized_sse
  fi
  # Last version-dependent stage: the rollout replaces the Agent, so no later check may assume VERSION.
  next_stage agent_rollout
  bash "${ROOT}/scripts/release-business-rollout.sh"
  record real_browser_agent_rollout
fi
# Default release acceptance retains terminal revocation; manual diagnostics
# may instead prove package removal without changing the node's trust.
next_stage "node_${BUSINESS_TERMINAL}"
python3 "${ROOT}/scripts/release-business-api.py" "$BUSINESS_TERMINAL"
if [[ "$BUSINESS_TERMINAL" == uninstall ]]; then
  record real_node_uninstall_and_offline
else
  record real_node_revocation_and_disconnect
fi
next_stage complete
