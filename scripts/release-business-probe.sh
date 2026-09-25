#!/usr/bin/env bash
# Disposable hosted runner only. Smoke and extended profiles retain separate scope.
# shellcheck disable=SC2024 # sudo reads protected state; redirects are runner-owned.
set -Eeuo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
: "${ARTIFACT_DIR:?}" "${VERSION:?}" "${CANDIDATE_SHA:?}"
: "${BUSINESS_PROFILE:=smoke}"
: "${PRODUCTION_SIGNER_ACCEPTANCE:=false}"
: "${INTEGRATED_INSTALL_ONLY:=false}"
[[ "$PRODUCTION_SIGNER_ACCEPTANCE" == false || "$BUSINESS_PROFILE" == extended ]]
[[ "${BUSINESS_PROFILE}" == smoke || "${BUSINESS_PROFILE}" == extended ]]
[[ "${GITHUB_ACTIONS:-}" == true && "${RUNNER_ENVIRONMENT:-}" == github-hosted ]]
[[ "${VERSION}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ && "${CANDIDATE_SHA}" =~ ^[0-9a-f]{40}$ ]]
[[ "${GITHUB_SHA:?}" == "${CANDIDATE_SHA}" && "$(git -C "${ROOT}" rev-parse HEAD)" == "${CANDIDATE_SHA}" ]]
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
export T07_WORK
T07_WORK="$(mktemp -d "${HOME}/.t07-XXXXXX")"
work="${T07_WORK}"
stage=preflight
started="$(date -u +%FT%TZ)"
stage_started="$(date +%s)"
export SOURCE_COMMIT="${CANDIDATE_SHA}" PACKAGE_ARCH="${CONTROLLER_ARCH:-amd64}" CONTROLLER_ARCH="${CONTROLLER_ARCH:-amd64}"
export SOURCE_DATE_EPOCH OUTPUT_DIR="${CANDIDATE_PRODUCTS:-${work}/products}" AGENT_SIGNING_KEY="${work}/signing.key"
SOURCE_DATE_EPOCH="$(git -C "${ROOT}" log -1 --format=%ct)"
export BUILDX_BUILDER="business-${GITHUB_RUN_ID:?}-${GITHUB_RUN_ATTEMPT:?}"
registry="${BUILDX_BUILDER}-registry"
export T07_RELAY_CONTAINER="${BUILDX_BUILDER}-relay"
oidc_container="${BUILDX_BUILDER}-oidc"
export T07_OIDC_CONTAINER="$oidc_container" T07_SIGNER_CONTAINER="${BUILDX_BUILDER}-signer"
signer_pid=
export OCSERV_SECRET_DIR="${work}/secrets" OCSERV_BACKUP_DIR="${work}/backup"
export OCSERV_CONTROLLER_STATE_ROOT="${work}/state"
export OCSERV_PUBLIC_HOST=localhost OCSERV_HTTPS_ADDRESS=127.0.0.1
export OCSERV_CONTROLLER_PUBLIC_URL=https://localhost OCSERV_PUBLIC_ORIGIN=https://localhost
export OCSERV_LOCAL_AUTH_ENABLED=true OCSERV_AUDIT_EVENT_KEY_ID=t07 OCSERV_BACKUP_INTERVAL_SECONDS=86400
export OCSERV_CERTIFICATE_SIGNER_URL=https://signer.unavailable.invalid
if [[ "$PRODUCTION_SIGNER_ACCEPTANCE" == true ]]; then
  [[ "$EUID" == 0 && -n "${CANDIDATE_BUNDLE:-}" && -n "${CANDIDATE_PRODUCTS:-}" ]]
  gateway="$(docker network inspect bridge --format '{{(index .IPAM.Config 0).Gateway}}')"
  export OCSERV_DEPLOYMENT_MODE=integrated OCSERV_HTTPS_ADDRESS=0.0.0.0
  export OCSERV_PUBLIC_HOST="controller.${gateway}.sslip.io" OCSERV_RELAY_PUBLIC_HOST="relay.${gateway}.sslip.io"
  export OCSERV_CONTROLLER_PUBLIC_URL="https://${OCSERV_PUBLIC_HOST}" OCSERV_PUBLIC_ORIGIN="https://${OCSERV_PUBLIC_HOST}"
  export OCSERV_RELAY_URL_A="https://${OCSERV_RELAY_PUBLIC_HOST}" OCSERV_RELAY_URL_B=
  export OCSERV_CERTIFICATE_SIGNER_URL=https://signer:9443/sign
  export OCSERV_RELAY_SECRET_DIR="${work}/relay-secrets"
  export OCSERV_SIGNER_SECRET_DIR="${work}/production-signer/secrets" OCSERV_SIGNER_STATE_DIR="${work}/production-signer/data"
fi
unset OCSERV_OIDC_ISSUER OCSERV_OIDC_CLIENT_ID OCSERV_OIDC_REDIRECT_URL OCSERV_OTEL_BACKEND_ENDPOINT
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
configure_auth_peers() {
  # --no-deps makes ordering our responsibility. The launcher stops both peers;
  # wait for the trust backend before starting the unchanged transport container.
  compose up -d --no-deps --wait control-plane
  compose start transportd
  python3 "${ROOT}/scripts/release-business-api.py" transport_ready
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
      docker logs "${oidc_container}"
      sudo journalctl --no-pager -o short-iso-precise -u ocservia-agent -u ocservia-privd -u ocserv
      if [[ "$PRODUCTION_SIGNER_ACCEPTANCE" == true ]]; then
        docker logs "$T07_SIGNER_CONTAINER"
        sudo journalctl --no-pager -o short-iso-precise -u ocservia-p2-crl
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
  jq -n --arg sha "${CANDIDATE_SHA}" --arg version "${VERSION}" --arg start "${started}" \
    --arg end "$(date -u +%FT%TZ)" --arg stage "${failed_stage}" --argjson code "${code}" \
    --arg profile "${BUSINESS_PROFILE}" \
    --arg arch "$CONTROLLER_ARCH" \
    --arg run "${GITHUB_RUN_ID}" --arg attempt "${GITHUB_RUN_ATTEMPT}" \
    --rawfile checkpoints "${ARTIFACT_DIR}/checkpoints.txt" \
    --slurpfile timings "${ARTIFACT_DIR}/timings.json" \
    '{candidate_sha:$sha,candidate_version:$version,run_id:$run,run_attempt:$attempt,profile:$profile,
      started_at:$start,finished_at:$end,exit_code:$code,last_stage:$stage,
      timings:$timings[0],passed_checkpoints:($checkpoints | split("\n") | map(select(length > 0))),
      probe_status:(if $code == 0 then "PASS" else "FAIL" end),
      scope:(if $profile == "smoke" then "business-smoke" else "integration" end),
      planned_topology:{hosts:1,architecture:$arch,native_systemd_node:true,relays:1,relay_redundancy:false},
      operator_mode:"simulated_two_principals",independent_human_custody:"NOT_VERIFIED",
      limitations:["separate authenticated principals and browser sessions are not two independently responsible people"],
      deferred:["Publish: immutable published Release download/bootstrap"],
      not_applicable:["cross-host resilience and performance assessment"]}' >"${ARTIFACT_DIR}/result.json"
  sudo systemctl stop ocservia-agent ocservia-privd ocserv >/dev/null 2>&1
  if [[ "$PRODUCTION_SIGNER_ACCEPTANCE" == true ]]; then
    sudo systemctl stop ocservia-p2-crl >/dev/null 2>&1
    docker rm -f "$T07_SIGNER_CONTAINER" >/dev/null 2>&1
  fi
  sudo ip netns pids t07-client 2>/dev/null | xargs -r sudo kill
  sudo ip netns del t07-client >/dev/null 2>&1
  compose down --volumes --remove-orphans >/dev/null 2>&1
  docker rm -f "${registry}" "${T07_RELAY_CONTAINER}" "${oidc_container}" >/dev/null 2>&1
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
openssl genpkey -algorithm ED25519 -out "${AGENT_SIGNING_KEY}"
openssl pkey -in "${AGENT_SIGNING_KEY}" -pubout -out "${work}/trusted-release.pub.pem"
export OCSERV_CONTROLLER_RELEASE_PUBLIC_KEY="${work}/trusted-release.pub.pem"
if [[ -n "${CANDIDATE_PRODUCTS:-}" ]]; then
  next_stage candidate_verification
  node scripts/release-artifacts.mjs verify "${OUTPUT_DIR}" agent "$CONTROLLER_ARCH" "${VERSION}" "${AGENT_MANIFEST_SHA256:?}"
  if [[ "$PRODUCTION_SIGNER_ACCEPTANCE" != true ]]; then
  node scripts/release-artifacts.mjs verify "${OUTPUT_DIR}" controller amd64 "${VERSION}" "${CONTROLLER_MANIFEST_SHA256:?}"
  if [[ -z "${RELEASE_RELAY_IMAGE:-}" ]]; then
    docker buildx create --driver docker-container --name "${BUILDX_BUILDER}" \
    --driver-opt image=moby/buildkit:v0.32.2@sha256:28a898719c18a33f4e8000685287fa36fd0dd9560c6440227d3a732d79bb41d8 --bootstrap --use
  fi
  fi
else
  bash "${ROOT}/scripts/bootstrap.sh" native-packages
  next_stage agent_package_build
  env -u BUILDX_BUILDER bash "${ROOT}/scripts/build-release-agent.sh" >"${ARTIFACT_DIR}/agent-build.log" 2>&1
  next_stage controller_image_build
  bash "${ROOT}/scripts/build-release-controller.sh" >"${ARTIFACT_DIR}/controller-build.log" 2>&1
fi
next_stage relay_image_build
if [[ "$PRODUCTION_SIGNER_ACCEPTANCE" == true ]]; then
  cp -R "${CANDIDATE_BUNDLE}/." "${work}/bundle/"
  export CANDIDATE_BUNDLE="${work}/bundle"
  bash "$ROOT/scripts/consume-controller-candidate.sh"
  manifest="${CANDIDATE_BUNDLE}/controller-release-${CONTROLLER_ARCH}.json"
  for name in gateway control transport backup edge relay signer postgres otel; do
    export "OCSERV_${name^^}_IMAGE=$(jq -er --arg name "$name" '.images[$name]' "$manifest")"
  done
  export T07_RELAY_IMAGE="$OCSERV_RELAY_IMAGE" T07_SIGNER_IMAGE="$OCSERV_SIGNER_IMAGE"
  install -m 600 "${CANDIDATE_BUNDLE}/candidate-signing.pub.pem" "${work}/candidate-release.pub.pem"
  export OCSERV_CONTROLLER_RELEASE_PUBLIC_KEY="${work}/candidate-release.pub.pem"
else
if [[ -n "${RELEASE_RELAY_IMAGE:-}" ]]; then
  docker tag "${RELEASE_RELAY_IMAGE}" "${BUILDX_BUILDER}-relay"
else
bash "${ROOT}/scripts/g6-buildx-cache.sh" relay-business-amd64 true business-relay \
  --builder "${BUILDX_BUILDER}" --platform linux/amd64 --provenance=false --load \
  --label "org.opencontainers.image.revision=${CANDIDATE_SHA}" \
  -t "${BUILDX_BUILDER}-relay" -f "${ROOT}/deploy/production/relay.Dockerfile" "${ROOT}" \
  >"${ARTIFACT_DIR}/relay-build.log" 2>&1
fi
docker run --rm --entrypoint /usr/local/bin/iroh-relay "${BUILDX_BUILDER}-relay" --version >"${ARTIFACT_DIR}/relay-version.txt"
docker image inspect --format '{{.Id}} {{.Architecture}}' "${BUILDX_BUILDER}-relay" >"${ARTIFACT_DIR}/relay-image.txt"
fi
find "${OUTPUT_DIR}" -maxdepth 1 -type f -print0 | sort -z | xargs -0 sha256sum >"${ARTIFACT_DIR}/product-digests.txt"
record candidate_built
next_stage dependency_install
if [[ "$INTEGRATED_INSTALL_ONLY" != true ]]; then
sudo apt-get update -qq
sudo apt-get install -y --no-install-recommends ocserv openconnect vpnc-scripts sqlite3 iputils-ping
sudo systemctl stop ocserv
{ dpkg-query -W ocserv openconnect libgnutls30t64 openssl systemd; ocserv --version; } >>"${ARTIFACT_DIR}/environment.txt" 2>&1
fi
# Subsequent output can contain task-only secret data. Keep it private until
# the exact-value redactor runs; the uploaded results never contain cookies.
exec >"${work}/private.log" 2>&1
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=t07-ca \
  -addext basicConstraints=critical,CA:TRUE -keyout "${work}/private/ca.key" -out "${work}/ca.crt"
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
  export OCSERV_RELAY_URL_A="https://${gateway}:3443" OCSERV_RELAY_URL_B=
fi
for name in postgres-owner-password postgres-app-password postgres-backup-password session-key audit-checkpoint-key \
  audit-event-key certificate-signer-token relay-access-token oidc-client-secret; do
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
  "${OCSERV_SECRET_DIR}/controller-iroh.key" "${OCSERV_SECRET_DIR}/relay-access-token"
sudo install -o root -g 65532 -m 440 "${work}/command.pub.pem" "${OCSERV_SECRET_DIR}/controller-command-verification-key.pem"
sudo install -o root -g root -m 444 "${work}/ca.crt" "${OCSERV_SECRET_DIR}/relay-ca.pem"
sudo chown 999:999 "${work}/backup"
registry_prefix=localhost:5000
registry_tag="$VERSION"
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
else
  docker run -d --name "${registry}" -p 127.0.0.1:5000:5000 registry:2
fi
if [[ "$PRODUCTION_SIGNER_ACCEPTANCE" != true ]]; then
publish_pull() {
  local name="$1" source="$2" target="${registry_prefix}/$1:${registry_tag}" ref image_id
  image_id="$(docker image inspect --format '{{.Id}}' "$source")"
  docker tag "$source" "$target"
  docker push "$target"
  ref="$(docker image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' "$target" | grep "^${registry_prefix}/${name}@sha256:")"
  [[ "$ref" =~ @sha256:[0-9a-f]{64}$ ]]
  if [[ "$PRODUCTION_SIGNER_ACCEPTANCE" == true ]]; then
    docker image rm "$source" "$target" >/dev/null
    docker pull "$ref"
    [[ "$(docker image inspect --format '{{.Id}}' "$ref")" == "$image_id" ]]
    jq -nc --arg candidate_sha "$CANDIDATE_SHA" --arg reference "$ref" --arg image_id "$image_id" \
      '{candidate_sha:$candidate_sha,reference:$reference,image_id:$image_id,pulled:true}' >>"${ARTIFACT_DIR}/registry-pulls.jsonl"
  fi
  PULLED_REFERENCE="$ref"
}
args=()
for name in gateway control transport backup; do
  docker load -i "${OUTPUT_DIR}/${name}-linux-amd64.tar"
  publish_pull "$name" "ghcr.io/gentlekingson/ocservia/${name}:${VERSION}-linux-amd64"
  ref="$PULLED_REFERENCE"
  args+=(--image "${name}=${ref}")
  export "OCSERV_${name^^}_IMAGE=${ref}"
done
# The frozen existing production database/image rows, not a new support matrix.
export OCSERV_POSTGRES_IMAGE OCSERV_OTEL_IMAGE
OCSERV_POSTGRES_IMAGE=docker.io/library/postgres@sha256:9b18b78397054fce88a9552e9d5a3ad5bb7fd258c5b3cc1c5028e46373d6ea8f
OCSERV_OTEL_IMAGE=docker.io/otel/opentelemetry-collector@sha256:0c066d4388070dad8dc9961d9f23649e85a226620e6b359334e4a6c7f9d73b23
manifest="${work}/bundle/controller-release-amd64.json"
node "${ROOT}/scripts/generate-controller-release-manifest.mjs" --output "${manifest}" \
  --release-version "${VERSION}" --release-tag "v${VERSION}" --source-commit "${CANDIDATE_SHA}" \
  --migration-dir "${ROOT}/control-plane/migrations" --platform linux/amd64 \
  "${args[@]}" --image "postgres=${OCSERV_POSTGRES_IMAGE}" --image "otel=${OCSERV_OTEL_IMAGE}"
(cd "${work}/bundle" && sha256sum controller-release-amd64.json >SHA256SUMS)
cp "${work}/bundle/SHA256SUMS" "${manifest}.sha256"
openssl pkeyutl -sign -rawin -inkey "${AGENT_SIGNING_KEY}" -in "${work}/bundle/SHA256SUMS" -out "${work}/bundle/SHA256SUMS.sig"
cp "${manifest}" "${ARTIFACT_DIR}/candidate-manifest.json"
fi
next_stage controller_install
"${ROOT}/deploy/production/controller.sh" install --release-file "${manifest}"
if [[ "$PRODUCTION_SIGNER_ACCEPTANCE" == true ]]; then
  T07_SIGNER_CONTAINER="$(compose ps -q signer)"
  T07_RELAY_CONTAINER="$(compose ps -q relay)"
fi
curl --fail --silent --show-error "$OCSERV_CONTROLLER_PUBLIC_URL/api/v1/version" | \
  jq -e --arg sha "${CANDIDATE_SHA}" --arg v "${VERSION}" '.commit == $sha and .version == $v' >/dev/null
record signed_controller_lifecycle
if [[ "$INTEGRATED_INSTALL_ONLY" == true ]]; then
  [[ "$PRODUCTION_SIGNER_ACCEPTANCE" == true ]]
  "$ROOT/deploy/production/controller.sh" uninstall
  "$ROOT/deploy/production/controller.sh" start
  curl --fail --silent --show-error "$OCSERV_CONTROLLER_PUBLIC_URL/api/v1/version" |
    jq -e --arg sha "$CANDIDATE_SHA" '.commit == $sha' >/dev/null
  record integrated_native_install_restart
  next_stage complete
  exit 0
fi
export T07_WORKSPACE=00000000-0000-7000-8000-000000000071
compose exec -T postgres psql -XAt -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia <<SQL
INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES
 ('${T07_WORKSPACE}','T07','t07',now(),now()),
 ('00000000-0000-7000-8000-000000000072','Other','t07-other',now(),now());
SQL
for role in requester approver; do
  openssl rand -hex 32 >"${work}/private/${role}-password"
  sudo install -o 65534 -g 65532 -m 400 "${work}/private/${role}-password" "${OCSERV_SECRET_DIR}/${role}-password"
done
compose run --rm --no-deps -e OCSERV_LOCAL_BOOTSTRAP_USERNAME=t07-requester \
  -e "OCSERV_LOCAL_BOOTSTRAP_WORKSPACE_ID=${T07_WORKSPACE}" \
  -e OCSERV_LOCAL_BOOTSTRAP_PASSWORD_FILE=/run/secrets/requester-password \
  -e OCSERV_LOCAL_BOOTSTRAP_APPROVER_USERNAME=t07-approver \
  -e OCSERV_LOCAL_BOOTSTRAP_APPROVER_PASSWORD_FILE=/run/secrets/approver-password \
  -v "${OCSERV_SECRET_DIR}/requester-password:/run/secrets/requester-password:ro" \
  -v "${OCSERV_SECRET_DIR}/approver-password:/run/secrets/approver-password:ro" \
  control-plane --bootstrap-local-admin
next_stage local_auth
python3 "${ROOT}/scripts/release-business-api.py" local
record real_local_auth
if [[ "${BUSINESS_PROFILE}" == extended ]]; then
next_stage external_auth
# This contains only public fault names; the capability-free fixture must read it.
mkdir -m 755 "${work}/oidc-fault"
printf '\n' >"${work}/oidc-fault/mode"
chmod 644 "${work}/oidc-fault/mode"
export OCSERV_OIDC_ISSUER=https://172.30.240.3:19443 OCSERV_OIDC_CLIENT_ID=upgrade
export OCSERV_OIDC_REDIRECT_URL="$OCSERV_CONTROLLER_PUBLIC_URL/api/v1/auth/callback"
signer_address=172.30.240.3
if [[ "$PRODUCTION_SIGNER_ACCEPTANCE" != true ]]; then
  export OCSERV_CERTIFICATE_SIGNER_URL="https://${signer_address}:19444/sign"
fi
docker run -d --name "${oidc_container}" --read-only --cap-drop ALL --security-opt no-new-privileges:true \
  --network ocservia-production_application --ip 172.30.240.3 \
  -v "${ROOT}/scripts/release-upgrade-oidc-fixture.mjs:/fixture.mjs:ro" \
  -v "${OCSERV_SECRET_DIR}/tls.key:/fixture/tls.key:ro" \
  -v "${OCSERV_SECRET_DIR}/tls.crt:/fixture/tls.crt:ro" \
  -v "${OCSERV_SECRET_DIR}/oidc-client-secret:/fixture/oidc-client-secret:ro" \
  -v "${work}/oidc-fault:/fault:ro" \
  node:24.18.1-bookworm-slim@sha256:235600a8101ab264e117b1768e925532262668dc9b581ef1dd7d96ced463b8e7 \
  node /fixture.mjs /fixture "${OCSERV_OIDC_ISSUER}" /fault/mode "$OCSERV_OIDC_REDIRECT_URL"
# The internal application network deliberately has no host gateway. Join only
# the task provider's network namespace, then run the signer as the runner UID.
if [[ "$PRODUCTION_SIGNER_ACCEPTANCE" != true ]]; then
provider_pid="$(docker inspect --format '{{.State.Pid}}' "${oidc_container}")"
sudo nsenter --target "${provider_pid}" --net -- setpriv --reuid="$(id -u)" --regid="$(id -g)" --clear-groups \
  python3 "${ROOT}/scripts/release-business-signer.py" "${work}" "${signer_address}" &
signer_pid=$!
fi
configure_auth_peers
python3 "${ROOT}/scripts/release-business-api.py" trust_controller
python3 "${ROOT}/scripts/release-business-api.py" oidc
export OCSERV_LOCAL_AUTH_ENABLED=false
configure_auth_peers
python3 "${ROOT}/scripts/release-business-api.py" trust_controller
python3 "${ROOT}/scripts/release-business-api.py" oidc
export OCSERV_LOCAL_AUTH_ENABLED=true
configure_auth_peers
python3 "${ROOT}/scripts/release-business-api.py" trust_controller
compose exec -T postgres psql -XAt -U ocservia_owner -d ocservia -c 'SHOW server_version' >>"${ARTIFACT_DIR}/environment.txt"
record real_external_oidc
else
  python3 "${ROOT}/scripts/release-business-api.py" transport_ready
fi
next_stage native_node
# The candidate is not a published Release. Verify the real signed package
# locally, then exercise the official managed-node convergence/preparation.
deb="${OUTPUT_DIR}/ocservia-agent_${VERSION}-1_amd64.deb"
sha256sum "${deb}" >"${work}/package.sha256"
openssl pkeyutl -sign -rawin -inkey "${AGENT_SIGNING_KEY}" -in "${work}/package.sha256" -out "${work}/package.sig"
openssl pkeyutl -verify -rawin -pubin -inkey "${work}/trusted-release.pub.pem" \
  -in "${work}/package.sha256" -sigfile "${work}/package.sig"
cp "${work}/package.sha256" "${work}/tampered.sha256"
printf '\ntampered\n' >>"${work}/tampered.sha256"
if openssl pkeyutl -verify -rawin -pubin -inkey "${work}/trusted-release.pub.pem" \
  -in "${work}/tampered.sha256" -sigfile "${work}/package.sig"; then
  echo 'tampered package manifest unexpectedly verified' >&2
  exit 1
fi
sha256sum -c "${work}/package.sha256"
record real_signature_and_tamper_rejection
sudo install -d -m 755 /etc/ocservia
node_key="${OUTPUT_DIR}/ocservia-agent-${VERSION}-linux-amd64.tar.gz.sha256.pub.pem"
sudo install -m 644 "${node_key}" /etc/ocservia/release-signing.pub.pem
export EXPECTED_RELEASE_KEY_SHA256
EXPECTED_RELEASE_KEY_SHA256="$(openssl pkey -pubin -in "${node_key}" -outform DER | sha256sum | cut -d' ' -f1)"
printf '%s\n' "${EXPECTED_RELEASE_KEY_SHA256}" | sudo tee /etc/ocservia/trusted-release-key.sha256 >/dev/null
sudo chmod 644 /etc/ocservia/trusted-release-key.sha256
sudo touch /etc/ocservia/agent-install-production-relays
sudo dpkg -i "${deb}"
sudo install -o root -g root -m 444 "${work}/ca.crt" /etc/ocservia-agent/relay-ca.pem
export CONTROLLER_ENDPOINT_ID="${OCSERV_CONTROLLER_ENDPOINT_ID}" RELAY_URL_A="${OCSERV_RELAY_URL_A}" RELAY_URL_B=
export RELAY_ACCESS_TOKEN_SOURCE="${OCSERV_SECRET_DIR}/relay-access-token"
export CONTROLLER_COMMAND_VERIFICATION_KEY_SOURCE="${work}/command.pub.pem"
export TRUSTED_RELEASE_KEY=/etc/ocservia/release-signing.pub.pem
export USER_PASSWORD_SEAL_KEY_ID=t07-user P12_PASSWORD_SEAL_KEY_ID=t07-p12 ENROLLMENT_ENVIRONMENT=production
bash "${ROOT}/deploy/managed-node/install.sh" --version "v${VERSION}" >"${ARTIFACT_DIR}/managed-prepare.log"
grep -q ENROLLMENT_READY "${ARTIFACT_DIR}/managed-prepare.log"
record signed_native_package_and_managed_prepare

if [[ "$PRODUCTION_SIGNER_ACCEPTANCE" != true ]]; then
mkdir -m 700 "${work}/relay"
cp "${OCSERV_SECRET_DIR}/tls.crt" "${work}/relay/relay.crt"
cp "${work}/private/tls.key" "${work}/relay/relay.key"
cp "${work}/private/relay-access-token" "${work}/relay/relay-token"
sudo chown -R 65532:65532 "${work}/relay"
docker run -d --name "${T07_RELAY_CONTAINER}" --read-only --cap-drop ALL \
  --security-opt no-new-privileges:true -p "${gateway}:3443:3443" \
  -e IROH_RELAY_ACCESS_TOKEN_FILE=/run/relay-secrets/relay-token \
  -e RUST_LOG=info,iroh_relay::server::clients=debug \
  -v "${work}/relay:/run/relay-secrets:ro" \
  -v "${ROOT}/deploy/g6-readiness/relay.toml:/etc/iroh-relay/relay.toml:ro" \
  "${T07_RELAY_IMAGE:-${BUILDX_BUILDER}-relay}" --config-path /etc/iroh-relay/relay.toml
fi
export T07_TRANSPORT_CONTAINER
T07_TRANSPORT_CONTAINER="$(compose ps -q transportd)"
[[ -n "${T07_TRANSPORT_CONTAINER}" ]]
sudo openssl pkey -pubin -in "${OCSERV_SECRET_DIR}/controller-command-verification-key.pem" -noout
[[ "$(sudo stat -c '%u:%g:%a:%h' "${OCSERV_SECRET_DIR}/controller-command-verification-key.pem")" == 0:65532:440:1 ]]
docker inspect "${T07_TRANSPORT_CONTAINER}" | jq -e '.[0].Mounts | all(.[]; .Destination != "/run/secrets/controller_command_signing_key")' >/dev/null
docker exec "${T07_TRANSPORT_CONTAINER}" test ! -e /run/secrets/controller_command_signing_key
printf 'SPKI public key, regular one-link root:65532 0440; no Controller signing-key mount\n' >"${ARTIFACT_DIR}/transport-key-boundary.txt"
[[ "$(sudo stat -c '%u:%g:%a:%h' "${OCSERV_SECRET_DIR}/relay-ca.pem")" == 0:0:444:1 ]]
[[ "$(sudo stat -c '%u:%g:%a:%h' /etc/ocservia-agent/relay-ca.pem)" == 0:0:444:1 ]]
# Compose uses init:true, so PID 1 is Docker's init rather than transportd.
docker exec "${T07_TRANSPORT_CONTAINER}" sh -c '
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
export T07_ENDPOINT T07_USER_HASH T07_P12_HASH
T07_ENDPOINT="$(sudo -u ocserv-agent /usr/libexec/ocservia/ocservia-agent --controller "${CONTROLLER_ENDPOINT_ID}" --prepare-enrollment)"
T07_USER_HASH="$(openssl pkey -pubin -in "${work}/user.pub.pem" -outform DER | sha256sum | cut -d' ' -f1)"
T07_P12_HASH="$(sudo openssl pkey -in /etc/ocservia-agent/p12-password-seal-private.pem -pubout -outform DER | sha256sum | cut -d' ' -f1)"
python3 "${ROOT}/scripts/release-business-api.py" token
sudo install -o root -g ocserv-agent -m 640 "${work}/private/enrollment-token" /etc/ocservia-agent/enrollment-token
bash "${ROOT}/deploy/managed-node/install.sh" --version "v${VERSION}" >"${ARTIFACT_DIR}/managed-enrollment.log"
grep -q '^PENDING_APPROVAL$' "${ARTIFACT_DIR}/managed-enrollment.log"
export T07_NODE
T07_NODE="$(sed -nE 's/^NODE_ID: ([0-9a-f-]{36})$/\1/p' "${ARTIFACT_DIR}/managed-enrollment.log")"
[[ "${T07_NODE}" =~ ^[0-9a-f-]{36}$ ]]
printf '%s\n' "${T07_NODE}" >"${work}/signer-node"
sudo openssl pkey -in /etc/ocservia-agent/p12-password-seal-private.pem -pubout >"${work}/p12.pub.pem"
sudo test ! -e /etc/ocservia-agent/enrollment-token
cmp "${ROOT}/deploy/production/systemd/agent-relays.sh" /usr/libexec/ocservia/ocservia-agent-relays
[[ ! -e /etc/systemd/system/ocservia-agent.service.d/99-t07-ca.conf ]]
record official_managed_enrollment_and_unchanged_launchers
# Prevent direct UDP connectivity from masking the single-Relay outage.
sudo iptables -I OUTPUT -m owner --uid-owner "$(id -u ocserv-agent)" -p udp ! --dport 53 -j REJECT
sudo install -m 600 "${work}/private/tls.key" /etc/ocserv/t07.key
sudo install -m 644 "${OCSERV_SECRET_DIR}/tls.crt" /etc/ocserv/t07.crt
sudo install -m 600 /dev/null /etc/ocserv/ocpasswd
next_stage config_tls
python3 "${ROOT}/scripts/release-business-api.py" config_prepare
config_ref="$(jq -r .id "${work}/config-reference.json")"
cat >"${work}/ocserv.conf" <<EOF
auth = "plain[passwd=/etc/ocserv/ocpasswd]"
tcp-port = 44443
udp-port = 0
run-as-user = ocservia-vpn
run-as-group = ocservia-vpn
socket-file = /run/ocserv.socket
server-cert = /etc/ocservia-agent/config-tls/${config_ref}/v1/server-cert.pem
server-key = /etc/ocservia-agent/config-tls/${config_ref}/v1/server-key.pem
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
sudo ocserv --test-config -c /etc/ocserv/ocserv.conf
sudo systemctl daemon-reload
sudo systemctl start ocserv ocservia-privd
next_stage node_approval
python3 "${ROOT}/scripts/release-business-api.py" approve
if [[ "$PRODUCTION_SIGNER_ACCEPTANCE" == true ]]; then
  python3 "$ROOT/scripts/release-production-signer.py" import
  record production_signer_approved_binding
fi
sudo systemctl enable --now ocservia-privd ocservia-agent
bash "${ROOT}/deploy/managed-node/install.sh" --version "v${VERSION}" >"${ARTIFACT_DIR}/managed-active.log"
grep -q SERVICES_ACTIVE "${ARTIFACT_DIR}/managed-active.log"
record native_node_services
if [[ "${BUSINESS_PROFILE}" == extended ]]; then
next_stage certificate
python3 "${ROOT}/scripts/release-business-api.py" certificate
record real_certificate_lifecycle
next_stage browser
npm --prefix "${ROOT}/web" ci --ignore-scripts
(cd "${ROOT}/web" && npx playwright install --with-deps chromium)
sudo apt-get install -y --no-install-recommends libnss3-tools
sudo install -m 644 "${work}/ca.crt" /usr/local/share/ca-certificates/t07.crt
sudo update-ca-certificates
mkdir -p "${HOME}/.pki/nssdb"
if [[ ! -f "${HOME}/.pki/nssdb/cert9.db" ]]; then certutil -N --empty-password -d "sql:${HOME}/.pki/nssdb"; fi
certutil -A -d "sql:${HOME}/.pki/nssdb" -n t07 -t C,, -i "${work}/ca.crt"
python3 "${ROOT}/scripts/release-business-api.py" browser_prepare
NODE_EXTRA_CA_CERTS="${work}/ca.crt" node "${ROOT}/scripts/release-business-browser.mjs"
python3 "${ROOT}/scripts/release-business-api.py" browser_verify
record real_browser_subset
else
  next_stage config_apply
  python3 "${ROOT}/scripts/release-business-api.py" smoke_config_apply
  python3 "${ROOT}/scripts/release-business-api.py" smoke_user
  record approved_config_apply_and_vpn_user
fi
next_stage vpn_client_setup
sudo ip netns add t07-client
sudo ip link add t07-host type veth peer name t07-peer
sudo ip link set t07-peer netns t07-client
sudo ip addr add 10.207.0.1/30 dev t07-host
sudo ip link set t07-host up
sudo ip netns exec t07-client ip addr add 10.207.0.2/30 dev t07-peer
sudo ip netns exec t07-client ip link set t07-peer up
sudo ip netns exec t07-client ip link set lo up
cat >"${work}/vpn-script" <<'EOF'
#!/bin/sh
set -eu
if [ "$reason" = connect ]; then
  ip link set "$TUNDEV" up
  ip addr add "$INTERNAL_IP4_ADDRESS/24" dev "$TUNDEV"
fi
EOF
chmod 700 "${work}/vpn-script"
next_stage vpn_before_rollback
python3 "${ROOT}/scripts/release-business-api.py" vpn_before_rollback
record real_vpn_after_config_apply
next_stage configuration_rollback
if [[ "${BUSINESS_PROFILE}" == extended ]]; then
  python3 "${ROOT}/scripts/release-business-api.py" configuration
else
  python3 "${ROOT}/scripts/release-business-api.py" smoke_rollback
fi
record config_plan_automatic_rollback
next_stage vpn_after_rollback
python3 "${ROOT}/scripts/release-business-api.py" vpn_after_rollback
record real_vpn_after_rollback
if [[ "${BUSINESS_PROFILE}" == extended ]]; then
  next_stage supplemental_business
  python3 "${ROOT}/scripts/release-business-api.py" business
  record real_vpn_business_and_recovery
fi
next_stage complete
