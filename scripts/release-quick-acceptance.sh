#!/usr/bin/env bash
# Disposable hosted runner only: Quick install end to end with locally built
# release images and a Pebble ACME directory. Run as root through sudo.
#
# Checks: two-account login, ACME certificates for both names, idempotent
# reruns, resuming an unrecorded Local bootstrap, root CA custody, no
# credentials in output, published ports, and upgrade/rollback/upgrade
# keeping material and certificates.
set -Eeuo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
: "${VERSION:?}" "${GITHUB_RUN_ID:?}" "${GITHUB_RUN_ATTEMPT:?}"
SOURCE_COMMIT="$(git -C "${ROOT}" rev-parse HEAD)"
export SOURCE_COMMIT CONTROLLER_ARCH=amd64
[[ "${EUID}" == 0 && -n "${SUDO_UID:-}" && "${VERSION}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]
[[ -z "$(git -C "${ROOT}" status --porcelain)" ]]
[[ ! -e /etc/ocservia && ! -e /var/lib/ocservia-controller && ! -e /root/ocservia-initial-credentials ]]
# As in quick-install.sh: the lifecycle belongs to root; SUDO_UID keeps git's
# trust in the runner-owned checkout.
unset SUDO_USER
umask 077

PEBBLE_IMAGE=ghcr.io/letsencrypt/pebble@sha256:ddf230642b1a584f519f32e347de1b05a6e4c1f6c35c1863b33effeab5f78199
POSTGRES_IMAGE=docker.io/library/postgres@sha256:1c59e2c3c818eaa0f0628f695b36e7c9e362d6b219b36a54a32df645cbd7e1af
OTEL_IMAGE=docker.io/otel/opentelemetry-collector@sha256:0c066d4388070dad8dc9961d9f23649e85a226620e6b359334e4a6c7f9d73b23
CREDENTIALS_DIR=/root/ocservia-initial-credentials
work="$(mktemp -d)"
export BUILDX_BUILDER="quick-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}" OUTPUT_DIR="${work}/products"
registry="${BUILDX_BUILDER}-registry" pebble="${BUILDX_BUILDER}-pebble"
gateway="$(docker network inspect bridge --format '{{(index .IPAM.Config 0).Gateway}}')"
controller="controller.${gateway}.sslip.io" relay="relay.${gateway}.sslip.io"
stage=build

cleanup() {
  local code=$?
  trap - EXIT
  set +e
  if ((code)); then
    echo "Quick acceptance failed at stage ${stage}" >&2
    [[ "${stage}" != build ]] || tail -n 100 "${work}/controller-build.log" >&2
    for log in "${work}"/run-*.log; do [[ ! -f "${log}" ]] || { echo "== ${log##*/}"; cat -- "${log}"; } >&2; done
    for container in $(docker ps -aq --filter label=com.docker.compose.project=ocservia-production) "${pebble}"; do
      docker inspect --format '== {{.Name}} {{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{end}}' "${container}"
      docker logs --tail 80 "${container}" 2>&1
    done >&2
  fi
  docker buildx rm "${BUILDX_BUILDER}" >/dev/null 2>&1
  rm -rf -- "${work}"
  exit "${code}"
}
trap cleanup EXIT
trap 'echo "Quick acceptance failed at line ${LINENO}" >&2' ERR
pass() { echo "PASS: $1"; }
# errexit ignores negated commands, so absence is asserted explicitly.
absent() { if grep -qF -- "$@"; then echo "unexpected: $1" >&2; return 1; fi; }

bash "${ROOT}/scripts/build-release-controller.sh" >"${work}/controller-build.log" 2>&1

stage=publish
docker run -d --restart unless-stopped --name "${registry}" -p 127.0.0.1:5000:5000 registry:2
manifest_args=(--image "postgres=${POSTGRES_IMAGE}" --image "otel=${OTEL_IMAGE}")
roles=(gateway control transport backup edge relay signer mysql_backup)
for name in "${roles[@]}"; do
  docker load -i "${OUTPUT_DIR}/${name}-linux-${CONTROLLER_ARCH}.tar"
  docker tag "ghcr.io/gentlekingson/ocservia/${name}:${VERSION}-linux-${CONTROLLER_ARCH}" "localhost:5000/${name}:v${VERSION}"
  docker push "localhost:5000/${name}:v${VERSION}"
  manifest_args+=(--image "${name}=localhost:5000/${name}:v${VERSION}")
done
manifest="${work}/controller-release-${CONTROLLER_ARCH}.json"
node "${ROOT}/scripts/generate-controller-release-manifest.mjs" --output "${manifest}" \
  --release-version "${VERSION}" --release-tag "v${VERSION}" --source-commit "${SOURCE_COMMIT}" \
  --migration-dir "${ROOT}/control-plane/migrations" --platform "linux/${CONTROLLER_ARCH}" \
  --manifest-version 2 "${manifest_args[@]}"
# The upgrade target names the same images by digest, a real configuration
# change between two reference forms (as in the Business probe).
upgrade_manifest="${work}/controller-release-digests.json"
cp -- "${manifest}" "${upgrade_manifest}"
for name in "${roles[@]}"; do
  ref="$(docker image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' "localhost:5000/${name}:v${VERSION}" |
    grep "^localhost:5000/${name}@sha256:")"
  jq --arg name "${name}" --arg ref "${ref}" '.images[$name] = $ref' "${upgrade_manifest}" >"${work}/next.json"
  mv -- "${work}/next.json" "${upgrade_manifest}"
done

stage=pebble
# Pebble runs on the host network: Controller containers reach its directory
# on the bridge gateway address, and it validates TLS-ALPN-01 on public 443.
pebble_dir="${work}/pebble"
mkdir -m 0755 "${pebble_dir}"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1 -subj /CN=quick-pebble-ca \
  -addext basicConstraints=critical,CA:TRUE -addext keyUsage=critical,keyCertSign \
  -keyout "${work}/pebble-ca.key" -out "${pebble_dir}/ca.pem" 2>/dev/null
openssl req -new -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj /CN=pebble \
  -keyout "${pebble_dir}/key.pem" -out "${work}/pebble.csr" 2>/dev/null
printf 'subjectAltName=IP:%s,IP:127.0.0.1\nextendedKeyUsage=serverAuth\n' "${gateway}" >"${work}/pebble.ext"
openssl x509 -req -days 1 -in "${work}/pebble.csr" -CA "${pebble_dir}/ca.pem" -CAkey "${work}/pebble-ca.key" \
  -CAcreateserial -extfile "${work}/pebble.ext" -out "${pebble_dir}/cert.pem" 2>/dev/null
jq -n '{pebble: {listenAddress: "0.0.0.0:14000", managementListenAddress: "0.0.0.0:15000",
  certificate: "/pebble/cert.pem", privateKey: "/pebble/key.pem", httpPort: 5002, tlsPort: 443,
  ocspResponderURL: "", externalAccountBindingRequired: false}}' >"${pebble_dir}/config.json"
chmod 0444 "${pebble_dir}"/*
docker run -d --restart unless-stopped --name "${pebble}" --network host \
  -e PEBBLE_VA_NOSLEEP=1 -e PEBBLE_WFE_NONCEREJECT=0 -v "${pebble_dir}:/pebble:ro" \
  "${PEBBLE_IMAGE}" -config /pebble/config.json
for _ in $(seq 30); do
  curl -fsS --cacert "${pebble_dir}/ca.pem" "https://${gateway}:14000/dir" >/dev/null 2>&1 && break
  sleep 1
done
curl -fsS --cacert "${pebble_dir}/ca.pem" "https://${gateway}:14000/dir" >/dev/null

stage=first_install
passphrase="${work}/root-ca-passphrase" export_dir="${work}/root-ca-export"
openssl rand -hex 24 >"${passphrase}"
quick=("${ROOT}/deploy/production/quick-install.sh" --controller-domain "${controller}" --relay-domain "${relay}"
  --acme-email quick-acceptance@ocservia.test --root-ca-passphrase-file "${passphrase}"
  --root-ca-export-dir "${export_dir}" --release-file "${manifest}")
OCSERV_ACME_DIRECTORY_URL="https://${gateway}:14000/dir" OCSERV_ACME_CA_FILE="${pebble_dir}/ca.pem" \
  "${quick[@]}" >"${work}/run-first.log" 2>&1
grep -qxF "Quick install complete: https://${controller}" "${work}/run-first.log"

stage=certificates
curl -fsS --cacert "${pebble_dir}/ca.pem" "https://${gateway}:15000/roots/0" >"${work}/acme-root.pem"
tls_ready() { curl -sS -o /dev/null --max-time 5 --cacert "${work}/acme-root.pem" "https://$1/" 2>/dev/null; }
for _ in $(seq 90); do
  tls_ready "${controller}" && tls_ready "${relay}" && break
  sleep 2
done
tls_ready "${controller}"
tls_ready "${relay}"
served() {
  local name
  for name in "${controller}" "${relay}"; do
    openssl s_client -connect "${gateway}:443" -servername "${name}" </dev/null 2>/dev/null |
      openssl x509 -noout -subject -serial -fingerprint -sha256
  done
}
certificates="$(served)"
pass "ACME certificates for both names verify against the directory root"

stage=accounts
login() {
  local account="$1" status
  rm -f -- "${work}/${account}.cookies"
  status="$(sed -n 's/^Password: //p' "${CREDENTIALS_DIR}/${account}.txt" |
    jq -Rc --arg username "initial-${account}" '{username: $username, password: .}' |
    curl -sS -o /dev/null -w '%{http_code}' --cacert "${work}/acme-root.pem" -c "${work}/${account}.cookies" \
      -H "Origin: https://${controller}" -H 'Content-Type: application/json' --data-binary @- \
      "https://${controller}/api/v1/auth/login")"
  [[ "${status}" == 204 ]] && grep -qF __Host-ocservia_session "${work}/${account}.cookies"
}
accounts() {
  login admin
  login approver
  [[ "$(stat -c '%u:%a' "${CREDENTIALS_DIR}" "${CREDENTIALS_DIR}"/{admin,approver}.txt | sort -u | paste -sd' ')" == "0:400 0:700" ]]
  [[ -e "${CREDENTIALS_DIR}/complete" && ! -e /etc/ocservia/secrets/local-bootstrap-admin-password ]]
}
accounts
pass "initial-admin and initial-approver log in"

stage=custody
# The root CA private key exists only, encrypted, in the export directory.
[[ -s "${export_dir}/root-ca.key.enc" ]]
root_public="$(openssl x509 -in "${export_dir}/root-ca.crt" -noout -pubkey)"
while IFS= read -r -d '' file; do
  grep -qF 'PRIVATE KEY-----' "${file}" || continue
  absent 'ENCRYPTED PRIVATE KEY' "${file}"
  [[ "$(openssl pkey -in "${file}" -pubout 2>/dev/null)" != "${root_public}" ]]
done < <(find /etc/ocservia /var/lib/ocservia-signer /var/lib/ocservia-controller /var/backups/ocservia -type f -print0)
pass "the root CA key is only in the export directory"

stage=ports
published="$(docker ps --filter label=com.docker.compose.project=ocservia-production --format '{{.Ports}}' |
  grep -oE ':[0-9]+->[0-9]+/(tcp|udp)' | sed -E 's/^:([0-9]+)->[0-9]+/\1/' | sort -u | paste -sd' ')"
[[ "${published}" == "443/tcp 7842/udp" ]]
pass "only 443/tcp and 7842/udp are published"

stage=rerun
materials() { find /etc/ocservia "${CREDENTIALS_DIR}" -type f ! -name complete -print0 | sort -z | xargs -0 sha256sum; }
before="$(materials)"
"${quick[@]}" >"${work}/run-rerun.log" 2>&1
grep -qF "initial Local administrators already exist" "${work}/run-rerun.log"
[[ "$(materials)" == "${before}" && "$(served)" == "${certificates}" ]]
accounts
pass "a rerun keeps material and certificates"

stage=bootstrap_resume
# An interruption after Local bootstrap committed but before completion was
# recorded: the rerun reuses the kept passwords and records completion.
rm -- "${CREDENTIALS_DIR}/complete"
"${quick[@]}" >"${work}/run-resume.log" 2>&1
grep -qF "Local authentication was already initialized by an earlier run" "${work}/run-resume.log"
[[ "$(materials)" == "${before}" ]]
accounts
pass "a rerun resumes an unrecorded Local bootstrap"

stage=lifecycle
config_keys=(OCSERV_DEPLOYMENT_MODE OCSERV_DATABASE_BACKEND OCSERV_DATABASE_DEPLOYMENT OCSERV_TLS_MODE
  OCSERV_ACME_EMAIL OCSERV_ACME_DIRECTORY_URL OCSERV_ACME_CA_FILE OCSERV_PUBLIC_HOST OCSERV_RELAY_PUBLIC_HOST
  OCSERV_LOCAL_AUTH_ENABLED OCSERV_SECRET_DIR OCSERV_RELAY_SECRET_DIR OCSERV_SIGNER_SECRET_DIR
  OCSERV_SIGNER_STATE_DIR OCSERV_BACKUP_DIR OCSERV_AUDIT_EVENT_KEY_ID OCSERV_CONTROLLER_ENDPOINT_ID)
lifecycle() {
  (
    # shellcheck source=deploy/lib/install-env.sh disable=SC1091
    source "${ROOT}/deploy/lib/install-env.sh"
    install_env_load /etc/ocservia/install.env "${config_keys[@]}"
    "${ROOT}/deploy/production/controller.sh" "$@"
  ) >"${work}/run-lifecycle.log" 2>&1
  curl -fsS --cacert "${work}/acme-root.pem" "https://${controller}/api/v1/version" |
    jq -e --arg commit "${SOURCE_COMMIT}" --arg version "${VERSION}" '.commit == $commit and .version == $version' >/dev/null
  [[ "$(materials)" == "${before}" && "$(served)" == "${certificates}" ]]
  accounts
}
lifecycle upgrade --release-file "${upgrade_manifest}"
pass "upgrade keeps material, certificates and accounts"
lifecycle rollback
pass "rollback keeps material, certificates and accounts"
lifecycle upgrade --release-file "${upgrade_manifest}"
pass "upgrade after rollback keeps material, certificates and accounts"

stage=output
for account in admin approver; do
  password="$(sed -n 's/^Password: //p' "${CREDENTIALS_DIR}/${account}.txt")"
  [[ -n "${password}" ]]
  absent "${password}" "${work}"/run-*.log
  for container in $(docker ps -aq --filter label=com.docker.compose.project=ocservia-production); do
    docker logs "${container}" >"${work}/container.log" 2>&1
    absent "${password}" "${work}/container.log"
  done
done
pass "passwords appear in no installer output or container log"
echo "Quick acceptance passed"
