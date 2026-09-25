#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

unset COMPOSE_PROFILES COMPOSE_ENV_FILES
export COMPOSE_DISABLE_ENV_FILE=1
deployment_mode="${OCSERV_DEPLOYMENT_MODE:-standalone}"
case "${deployment_mode}" in
  standalone) ;;
  integrated)
    if (( EUID != 0 )); then
      echo "Integrated requires the explicit root lifecycle to inspect UID-65532 private Signer material" >&2
      exit 2
    fi
    compose_version="$(docker compose version --short)"
    compose_version="${compose_version#v}"
    if [[ ! "${compose_version}" =~ ^([0-9]+)\.([0-9]+)\.([0-9]+)([-+].*)?$ ]] \
      || (( BASH_REMATCH[1] < 2 || (BASH_REMATCH[1] == 2 && (BASH_REMATCH[2] < 24 || (BASH_REMATCH[2] == 24 && BASH_REMATCH[3] < 4))) )); then
      echo "Integrated requires Compose >= 2.24.4" >&2
      exit 2
    fi
    for hostname in "${OCSERV_PUBLIC_HOST:-}" "${OCSERV_RELAY_PUBLIC_HOST:-}"; do
      if [[ ${#hostname} -gt 253 || ! "${hostname}" =~ ^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$ ]]; then
        echo "Integrated requires two lowercase DNS names" >&2
        exit 2
      fi
      IFS=. read -ra labels <<<"${hostname}"
      for label in "${labels[@]}"; do
        (( ${#label} <= 63 )) || exit 2
      done
    done
    if [[ "${OCSERV_PUBLIC_HOST}" == "${OCSERV_RELAY_PUBLIC_HOST}" \
      || -n "${OCSERV_RELAY_URL_B:-}" \
      || "${OCSERV_RELAY_URL_A:-https://${OCSERV_RELAY_PUBLIC_HOST}}" != "https://${OCSERV_RELAY_PUBLIC_HOST}" \
      || "${OCSERV_CERTIFICATE_SIGNER_URL:-https://signer:9443/sign}" != https://signer:9443/sign \
      || "${OCSERV_PUBLIC_ORIGIN:-https://${OCSERV_PUBLIC_HOST}}" != "https://${OCSERV_PUBLIC_HOST}" \
      || "${OCSERV_AUTH_TRUSTED_PROXY_CIDRS-${OCSERV_GATEWAY_APPLICATION_IP:-172.30.240.2}/32}" != "${OCSERV_GATEWAY_APPLICATION_IP:-172.30.240.2}/32" ]]; then
      echo "conflicting Integrated hostname, Relay, Signer, origin or proxy trust configuration" >&2
      exit 2
    fi
    export OCSERV_RELAY_URL_A="https://${OCSERV_RELAY_PUBLIC_HOST}"
    export OCSERV_CERTIFICATE_SIGNER_URL=https://signer:9443/sign
    ;;
  *) echo "OCSERV_DEPLOYMENT_MODE must be standalone or integrated" >&2; exit 2 ;;
esac
oidc_enabled=false
if [[ -n "${OCSERV_OIDC_ISSUER:-}${OCSERV_OIDC_CLIENT_ID:-}${OCSERV_OIDC_REDIRECT_URL:-}" ]]; then
  oidc_enabled=true
fi
if [[ "${oidc_enabled}" == false && "${OCSERV_LOCAL_AUTH_ENABLED:-false}" != true ]]; then
  echo "enable Local authentication or configure OIDC for production" >&2
  exit 2
fi
otel_enabled=false
if [[ -n "${OCSERV_OTEL_BACKEND_ENDPOINT:-}" ]]; then
  otel_enabled=true
fi

database_backend="${OCSERV_DATABASE_BACKEND:-postgres}"
database_deployment="${OCSERV_DATABASE_DEPLOYMENT:-bundled}"
case "${database_backend}:${database_deployment}" in
  postgres:bundled) database_overlay="compose.postgres.yaml" ;;
  postgres:external) database_overlay="compose.external-postgres.yaml" ;;
  mysql:external|mariadb:external) database_overlay="compose.external-mysql.yaml" ;;
  mysql:bundled|mariadb:bundled)
    echo "bundled ${database_backend} is not implemented; use an externally managed database" >&2
    exit 2
    ;;
  *)
    echo "OCSERV_DATABASE_BACKEND must be postgres, mysql or mariadb and OCSERV_DATABASE_DEPLOYMENT must be bundled or external" >&2
    exit 2
    ;;
esac

image_variables=(OCSERV_GATEWAY_IMAGE OCSERV_CONTROL_IMAGE OCSERV_TRANSPORT_IMAGE
  OCSERV_BACKUP_IMAGE OCSERV_POSTGRES_IMAGE OCSERV_OTEL_IMAGE)
if [[ "${deployment_mode}" == integrated ]]; then
  image_variables+=(OCSERV_EDGE_IMAGE OCSERV_RELAY_IMAGE OCSERV_SIGNER_IMAGE)
fi
for variable in "${image_variables[@]}"; do
  value="${!variable:-}"
  if [[ ! "${value}" =~ ^[^[:space:]]+@sha256:[0-9a-f]{64}$ ]]; then
    echo "${variable} must contain a full sha256 image digest" >&2
    exit 2
  fi
done
if [[ "${database_backend}" == mysql || "${database_backend}" == mariadb ]]; then
  if [[ ! "${OCSERV_DATABASE_BACKUP_IMAGE:-}" =~ ^[^[:space:]]+@sha256:[0-9a-f]{64}$ ]]; then
    echo "OCSERV_DATABASE_BACKUP_IMAGE must contain a full sha256 image digest" >&2
    exit 2
  fi
fi

secret_dir="${OCSERV_SECRET_DIR:-}"
if [[ -z "${secret_dir}" || ! -d "${secret_dir}" || -L "${secret_dir}" \
  || "$(stat -c '%u:%a' "${secret_dir}")" != "$(id -u):700" ]]; then
  echo "OCSERV_SECRET_DIR must be an existing mode-0700 directory owned by the launcher user" >&2
  exit 2
fi
if [[ "${secret_dir}" != /* || "$(realpath -e -- "${secret_dir}")" != "${secret_dir}" ]]; then
  echo "OCSERV_SECRET_DIR must be an absolute canonical path without symlink ancestry" >&2
  exit 2
fi
ancestor="${secret_dir}"
while true; do
  IFS=: read -r ancestor_uid ancestor_mode < <(stat -c '%u:%a' "${ancestor}")
  if [[ "${ancestor_uid}" != "0" && "${ancestor_uid}" != "$(id -u)" ]] \
    || (( (8#${ancestor_mode} & 8#022) != 0 )); then
    echo "OCSERV_SECRET_DIR ancestry must be root- or launcher-owned and not group/world writable" >&2
    exit 2
  fi
  [[ "${ancestor}" == "/" ]] && break
  ancestor="$(dirname -- "${ancestor}")"
done
general_secrets=(tls.crt tls.key database-owner-url database-app-url session-key \
  audit-checkpoint-key certificate-signer-token)
if [[ "${database_backend}:${database_deployment}" == postgres:bundled ]]; then
  general_secrets+=(postgres-owner-password postgres-app-password postgres-backup-password postgres.pgpass)
elif [[ "${database_backend}" == postgres ]]; then
  general_secrets+=(postgres.pgpass database-ca.pem)
else
  general_secrets+=(database-backup.cnf database-ca.pem)
fi
if [[ "${oidc_enabled}" == true ]]; then
  general_secrets+=(oidc-client-secret)
fi
if [[ "${otel_enabled}" == true ]]; then
  general_secrets+=(otel-client.crt otel-client.key otel-ca.crt)
fi
for secret in "${general_secrets[@]}"; do
  path="${secret_dir}/${secret}"
  if [[ ! -f "${path}" || -L "${path}" || "$(stat -c '%u:%a' "${path}")" != "$(id -u):444" ]]; then
    echo "${path} must be a launcher-owned regular file with mode 0444 inside the private secret directory" >&2
    exit 2
  fi
done
for secret in audit-event-key controller-command-signing-key.pem; do
  path="${secret_dir}/${secret}"
  if [[ ! -f "${path}" || -L "${path}" \
    || "$(stat -c '%u:%g:%a' "${path}")" != "65534:65532:400" ]]; then
    echo "${path} must be owned by uid:gid 65534:65532 with mode 0400" >&2
    exit 2
  fi
done
path="${secret_dir}/controller-command-verification-key.pem"
if [[ ! -f "${path}" || -L "${path}" || "$(stat -c '%u:%g:%a:%h' "${path}")" != "0:65532:440:1" ]]; then
  echo "${path} must be a one-link root:65532 regular file with mode 0440" >&2
  exit 2
fi
relay_ca=false
path="${secret_dir}/relay-ca.pem"
if [[ -e "${path}" || -L "${path}" ]]; then
  if [[ ! -f "${path}" || -L "${path}" || ! -s "${path}" \
    || "$(stat -c '%u:%g:%a:%h' "${path}")" != "0:0:444:1" ]]; then
    echo "${path} must be a nonempty one-link root:root regular file with mode 0444" >&2
    exit 2
  fi
  relay_ca=true
fi
for secret in relay-access-token controller-iroh.key; do
  path="${secret_dir}/${secret}"
  if [[ ! -f "${path}" || -L "${path}" || "$(stat -c '%u:%g:%a' "${path}")" != "65532:65532:400" ]]; then
    echo "${path} must be owned by uid:gid 65532:65532 with mode 0400" >&2
    exit 2
  fi
done

if [[ "${deployment_mode}" == integrated ]]; then
  for variable in OCSERV_RELAY_SECRET_DIR OCSERV_SIGNER_SECRET_DIR OCSERV_SIGNER_STATE_DIR; do
    path="${!variable:-}"
    expected_owner="$(id -u):700"
    [[ "${variable}" == OCSERV_RELAY_SECRET_DIR ]] || expected_owner=65532:700
    if [[ "${path}" != /* || ! -d "${path}" || -L "${path}" \
      || "$(realpath -e -- "${path}")" != "${path}" \
      || "$(stat -c '%u:%a' "${path}")" != "${expected_owner}" ]]; then
      echo "${variable} must be a canonical mode-0700 directory with owner ${expected_owner%:*}" >&2
      exit 2
    fi
    ancestor="$(dirname -- "${path}")"
    while true; do
      IFS=: read -r ancestor_uid ancestor_mode < <(stat -c '%u:%a' "${ancestor}")
      if [[ "${ancestor_uid}" != 0 && "${ancestor_uid}" != "$(id -u)" ]] \
        || (( (8#${ancestor_mode} & 8#022) != 0 )); then
        echo "Integrated directory ancestry must be protected" >&2
        exit 2
      fi
      [[ "${ancestor}" == / ]] && break
      ancestor="$(dirname -- "${ancestor}")"
    done
  done
  for secret in tls.crt tls.key; do
    path="${OCSERV_RELAY_SECRET_DIR}/${secret}"
    [[ -s "${path}" && -f "${path}" && ! -L "${path}" \
      && "$(stat -c '%u:%a:%h' "${path}")" == "$(id -u):444:1" ]] || {
      echo "Relay TLS files must be nonempty launcher-owned mode-0444 single-link files" >&2; exit 2;
    }
  done
  for secret in issuer-chain.pem issuer-key.pem tls-cert.pem tls-key.pem api-token tls-ca.pem; do
    path="${OCSERV_SIGNER_SECRET_DIR}/${secret}"
    mode=400
    [[ "${secret}" != tls-ca.pem ]] || mode=444
    [[ -s "${path}" && -f "${path}" && ! -L "${path}" \
      && "$(stat -c '%u:%g:%a:%h' "${path}")" == "65532:65532:${mode}:1" ]] || {
      echo "invalid Signer secret ownership, mode or link count: ${secret}" >&2; exit 2;
    }
  done
  cmp -s "${OCSERV_SIGNER_SECRET_DIR}/api-token" "${secret_dir}/certificate-signer-token" || {
    echo "Controller and Signer API tokens differ" >&2; exit 2;
  }
fi

prepare_transport_runtime=false
teardown=false
for argument in "$@"; do
  case "${argument}" in
    build|--build)
      echo "production lifecycle only consumes prebuilt digest-pinned images" >&2
      exit 2
      ;;
  esac
done
for argument in "$@"; do
  case "${argument}" in
    down)
      teardown=true
      break
      ;;
    up|create|run)
      backup_dir="${OCSERV_BACKUP_DIR:-}"
      if [[ -z "${backup_dir}" || ! -d "${backup_dir}" || -L "${backup_dir}" ]]; then
        echo "OCSERV_BACKUP_DIR must be an existing real directory" >&2
        exit 2
      fi
      if [[ "$(stat -c '%u:%g:%a' "${backup_dir}")" != "999:999:700" ]]; then
        echo "OCSERV_BACKUP_DIR must be owned by uid:gid 999:999 with mode 0700" >&2
        exit 2
      fi
      if [[ "${argument}" == "up" ]]; then
        prepare_transport_runtime=true
      fi
      break
      ;;
  esac
done

compose=(docker compose --env-file /dev/null -p ocservia-production
  -f "${ROOT}/deploy/production/compose.yaml"
  -f "${ROOT}/deploy/production/${database_overlay}")
if [[ "${oidc_enabled}" == true ]]; then
  compose+=(-f "${ROOT}/deploy/production/compose.oidc.yaml")
fi
if [[ "${relay_ca}" == true ]]; then
  compose+=(-f "${ROOT}/deploy/production/compose.relay-ca.yaml")
fi
if [[ "${deployment_mode}" == integrated ]]; then
  compose+=(-f "${ROOT}/deploy/production/integrated/compose.signer.yaml"
    -f "${ROOT}/deploy/production/integrated/compose.yaml")
fi
if [[ "${otel_enabled}" == true || "${teardown}" == true ]]; then
  compose+=(--profile observability)
fi
if [[ "${deployment_mode}" == integrated ]]; then
  # Check the merged model, not individual overlays or Compose list-merge assumptions.
  "${compose[@]}" config --format json | jq -e '
    ([.services | to_entries[] | .key as $service | .value.ports[]? |
      [$service, .target, (.published | tostring), .protocol]] | sort) ==
      [["edge", 8443, "443", "tcp"], ["relay", 7842, "7842", "udp"]] and
    all(.services[]; (has("build") | not) and (.image | test("^[^\\s@]+@sha256:[0-9a-f]{64}$"))) and
    (.networks.signer.internal == true) and
    (.services.signer.networks | keys == ["signer"]) and
    (.services["control-plane"].environment.OCSERV_CERTIFICATE_SIGNER_CA_FILE == "/run/secrets/certificate_signer_ca")
  ' >/dev/null || { echo "Integrated rendered deployment contract rejected" >&2; exit 2; }
fi
if [[ "${prepare_transport_runtime}" == true ]]; then
  if [[ "${otel_enabled}" == false ]]; then
    "${compose[@]}" --profile observability rm --stop --force otel-collector
  fi
  "${compose[@]}" stop control-plane transportd
  "${compose[@]}" run --rm --no-deps transport-runtime-init
fi

exec "${compose[@]}" "$@"
