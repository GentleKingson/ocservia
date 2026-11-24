#!/usr/bin/env bash
set -euo pipefail

COMPOSE_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

unset COMPOSE_PROFILES COMPOSE_ENV_FILES OCSERV_RELAY_TRUST_SECRET OCSERV_RELAY_TRUST_TARGET \
  OCSERV_DATABASE_NETWORK OCSERV_DATABASE_TLS_CA_FILE OCSERV_DATABASE_BUNDLED_POSTGRES \
  OCSERV_CERTIFICATE_SIGNER_CA_FILE OCSERV_DATABASE_CA_SECRET OCSERV_SIGNER_CA_SECRET \
  OCSERV_RELAY_TEMPLATE OCSERV_RELAY_ACME_VOLUME OCSERV_RELAY_SHARED_ACME_DIR OCSERV_RELAY_ROUTE_PREFIX \
  OCSERV_RELAY_MODE_ARG OCSERV_BACKUP_SSLMODE OCSERV_RELAY_TLS_DIR IROH_RELAY_ACME_CA
export COMPOSE_DISABLE_ENV_FILE=1
deployment_mode="${OCSERV_DEPLOYMENT_MODE:-standalone}"
shared_domain=false
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
        echo "Integrated requires valid lowercase DNS names" >&2
        exit 2
      fi
      IFS=. read -ra labels <<<"${hostname}"
      for label in "${labels[@]}"; do
        (( ${#label} <= 63 )) || exit 2
      done
    done
    if [[ "${OCSERV_PUBLIC_HOST}" == "${OCSERV_RELAY_PUBLIC_HOST}" ]]; then
      shared_domain=true
    fi
    if [[ "${OCSERV_RELAY_URL_A:-https://${OCSERV_RELAY_PUBLIC_HOST}}" != "https://${OCSERV_RELAY_PUBLIC_HOST}" \
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
tls_mode="${OCSERV_TLS_MODE:-manual}"
case "${deployment_mode}:${tls_mode}" in
  *:manual)
    if [[ -n "${OCSERV_ACME_EMAIL:-}${OCSERV_ACME_DIRECTORY_URL:-}${OCSERV_ACME_CA_FILE:-}" ]]; then
      echo "ACME settings require OCSERV_TLS_MODE=acme" >&2
      exit 2
    fi
    ;;
  integrated:acme)
    if [[ ! "${OCSERV_ACME_EMAIL:-}" =~ ^[A-Za-z0-9._%+-]+@[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$ ]]; then
      echo "OCSERV_ACME_EMAIL must be a plain email address" >&2
      exit 2
    fi
    if [[ -n "${OCSERV_ACME_DIRECTORY_URL:-}" && ! "${OCSERV_ACME_DIRECTORY_URL}" =~ ^https://[^[:space:]\"{}]+$ ]]; then
      echo "OCSERV_ACME_DIRECTORY_URL must be an https URL" >&2
      exit 2
    fi
    if [[ -n "${OCSERV_ACME_CA_FILE:-}" ]]; then
      path="${OCSERV_ACME_CA_FILE}"
      if [[ -z "${OCSERV_ACME_DIRECTORY_URL:-}" || "${path}" != /* || ! -f "${path}" || -L "${path}" || ! -s "${path}" \
        || "$(stat -c '%u:%a:%h' "${path}")" != "${EUID}:444:1" ]]; then
        echo "OCSERV_ACME_CA_FILE must be a nonempty launcher-owned mode-0444 single-link file for OCSERV_ACME_DIRECTORY_URL" >&2
        exit 2
      fi
    fi
    ;;
  *) echo "OCSERV_TLS_MODE must be manual, or acme with Integrated" >&2; exit 2 ;;
esac
if [[ "${OCSERV_LOCAL_AUTH_ENABLED:-true}" != true ]]; then
  echo "enable Local authentication (OCSERV_LOCAL_AUTH_ENABLED=true) for production" >&2
  exit 2
fi
otel_enabled=false
if [[ -n "${OCSERV_OTEL_BACKEND_ENDPOINT:-}" ]]; then
  otel_enabled=true
fi

database_backend="${OCSERV_DATABASE_BACKEND:-postgres}"
database_deployment="${OCSERV_DATABASE_DEPLOYMENT:-bundled}"
case "${database_backend}:${database_deployment}" in
  postgres:bundled)
    unset OCSERV_DATABASE_BACKUP_HOST OCSERV_DATABASE_BACKUP_PORT OCSERV_DATABASE_BACKUP_NAME OCSERV_DATABASE_BACKUP_USER
    ;;
  postgres:external)
    : "${OCSERV_DATABASE_BACKUP_HOST:?set the external PostgreSQL backup host}"
    export OCSERV_DATABASE_NETWORK=database-egress OCSERV_DATABASE_BUNDLED_POSTGRES=false
    export OCSERV_DATABASE_CA_SECRET=database_ca OCSERV_DATABASE_TLS_CA_FILE=/run/secrets/database_ca
    export OCSERV_BACKUP_SSLMODE=verify-full
    ;;
  *)
    echo "OCSERV_DATABASE_BACKEND must be postgres and OCSERV_DATABASE_DEPLOYMENT must be bundled or external" >&2
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
  if [[ ! "${value}" =~ ^[^[:space:]@]+@sha256:[0-9a-f]{64}$ ]]; then
    echo "${variable} must contain a sha256 image reference" >&2
    exit 2
  fi
done

declare -A protected_ancestors=()
protected_directory() {
  local path="$1" expected="$2" format="$3" ancestor owner mode
  if [[ "${path}" != /* || ! -d "${path}" || "$(realpath -e -- "${path}")" != "${path}" ||
    "$(stat -c "${format}" -- "${path}")" != "${expected}" ]]; then
    echo "${path} must be a canonical directory with ${format}=${expected}" >&2
    return 2
  fi
  ancestor="$(dirname -- "${path}")"
  while [[ -z "${protected_ancestors[${ancestor}]:-}" ]]; do
    IFS=: read -r owner mode < <(stat -c '%u:%a' -- "${ancestor}")
    if [[ "${owner}" != 0 && "${owner}" != "${EUID}" ]] || (( (8#${mode} & 8#022) != 0 )); then
      echo "${ancestor} must be root- or launcher-owned and protected from other writers" >&2
      return 2
    fi
    protected_ancestors[${ancestor}]=1
    [[ "${ancestor}" != / ]] || break
    ancestor="$(dirname -- "${ancestor}")"
  done
}

secret_dir="${OCSERV_SECRET_DIR:-}"
protected_directory "${secret_dir}" "${EUID}:700" '%u:%a'
general_secrets=(database-owner-url database-app-url session-key audit-checkpoint-key certificate-signer-token)
if [[ "${tls_mode}" == manual ]]; then
  general_secrets+=(tls.crt tls.key)
fi
if [[ "${database_deployment}" == bundled ]]; then
  general_secrets+=(postgres-owner-password postgres-app-password postgres-backup-password postgres.pgpass)
else
  general_secrets+=(postgres.pgpass database-ca.pem)
fi
if [[ "${otel_enabled}" == true ]]; then
  general_secrets+=(otel-client.crt otel-client.key otel-ca.crt)
fi
for secret in "${general_secrets[@]}"; do
  path="${secret_dir}/${secret}"
  if [[ ! -f "${path}" || -L "${path}" || "$(stat -c '%u:%a' "${path}")" != "${EUID}:444" ]]; then
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
  mode=400
  [[ "${secret}" != relay-access-token ]] || mode=440
  if [[ ! -f "${path}" || -L "${path}" || "$(stat -c '%u:%g:%a:%h' "${path}")" != "65532:65532:${mode}:1" ]]; then
    echo "${path} must be a one-link file owned by uid:gid 65532:65532 with mode 0${mode}" >&2
    exit 2
  fi
done

if [[ "${deployment_mode}" == integrated ]]; then
  for variable in OCSERV_RELAY_SECRET_DIR OCSERV_SIGNER_SECRET_DIR OCSERV_SIGNER_STATE_DIR; do
    path="${!variable:-}"
    expected_owner="${EUID}:700"
    [[ "${variable}" == OCSERV_RELAY_SECRET_DIR ]] || expected_owner=65532:700
    protected_directory "${path}" "${expected_owner}" '%u:%a'
  done
  relay_tls=(tls.crt tls.key)
  [[ "${tls_mode}" == manual && "${shared_domain}" == false ]] || relay_tls=()
  for secret in "${relay_tls[@]}"; do
    path="${OCSERV_RELAY_SECRET_DIR}/${secret}"
    [[ -s "${path}" && -f "${path}" && ! -L "${path}" \
      && "$(stat -c '%u:%a:%h' "${path}")" == "${EUID}:444:1" ]] || {
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

compose=(docker compose --env-file /dev/null -p ocservia-production
  -f "${COMPOSE_ROOT}/deploy/production/compose.yaml" --profile "${database_deployment}-postgres")
if [[ "${relay_ca}" == true ]]; then
  export OCSERV_RELAY_TRUST_SECRET=relay_ca OCSERV_RELAY_TRUST_TARGET=relay_ca
fi
if [[ "${deployment_mode}" == integrated ]]; then
  compose+=(--profile "${tls_mode}")
  if [[ -n "${OCSERV_ACME_CA_FILE:-}" ]]; then
    export IROH_RELAY_ACME_CA=/run/secrets/acme_ca
  fi
  export OCSERV_SIGNER_CA_SECRET=certificate_signer_ca OCSERV_CERTIFICATE_SIGNER_CA_FILE=/run/secrets/certificate_signer_ca
  if [[ "${shared_domain}" == true ]]; then
    export OCSERV_RELAY_MODE_ARG=--dev OCSERV_RELAY_TLS_DIR="${secret_dir}"
    export OCSERV_RELAY_ROUTE_PREFIX=relay
    if [[ "${tls_mode}" == acme ]]; then
      export OCSERV_RELAY_ACME_VOLUME=gateway-acme:/var/lib/caddy-acme:ro
      export OCSERV_RELAY_SHARED_ACME_DIR=/var/lib/caddy-acme OCSERV_RELAY_TEMPLATE=./integrated/relay.shared-acme.toml
    fi
  fi
else
  compose+=(--profile standalone)
fi
if [[ "${otel_enabled}" == true ]]; then
  compose+=(--profile observability)
fi

backup_checked=false
production_compose() {
  local argument
  for argument; do
    case "${argument}" in
      build|--build) echo "production lifecycle only consumes prebuilt images" >&2; return 2 ;;
    esac
  done
  if [[ "${1:-}" == recreate-database-clients ]]; then
    if [[ "${database_deployment}" != bundled || $# != 2 || ! "$2" =~ ^[1-9][0-9]*$ ]]; then
      echo "usage for bundled PostgreSQL: recreate-database-clients <timeout-seconds>" >&2
      return 2
    fi
    protected_directory "${OCSERV_BACKUP_DIR:-}" 999:999:700 '%u:%g:%a' || return
    "${compose[@]}" up -d --force-recreate --no-deps --wait --wait-timeout "$2" control-plane backup
    return
  fi
  case "${1:-}" in
    run|create|up)
      if [[ "${backup_checked}" == false ]]; then
        protected_directory "${OCSERV_BACKUP_DIR:-}" 999:999:700 '%u:%g:%a' || return
        backup_checked=true
      fi ;;
    down)
      "${compose[@]}" --profile observability "$@"
      return ;;
  esac
  if [[ "${1:-}" == up ]]; then
    if [[ "${otel_enabled}" == false ]]; then
      "${compose[@]}" --profile observability rm --stop --force otel-collector
    fi
    # Release the public sockets and static IP held by a pre-profile gateway
    # or relay when upgrading a standalone or ACME deployment.
    if [[ "${deployment_mode}" == standalone ]]; then
      "${compose[@]}" --profile manual rm --stop --force gateway
    elif [[ "${tls_mode}" == acme ]]; then
      "${compose[@]}" --profile manual rm --stop --force gateway relay
    fi
    "${compose[@]}" stop control-plane transportd
    "${compose[@]}" run --rm --no-deps transport-runtime-init
  fi
  "${compose[@]}" "$@"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  production_compose "$@"
fi
