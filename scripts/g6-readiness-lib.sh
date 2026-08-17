#!/usr/bin/env bash
# Shared helpers for the two failure-domain sides of the formal G6
# production-readiness harness. Sourced by scripts/g6-readiness-fd-a.sh and
# scripts/g6-readiness-fd-b.sh; never executed directly.
#
# Era model: era 1 runs the full control plane on fd-a (first PostgreSQL
# primary, relay-a, transportd #1) while fd-b streams a standby and hosts
# relay-b; era 2 promotes fd-b's standby and runs the full control plane
# there (transportd #2 reuses the controller endpoint key handed over by
# rendezvous, so every agent redials the same controller NodeId). Topology
# role coverage across failure domains comes from instances started in both
# eras; concurrent safety comes from the fencing, leadership, and relay
# takeovers exercised between the eras.

g6rd_init_environment() {
  G6RD_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
  # shellcheck source=scripts/env.sh disable=SC1091
  source "${G6RD_ROOT}/scripts/env.sh"
  RUN_ID="${RUN_ID:?RUN_ID is required}"
  FD_ID="${FD_ID:?FD_ID is required (fd-a or fd-b)}"
  FD_ALIAS="${FD_ALIAS:?FD_ALIAS is required (fd-alpha or fd-beta)}"
  ARTIFACT_DIR="${ARTIFACT_DIR:-${RUNNER_TEMP:?RUNNER_TEMP is required}/artifacts/g6-readiness-${FD_ID}}"
  G6_AUTHORITY="${G6_AUTHORITY:?G6_AUTHORITY is required (engineering or production_readiness)}"
  case "${G6_AUTHORITY}" in
    engineering | production_readiness) ;;
    *)
      echo "G6_AUTHORITY must be engineering or production_readiness" >&2
      return 1
      ;;
  esac
  G6RD_CANDIDATE_SHA="${G6RD_CANDIDATE_SHA:?G6RD_CANDIDATE_SHA is required}"
  # Same derivation as the frozen verifier pattern: the environment id binds
  # both failure domains to one shared run identity.
  local shared_run="${RUN_ID%-fd-[a-b]}"
  G6RD_ENVIRONMENT_ID="${G6RD_ENVIRONMENT_ID:-g6-$(printf '%s' "${shared_run}" | openssl dgst -sha256 -r | cut -c1-16)}"
  if [[ ! "${G6RD_ENVIRONMENT_ID}" =~ ^g6-[a-z0-9]{8,32}$ ]]; then
    echo "environment id ${G6RD_ENVIRONMENT_ID} violates the frozen g6-[a-z0-9]{8,32} pattern" >&2
    return 1
  fi
  case "${RUN_ID}${FD_ID}${FD_ALIAS}${G6RD_CANDIDATE_SHA}" in
    *[!a-zA-Z0-9._-]*)
      echo "run or failure-domain IDs contain unsafe characters" >&2
      return 1
      ;;
  esac
  [[ "${FD_ID}" == "fd-a" || "${FD_ID}" == "fd-b" ]] || {
    echo "FD_ID must be fd-a or fd-b" >&2
    return 1
  }
  G6RD_WORK="${RUNNER_TEMP}/g6-readiness-${RUN_ID}"
  G6RD_STATE="${G6RD_WORK}/state"
  G6RD_SECRETS="${G6RD_WORK}/secrets"
  G6RD_OUTBOX="${G6RD_WORK}/outbox"
  G6RD_ARCHIVE="${G6RD_WORK}/pgarchive"
  G6RD_BASEBACKUP="${G6RD_WORK}/basebackup"
  G6RD_RESTORE="${G6RD_WORK}/restore"
  G6RD_LOGS="${G6RD_WORK}/logs"
  G6RD_AGENTS="${G6RD_WORK}/agents"
  G6RD_RESULT_BARRIER="${G6RD_WORK}/result-barrier"
  G6RD_TUNNEL_BIN="${G6RD_ROOT}/rust/target/release/ocservia-g6-tunnel"
  COMPOSE_PROJECT="ocservia-g6-rd-${RUN_ID}"
  COMPOSE_FILE="${G6RD_ROOT}/deploy/g6-readiness/compose.yaml"
  G6RD_AGENT_COMPOSE="${G6RD_WORK}/agents-${FD_ID}.yaml"
  export COMPOSE_PROJECT COMPOSE_FILE RUN_ID FD_ID FD_ALIAS G6_AUTHORITY
  umask 077
  mkdir -p "${G6RD_STATE}" "${G6RD_SECRETS}" "${G6RD_OUTBOX}" \
    "${G6RD_ARCHIVE}" "${G6RD_BASEBACKUP}" "${G6RD_RESTORE}" \
    "${G6RD_LOGS}" "${G6RD_AGENTS}" "${G6RD_RESULT_BARRIER}" "${ARTIFACT_DIR}"
  chmod 0700 "${G6RD_WORK}" "${G6RD_SECRETS}"
  chmod 0777 "${G6RD_RESULT_BARRIER}"
}

g6rd_now() {
  date -u +%Y-%m-%dT%H:%M:%SZ
}

g6rd_uuidv7() {
  local timestamp random
  timestamp="$(node -p 'Date.now().toString(16).padStart(12,"0")')"
  random="$(openssl rand -hex 9)"
  printf '%s-%s-7%s-8%s-%s\n' \
    "${timestamp:0:8}" "${timestamp:8:4}" \
    "${random:0:3}" "${random:3:3}" "${random:6:12}"
}

g6rd_extract_pitr_marker_row() {
  local raw="${1:?psql marker output is required}" row
  row="${raw%%$'\n'*}"
  if [[ ! "${row}" =~ ^[0-9]+:[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]{6}Z$ ]]; then
    echo "PITR marker output does not match txid:RFC3339-microseconds" >&2
    printf '%s\n' "${row}" | od -c | head -5 >&2
    return 1
  fi
  printf '%s\n' "${row}"
}

g6rd_secret() {
  local name="${1:?secret name is required}"
  local path="${G6RD_SECRETS}/${name}"
  [[ -s "${path}" ]] || return 1
  cat -- "${path}"
}

# ---------------------------------------------------------------------------
# Secrets. All credentials are test-only, generated per run, and shared to
# the peer failure domain only through short-lived workflow artifacts.
# ---------------------------------------------------------------------------

g6rd_generate_passwords() {
  local name
  for name in owner-password app-password replication-password dev-auth-token relay-token \
    oidc-client-secret; do
    [[ -s "${G6RD_SECRETS}/${name}" ]] && continue
    openssl rand -hex 24 >"${G6RD_SECRETS}/${name}"
    chmod 0600 "${G6RD_SECRETS}/${name}"
  done
}

g6rd_seal_session_cookie() {
  local session_id="${1:?session id is required}"
  local identity_id="${2:?identity id is required}"
  node --input-type=module - "${G6RD_SECRETS}/session-key" \
    "${session_id}" "${identity_id}" <<'NODE'
import { createCipheriv, randomBytes } from "node:crypto";
import { readFileSync } from "node:fs";

const [, , keyPath, sessionID, identityID] = process.argv;
const key = Buffer.from(readFileSync(keyPath, "utf8").trim(), "hex");
const nonce = randomBytes(12);
const cipher = createCipheriv("aes-256-gcm", key, nonce);
const plaintext = Buffer.from(JSON.stringify({
  SessionID: sessionID,
  IdentityID: identityID,
  ExpiresAt: new Date(Date.now() + 7 * 60 * 60 * 1000).toISOString(),
}));
const ciphertext = Buffer.concat([cipher.update(plaintext), cipher.final()]);
process.stdout.write(Buffer.concat([
  nonce,
  ciphertext,
  cipher.getAuthTag(),
]).toString("base64url"));
NODE
}

g6rd_generate_auth_fixtures() {
  [[ -s "${G6RD_SECRETS}/session-key" ]] || {
    openssl rand -hex 32 >"${G6RD_SECRETS}/session-key"
    chmod 0600 "${G6RD_SECRETS}/session-key"
  }
  local role identity_id session_id
  for role in requester approver; do
    if [[ ! -s "${G6RD_SECRETS}/${role}-identity-id" ]]; then
      g6rd_uuidv7 >"${G6RD_SECRETS}/${role}-identity-id"
      g6rd_uuidv7 >"${G6RD_SECRETS}/${role}-session-id"
    fi
    identity_id="$(g6rd_secret "${role}-identity-id")"
    session_id="$(g6rd_secret "${role}-session-id")"
    if [[ ! -s "${G6RD_SECRETS}/${role}-session-cookie" ]]; then
      g6rd_seal_session_cookie "${session_id}" "${identity_id}" \
        >"${G6RD_SECRETS}/${role}-session-cookie"
    fi
    chmod 0600 "${G6RD_SECRETS}/${role}-identity-id" \
      "${G6RD_SECRETS}/${role}-session-id" \
      "${G6RD_SECRETS}/${role}-session-cookie"
  done
}

g6rd_generate_relay_tls() {
  [[ -s "${G6RD_SECRETS}/relay-ca.key" ]] && return 0
  local ca_subject='/O=ocservia-g6/CN=g6 readiness relay CA'
  openssl req -x509 -newkey rsa:2048 -sha256 -days 2 -nodes \
    -keyout "${G6RD_SECRETS}/relay-ca.key" \
    -out "${G6RD_SECRETS}/relay-ca.pem" -subj "${ca_subject}" \
    -addext 'basicConstraints=critical,CA:TRUE' \
    -addext 'keyUsage=critical,keyCertSign,cRLSign' >/dev/null 2>&1
  # One leaf serves both relay hostnames; every client (transportd, agent,
  # probe) validates it against the relay CA via --relay-ca-file.
  openssl req -newkey ed25519 -nodes \
    -keyout "${G6RD_SECRETS}/relay-leaf.key" \
    -out "${G6RD_SECRETS}/relay-leaf.csr" \
    -subj '/O=ocservia-g6/CN=relay' >/dev/null 2>&1
  openssl x509 -req -in "${G6RD_SECRETS}/relay-leaf.csr" \
    -CA "${G6RD_SECRETS}/relay-ca.pem" \
    -CAkey "${G6RD_SECRETS}/relay-ca.key" \
    -CAcreateserial -days 2 -sha256 \
    -extfile <(printf 'subjectAltName=DNS:relay-a,DNS:relay-b\nextendedKeyUsage=serverAuth\n') \
    -out "${G6RD_SECRETS}/relay-leaf.crt" >/dev/null 2>&1
  cat "${G6RD_SECRETS}/relay-leaf.crt" "${G6RD_SECRETS}/relay-ca.pem" \
    >"${G6RD_SECRETS}/relay-chain.crt"
  chmod 0600 "${G6RD_SECRETS}"/relay-*.key
  rm -f "${G6RD_SECRETS}/relay-leaf.csr"
}

g6rd_generate_command_signing_key() {
  [[ -s "${G6RD_SECRETS}/command-signing.pem" ]] && return 0
  # The Controller signs command fences and session grants with this
  # Ed25519 key; the same PKCS#8 form the Go loader requires.
  openssl genpkey -algorithm ed25519 \
    -out "${G6RD_SECRETS}/command-signing.pem" >/dev/null 2>&1
  openssl pkey -in "${G6RD_SECRETS}/command-signing.pem" -pubout \
    -out "${G6RD_SECRETS}/command-verification.pem" >/dev/null 2>&1
  chmod 0600 "${G6RD_SECRETS}/command-signing.pem"
}

g6rd_generate_seal_keys() {
  # Two distinct RSA pairs: privd startup requires a user-password seal key
  # and a p12 sealing pair, each with the public-key SHA-256 declared.
  local pair name
  for pair in user-password p12; do
    [[ -s "${G6RD_SECRETS}/seal-${pair}.key" ]] && continue
    openssl genpkey -algorithm rsa -pkeyopt rsa_keygen_bits:2048 \
      -out "${G6RD_SECRETS}/seal-${pair}.key" >/dev/null 2>&1
    openssl pkey -in "${G6RD_SECRETS}/seal-${pair}.key" -pubout \
      -out "${G6RD_SECRETS}/seal-${pair}-public.pem" >/dev/null 2>&1
    openssl dgst -sha256 -binary "${G6RD_SECRETS}/seal-${pair}-public.pem" \
      | xxd -p -c 64 >"${G6RD_SECRETS}/seal-${pair}-sha256"
    chmod 0600 "${G6RD_SECRETS}/seal-${pair}.key"
  done
}

g6rd_generate_controller_key() {
  [[ -s "${G6RD_SECRETS}/controller.key" ]] || {
    dd if=/dev/urandom of="${G6RD_SECRETS}/controller.key" bs=32 count=1 status=none
    chmod 0600 "${G6RD_SECRETS}/controller.key"
  }
}

g6rd_generate_secrets() {
  g6rd_generate_passwords
  g6rd_generate_auth_fixtures
  g6rd_generate_relay_tls
  g6rd_generate_command_signing_key
  g6rd_generate_seal_keys
  [[ "${FD_ID}" == "fd-a" ]] && g6rd_generate_controller_key
}

# The per-FD relay secret bundle mounted into the relay container.
g6rd_materialize_relay_dir() {
  local dir="${G6RD_WORK}/relay-secrets"
  if [[ -s "${dir}/relay.crt" && -s "${dir}/relay.key" \
    && -s "${dir}/relay-ca.pem" && -s "${dir}/relay-token" ]]; then
    printf '%s\n' "${dir}"
    return 0
  fi
  rm -rf -- "${dir}"
  mkdir -p "${dir}"
  cp -f "${G6RD_SECRETS}/relay-chain.crt" "${dir}/relay.crt"
  cp -f "${G6RD_SECRETS}/relay-leaf.key" "${dir}/relay.key"
  cp -f "${G6RD_SECRETS}/relay-ca.pem" "${dir}/relay-ca.pem"
  cp -f "${G6RD_SECRETS}/relay-token" "${dir}/relay-token"
  chmod 0644 "${dir}/relay.crt" "${dir}/relay-ca.pem"
  chmod 0600 "${dir}/relay.key" "${dir}/relay-token"
  docker run --rm --network none --log-driver none \
    -v "${dir}:/fix" postgres:17.10-bookworm \
    sh -c 'chown 65532:65532 /fix/*; chmod 0755 /fix' >/dev/null
  printf '%s\n' "${dir}"
}

# The per-FD signing dir bind-mounted read-only into every role container.
# Its root-owned ancestry is accepted by every consumer; command-signing.pem
# is process-owned (0400) for the Go roles, while the verification copies
# carry the ownership privd (root) and the agent (65532:65532 0640) require.
g6rd_materialize_signing_dir() {
  local dir="${G6RD_WORK}/signing"
  if [[ -s "${dir}/command-signing.pem" \
    && -s "${dir}/command-verification.pem" \
    && -s "${dir}/command-verification-agent.pem" \
    && -s "${dir}/command-verification-privd.pem" \
    && -s "${dir}/session-key" \
    && -s "${dir}/oidc-client-secret" ]]; then
    printf '%s\n' "${dir}"
    return 0
  fi
  rm -rf -- "${dir}"
  mkdir -p "${dir}"
  cp -f "${G6RD_SECRETS}/command-signing.pem" "${dir}/command-signing.pem"
  cp -f "${G6RD_SECRETS}/command-verification.pem" "${dir}/command-verification.pem"
  cp -f "${G6RD_SECRETS}/command-verification.pem" "${dir}/command-verification-agent.pem"
  cp -f "${G6RD_SECRETS}/command-verification.pem" "${dir}/command-verification-privd.pem"
  cp -f "${G6RD_SECRETS}/session-key" "${dir}/session-key"
  cp -f "${G6RD_SECRETS}/oidc-client-secret" "${dir}/oidc-client-secret"
  chmod 0400 "${dir}/command-signing.pem" "${dir}/session-key" \
    "${dir}/oidc-client-secret"
  chmod 0640 "${dir}/command-verification.pem" \
    "${dir}/command-verification-agent.pem" "${dir}/command-verification-privd.pem"
  docker run --rm --network none --log-driver none \
    -v "${dir}:/fix" postgres:17.10-bookworm sh -c \
    'chown 0:65532 /fix; chmod 0755 /fix; chown 65534:65532 /fix/command-signing.pem /fix/session-key /fix/oidc-client-secret; chown 0:65532 /fix/command-verification.pem /fix/command-verification-agent.pem /fix/command-verification-privd.pem' \
    >/dev/null
  printf '%s\n' "${dir}"
}

g6rd_materialize_probe_controller_key_dir() {
  local dir="${G6RD_WORK}/probe-controller-key"
  if [[ ! -s "${dir}/controller.key" ]]; then
    rm -rf -- "${dir}"
    mkdir -p "${dir}"
    cp -f "${G6RD_SECRETS}/controller.key" "${dir}/controller.key"
    chmod 0400 "${dir}/controller.key"
    docker run --rm --network none --log-driver none \
      -v "${dir}:/fix" postgres:17.10-bookworm sh -c \
      'chmod 0755 /fix; chown 65534:65532 /fix/controller.key' \
      >/dev/null
  fi
  printf '%s\n' "${dir}"
}

# ---------------------------------------------------------------------------
# Compose. Every phase runs as its own process, so substituted variables
# must be exported even for diagnostics and cleanup steps.
# ---------------------------------------------------------------------------

g6rd_relay_url_a() {
  # relay-a is local on fd-a and reached through a tunnel forward on fd-b.
  if [[ "${FD_ID}" == "fd-a" ]]; then
    printf 'https://relay-a:%s\n' "${G6_RELAY_A_PORT:-3443}"
  else
    printf 'https://relay-a:%s\n' "${G6_RELAY_A_FORWARD_PORT:-3444}"
  fi
}

g6rd_relay_url_b() {
  # relay-b is local on fd-b and reached through a tunnel forward on fd-a.
  if [[ "${FD_ID}" == "fd-b" ]]; then
    printf 'https://relay-b:%s\n' "${G6_RELAY_B_PORT:-3443}"
  else
    printf 'https://relay-b:%s\n' "${G6_RELAY_B_FORWARD_PORT:-3445}"
  fi
}

g6rd_export_common_env() {
  local required
  for required in owner-password app-password replication-password dev-auth-token \
    oidc-client-secret session-key requester-identity-id requester-session-id \
    requester-session-cookie approver-identity-id approver-session-id \
    approver-session-cookie \
    command-signing.pem command-verification.pem relay-chain.crt relay-leaf.key \
    relay-ca.pem relay-token; do
    [[ -s "${G6RD_SECRETS}/${required}" ]] || return 1
  done
  export G6_FD_ID="${FD_ID}"
  export G6_OWNER_PASSWORD="${G6_OWNER_PASSWORD:-$(g6rd_secret owner-password)}"
  export G6_APP_PASSWORD="${G6_APP_PASSWORD:-$(g6rd_secret app-password)}"
  export G6_REPLICATION_PASSWORD="${G6_REPLICATION_PASSWORD:-$(g6rd_secret replication-password)}"
  export G6_DEV_AUTH_TOKEN="${G6_DEV_AUTH_TOKEN:-$(g6rd_secret dev-auth-token)}"
  export G6_ARCHIVE_DIR="${G6_ARCHIVE_DIR:-${G6RD_ARCHIVE}}"
  export G6_BASEBACKUP_DIR="${G6_BASEBACKUP_DIR:-${G6RD_BASEBACKUP}}"
  export G6_RESULT_BARRIER_DIR="${G6_RESULT_BARRIER_DIR:-${G6RD_RESULT_BARRIER}}"
  export G6_DB_HOST="${G6_DB_HOST:-postgres}"
  export G6_DB_PORT="${G6_DB_PORT:-5432}"
  export G6_SIGNING_DIR="${G6_SIGNING_DIR:-$(g6rd_materialize_signing_dir)}"
  export G6_RELAY_DIR="${G6_RELAY_DIR:-$(g6rd_materialize_relay_dir)}"
  export G6_RELAY_URL_A="${G6_RELAY_URL_A:-$(g6rd_relay_url_a)}"
  export G6_RELAY_URL_B="${G6_RELAY_URL_B:-$(g6rd_relay_url_b)}"
  export G6_API_BIND_PORT="${G6_API_BIND_PORT:-18080}"
  export G6_RELAY_BIND_PORT="${G6_RELAY_BIND_PORT:-3443}"
  local probe_key_dir="${G6RD_WORK}/probe-controller-key"
  if [[ -s "${G6RD_SECRETS}/controller.key" ]]; then
    probe_key_dir="$(g6rd_materialize_probe_controller_key_dir)"
  else
    mkdir -p "${probe_key_dir}"
  fi
  export G6_PROBE_CONTROLLER_KEY_DIR="${probe_key_dir}"
  if [[ -s "${G6RD_STATE}/controller-endpoint-id" ]]; then
    OCSERV_CONTROLLER_ENDPOINT_ID="$(<"${G6RD_STATE}/controller-endpoint-id")"
    export OCSERV_CONTROLLER_ENDPOINT_ID
  fi
}

g6rd_placeholder_env() {
  export G6_FD_ID="${FD_ID}"
  export G6_OWNER_PASSWORD="${G6_OWNER_PASSWORD:-harness-placeholder}"
  export G6_APP_PASSWORD="${G6_APP_PASSWORD:-harness-placeholder}"
  export G6_REPLICATION_PASSWORD="${G6_REPLICATION_PASSWORD:-harness-placeholder}"
  export G6_DEV_AUTH_TOKEN="${G6_DEV_AUTH_TOKEN:-harness-placeholder}"
  export G6_ARCHIVE_DIR="${G6_ARCHIVE_DIR:-${G6RD_ARCHIVE}}"
  export G6_BASEBACKUP_DIR="${G6_BASEBACKUP_DIR:-${G6RD_BASEBACKUP}}"
  export G6_RESULT_BARRIER_DIR="${G6_RESULT_BARRIER_DIR:-${G6RD_RESULT_BARRIER}}"
  export G6_DB_HOST="${G6_DB_HOST:-postgres}"
  export G6_DB_PORT="${G6_DB_PORT:-5432}"
  export G6_SIGNING_DIR="${G6_SIGNING_DIR:-${G6RD_WORK}/signing}"
  export G6_RELAY_DIR="${G6_RELAY_DIR:-${G6RD_WORK}/relay-secrets}"
  export G6_RELAY_URL_A="${G6_RELAY_URL_A:-https://relay-a:3443}"
  export G6_RELAY_URL_B="${G6_RELAY_URL_B:-https://relay-b:3443}"
  export G6_API_BIND_PORT="${G6_API_BIND_PORT:-18080}"
  export G6_RELAY_BIND_PORT="${G6_RELAY_BIND_PORT:-3443}"
  export G6_PROBE_CONTROLLER_KEY_DIR="${G6_PROBE_CONTROLLER_KEY_DIR:-${G6RD_WORK}/probe-controller-key}"
}

g6rd_compose() {
  if [[ -z "${G6_OWNER_PASSWORD:-}" || -z "${G6_DEV_AUTH_TOKEN:-}" || -z "${G6_FD_ID:-}" ]]; then
    if ! g6rd_export_common_env; then
      g6rd_placeholder_env
    fi
  fi
  docker compose --project-name "${COMPOSE_PROJECT}" --file "${COMPOSE_FILE}" "$@"
}

g6rd_agent_compose() {
  [[ -s "${G6RD_AGENT_COMPOSE}" ]] || {
    echo "agent overlay ${G6RD_AGENT_COMPOSE} has not been generated" >&2
    return 1
  }
  if [[ -z "${G6_OWNER_PASSWORD:-}" || -z "${G6_DEV_AUTH_TOKEN:-}" || -z "${G6_FD_ID:-}" ]]; then
    if ! g6rd_export_common_env; then
      g6rd_placeholder_env
    fi
  fi
  docker compose --project-name "${COMPOSE_PROJECT}" \
    --file "${COMPOSE_FILE}" --file "${G6RD_AGENT_COMPOSE}" "$@"
}

# ---------------------------------------------------------------------------
# Pinned g6-tunnel: one keypair and one serve/forward process per forwarded
# service, because a relay would drop two endpoints presenting the same
# NodeId (the same constraint that orders the transportd handover).
# ---------------------------------------------------------------------------

g6rd_build_tunnel() {
  [[ -x "${G6RD_TUNNEL_BIN}" ]] && return 0
  "${G6RD_ROOT}/.tools/cargo/bin/cargo" build --release \
    --manifest-path "${G6RD_ROOT}/rust/Cargo.toml" -p ocservia-g6-tunnel
}

g6rd_tunnel_key() {
  local name="${1:?tunnel key name is required}"
  local key="${G6RD_SECRETS}/tunnel-${name}.key"
  [[ -s "${key}" ]] || {
    dd if=/dev/urandom of="${key}" bs=32 count=1 status=none
    chmod 0600 "${key}"
  }
  "${G6RD_TUNNEL_BIN}" node-id --key-file "${key}"
}

g6rd_tunnel_serve() {
  local name="${1:?tunnel name is required}"
  local peer="${2:?peer node id is required}"
  local target_port="${3:?target port is required}"
  local pid_file="${G6RD_STATE}/tunnel-serve-${name}.pid"
  [[ -s "${pid_file}" ]] && kill -0 "$(<"${pid_file}")" 2>/dev/null && return 0
  nohup "${G6RD_TUNNEL_BIN}" serve \
    --key-file "${G6RD_SECRETS}/tunnel-${name}.key" \
    --peer-node "${peer}" \
    --forward "127.0.0.1:${target_port}" \
    >"${G6RD_LOGS}/tunnel-serve-${name}.log" 2>&1 &
  echo $! >"${pid_file}"
  sleep 2
  kill -0 "$(<"${pid_file}")"
}

g6rd_tunnel_forward() {
  local name="${1:?tunnel name is required}"
  local peer="${2:?peer node id is required}"
  local listen_port="${3:?listen port is required}"
  local pid_file="${G6RD_STATE}/tunnel-forward-${name}.pid"
  [[ -s "${pid_file}" ]] && kill -0 "$(<"${pid_file}")" 2>/dev/null && return 0
  # 0.0.0.0 so containers reach the forward through the bridge gateway
  # (extra_hosts host-gateway) while the host driver keeps loopback access.
  nohup "${G6RD_TUNNEL_BIN}" forward \
    --key-file "${G6RD_SECRETS}/tunnel-${name}.key" \
    --peer-node "${peer}" \
    --listen "0.0.0.0:${listen_port}" \
    >"${G6RD_LOGS}/tunnel-forward-${name}.log" 2>&1 &
  echo $! >"${pid_file}"
  sleep 2
  kill -0 "$(<"${pid_file}")"
}

g6rd_tunnel_stop() {
  local status=0 pid_file pid
  for pid_file in "${G6RD_STATE}"/tunnel-*.pid; do
    [[ -f "${pid_file}" ]] || continue
    pid="$(<"${pid_file}")"
    if kill -0 "${pid}" 2>/dev/null; then
      kill "${pid}" 2>/dev/null || status=1
      local _
      for _ in 1 2 3 4 5; do
        kill -0 "${pid}" 2>/dev/null || break
        sleep 1
      done
      kill -9 "${pid}" 2>/dev/null || true
    fi
    rm -f "${pid_file}"
  done
  return "${status}"
}

# The forwarded listener must bind the docker bridge gateway so containers
# reach the peer through extra_hosts host-gateway entries.
g6rd_host_gateway_address() {
  docker network inspect bridge --format '{{(index .IPAM.Config 0).Gateway}}'
}

# ---------------------------------------------------------------------------
# PostgreSQL helpers (published loopback ports, pinned client image).
# ---------------------------------------------------------------------------

g6rd_psql() {
  PGPASSWORD="$(g6rd_secret owner-password)" docker run --rm --network host \
    --log-driver none -e PGPASSWORD \
    postgres:17.10-bookworm psql \
    "host=127.0.0.1 port=${G6_DB_PORT:-5432} user=ocservia_owner dbname=ocservia sslmode=disable" \
    -v ON_ERROR_STOP=1 "$@"
}

g6rd_psql_json() {
  g6rd_psql -Atc "$1"
}

g6rd_archive_has_segment() {
  local segment="${1:?WAL segment is required}"
  [[ "${segment}" =~ ^[0-9A-F]{24}$ ]] || return 1
  docker run --rm --entrypoint /bin/sh \
    -v "${G6RD_ARCHIVE}:/archive:ro" postgres:17.10-bookworm \
    -c "test -f /archive/${segment}"
}

# ---------------------------------------------------------------------------
# Timeline. fd-b owns the merged timeline and stamps every event when it
# observes it, so sequence and timestamps stay monotonic across the two
# runner clocks (same discipline as the stage-6 harness).
# ---------------------------------------------------------------------------

g6rd_timeline_init() {
  : >"${G6RD_OUTBOX}/timeline.jsonl"
  echo 0 >"${G6RD_STATE}/timeline-sequence"
  g6rd_now >"${G6RD_STATE}/timeline-last"
}

g6rd_timeline_event() {
  local event_id="${1:?event id is required}" stamp_file="${2:-}" sequence last stamp
  sequence="$(( $(<"${G6RD_STATE}/timeline-sequence") + 1 ))"
  last="$(<"${G6RD_STATE}/timeline-last")"
  if [[ -n "${stamp_file}" && -s "${stamp_file}" ]]; then
    stamp="$(<"${stamp_file}")"
  else
    stamp="$(g6rd_now)"
  fi
  stamp="$(sed -E 's/\.[0-9]+Z$/Z/' <<<"${stamp}")"
  stamp="$(jq -nr --arg l "${last}" --arg s "${stamp}" \
    'if ($s | fromdateiso8601) <= ($l | fromdateiso8601)
     then (($l | fromdateiso8601) + 1 | todateiso8601)
     else $s end')"
  jq -cn --argjson sequence "${sequence}" --arg timestamp "${stamp}" \
    --arg environment_id "${G6RD_ENVIRONMENT_ID}" \
    --arg candidate_sha "${G6RD_CANDIDATE_SHA}" \
    --arg event_id "${event_id}" \
    '{sequence:$sequence,timestamp:$timestamp,environment_id:$environment_id,candidate_sha:$candidate_sha,event_id:$event_id}' \
    >>"${G6RD_OUTBOX}/timeline.jsonl"
  echo "${sequence}" >"${G6RD_STATE}/timeline-sequence"
  echo "${stamp}" >"${G6RD_STATE}/timeline-last"
}

g6rd_wait_until() {
  local attempts="${1:?attempts is required}"
  local interval="${2:?interval seconds is required}"
  local description="${3:?description is required}"
  shift 3
  local _
  for _ in $(seq 1 "${attempts}"); do
    if "$@" >/dev/null 2>&1; then
      return 0
    fi
    sleep "${interval}"
  done
  echo "timed out waiting for ${description}" >&2
  return 1
}

# ---------------------------------------------------------------------------
# API driver. All commands in the evidence window are issued through these
# helpers so the accepted-write identity chain (http request id = command id
# = outbox row = audit write) is complete by construction.
# ---------------------------------------------------------------------------

g6rd_api_port() {
  if [[ "${FD_ID}" == "fd-a" ]]; then
    printf '%s\n' "${G6_API_BIND_PORT:-18080}"
  else
    # era 1 targets fd-a's api through the tunnel forward; once the local
    # standby is promoted the driver switches to the local published port
    # (the gateway transfer of the api-instance failover observation).
    if [[ "${G6RD_API_LOCAL:-}" == 1 || -e "${G6RD_STATE}/promoted" ]]; then
      printf '%s\n' "${G6_API_BIND_PORT:-18080}"
    else
      printf '%s\n' "${G6_API_FORWARD_PORT:-18081}"
    fi
  fi
}

g6rd_api_curl() {
  local path="${1:?api path is required}"
  shift
  local port
  port="$(g6rd_api_port)"
  curl --silent --show-error --max-time 10 \
    --header "Authorization: Bearer $(g6rd_secret dev-auth-token)" "$@" \
    "http://127.0.0.1:${port}${path}"
}

g6rd_api_session_curl() {
  local role="${1:?session role is required}"
  local path="${2:?api path is required}"
  shift 2
  local port
  case "${role}" in
    requester | approver) ;;
    *)
      echo "unknown G6 session role ${role}" >&2
      return 2
      ;;
  esac
  port="$(g6rd_api_port)"
  curl --silent --show-error --max-time 10 \
    --header "Cookie: __Host-ocservia_session=$(g6rd_secret "${role}-session-cookie")" \
    "$@" "http://127.0.0.1:${port}${path}"
}

g6rd_api_ready() {
  g6rd_api_curl /readyz -o /dev/null -w '%{http_code}' | grep -q '^200$'
}

g6rd_node_revision() {
  local node_id="${1:?node id is required}"
  g6rd_api_curl "/api/v1/nodes/${node_id}" | jq -er '.version'
}

g6rd_mint_enrollment_token() {
  local node_name="${1:?node name is required}" endpoint="${2:?endpoint id is required}"
  local response="${G6RD_STATE}/enrollment-token-response-${BASHPID}.json" status token detail
  if ! status="$(g6rd_api_session_curl requester /api/v1/enrollment-tokens -X POST \
    --header 'Content-Type: application/json' \
    --header "X-Workspace-ID: ${G6RD_WORKSPACE_ID}" \
    --data "$(jq -cn --arg workspace "${G6RD_WORKSPACE_ID:?workspace id is required}" \
      --arg name "${node_name}" --arg endpoint "${endpoint}" \
      '{workspace_id:$workspace,environment:"development",expected_node_name:$name,expected_endpoint_id:$endpoint,reason:"g6 readiness harness"}')" \
    -o "${response}" -w '%{http_code}')"; then
    rm -f -- "${response}"
    echo "enrollment token API request failed before an HTTP response" >&2
    return 1
  fi
  if [[ "${status}" != 201 ]]; then
    detail="$(jq -r 'if type == "object" then (.detail // .title // "unspecified API error") else "invalid JSON response" end' \
      "${response}" 2>/dev/null || printf 'invalid JSON response')"
    rm -f -- "${response}"
    echo "enrollment token API returned HTTP ${status}: ${detail}" >&2
    return 1
  fi
  if ! token="$(jq -er '.token | select(type == "string" and length == 43)' "${response}")"; then
    rm -f -- "${response}"
    echo "enrollment token API returned an invalid success document" >&2
    return 1
  fi
  rm -f -- "${response}"
  printf '%s\n' "${token}"
}

g6rd_approve_node() {
  local node_id="${1:?node id is required}"
  local capabilities request response status detail approval_id request_hash
  local node_status approval_status
  capabilities="$(G6_DB_PORT="${G6_APPROVAL_DB_PORT:-5432}" g6rd_psql -Atc \
    "SELECT COALESCE(json_agg(capability ORDER BY capability),'[]'::json)::text \
     FROM node_capabilities WHERE node_id='${node_id}'")"
  jq -e 'type == "array" and length > 0 and all(.[]; type == "string" and length > 0)' \
    <<<"${capabilities}" >/dev/null || {
    echo "node ${node_id} did not advertise approvable capabilities" >&2
    return 1
  }
  request="$(jq -cn --arg node_id "${node_id}" --argjson capabilities "${capabilities}" \
    '{action:"node.approve",resource_type:"node",resource_id:$node_id,
      reason:"approve a G6 readiness managed node",ttl_seconds:600,
      node_approval:{labels:{},policy:"standard",capabilities:$capabilities}}')"
  response="${G6RD_STATE}/node-approval-response-${BASHPID}.json"
  if ! status="$(g6rd_api_session_curl requester /api/v1/approval-requests -X POST \
    --header 'Content-Type: application/json' \
    --header "X-Workspace-ID: ${G6RD_WORKSPACE_ID}" \
    --data "${request}" --output "${response}" --write-out '%{http_code}')"; then
    rm -f -- "${response}"
    echo "node approval request failed before an HTTP response" >&2
    return 1
  fi
  if [[ "${status}" != 201 ]]; then
    detail="$(jq -r 'if type == "object" then (.detail // .title // "unspecified API error") else "invalid JSON response" end' \
      "${response}" 2>/dev/null || printf 'invalid JSON response')"
    rm -f -- "${response}"
    echo "node approval request returned HTTP ${status}: ${detail}" >&2
    return 1
  fi
  if ! approval_id="$(jq -er '.id | select(type == "string")' "${response}")" || \
    ! request_hash="$(jq -er '.request_hash | select(type == "string" and length == 64)' "${response}")"; then
    rm -f -- "${response}"
    echo "node approval request returned an invalid success document" >&2
    return 1
  fi
  rm -f -- "${response}"

  response="${G6RD_STATE}/node-approval-decision-${BASHPID}.json"
  if ! status="$(g6rd_api_session_curl approver \
    "/api/v1/approval-requests/${approval_id}:approve" -X POST \
    --header 'Content-Type: application/json' \
    --data "$(jq -cn --arg hash "${request_hash}" \
      '{reason:"independent G6 readiness review",expected_request_hash:$hash}')" \
    --output "${response}" --write-out '%{http_code}')"; then
    rm -f -- "${response}"
    echo "node approval decision failed before an HTTP response" >&2
    return 1
  fi
  if [[ "${status}" != 200 ]]; then
    detail="$(jq -r 'if type == "object" then (.detail // .title // "unspecified API error") else "invalid JSON response" end' \
      "${response}" 2>/dev/null || printf 'invalid JSON response')"
    rm -f -- "${response}"
    echo "node approval decision returned HTTP ${status}: ${detail}" >&2
    return 1
  fi
  rm -f -- "${response}"

  response="${G6RD_STATE}/node-approval-apply-${BASHPID}.json"
  if ! status="$(g6rd_api_session_curl requester "/api/v1/nodes/${node_id}/approval" -X POST \
    --header 'Content-Type: application/json' \
    --header "X-Approval-ID: ${approval_id}" \
    --data "$(jq -cn --argjson capabilities "${capabilities}" \
      '{labels:{},policy:"standard",capabilities:$capabilities,reason:"g6 readiness harness"}')" \
    --output "${response}" --write-out '%{http_code}')"; then
    rm -f -- "${response}"
    echo "node approval apply failed before an HTTP response" >&2
    return 1
  fi
  if [[ "${status}" == 503 ]] && jq -e \
    '.type == "https://ocservia.dev/problems/transport-unavailable"' \
    "${response}" >/dev/null 2>&1; then
    # Node activation commits before the immediate transport synchronization.
    # Reconcile both durable records before accepting that defined pending
    # outcome. The caller must synchronize transport trust before starting the
    # fleet; treating any other 503 or durable state as success is forbidden.
    if node_status="$(g6rd_api_session_curl requester "/api/v1/nodes/${node_id}" \
      | jq -er '.trust_status')" && \
      approval_status="$(g6rd_api_session_curl approver \
        "/api/v1/approval-requests/${approval_id}" | jq -er '.status')" && \
      [[ "${node_status}" == active && "${approval_status}" == consumed ]]; then
      rm -f -- "${response}"
      return 0
    fi
  fi
  if [[ "${status}" != 200 ]]; then
    detail="$(jq -r 'if type == "object" then (.detail // .title // "unspecified API error") else "invalid JSON response" end' \
      "${response}" 2>/dev/null || printf 'invalid JSON response')"
    rm -f -- "${response}"
    echo "node approval apply returned HTTP ${status}: ${detail}" >&2
    return 1
  fi
  rm -f -- "${response}"
}

# The agent prints its UUID before closing the Iroh endpoint. Relay shutdown
# logs may follow on stdout, so select the exact protocol value instead of
# assuming the final output line is the enrollment result.
g6rd_extract_enrollment_node_id() {
  local line node_id=""
  while IFS= read -r line; do
    if [[ "${line}" =~ ^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$ ]]; then
      node_id="${line}"
    fi
  done
  [[ -n "${node_id}" ]] || return 1
  printf '%s\n' "${node_id}"
}

# Issues one synthetic.noop enqueue and appends the raw observation to the
# harness request log: timestamp, latency, command id, node, and the http
# outcome. The evidence builder turns this log into http-samples.csv rows
# whose request_id is the accepted command id.
g6rd_enqueue_command() {
  local node_id="${1:?node id is required}" key="${2:?idempotency key is required}"
  local log="${3:-${G6RD_STATE}/enqueue-log.jsonl}" revision stamp latency body command_id
  revision="$(g6rd_node_revision "${node_id}")" || return 1
  stamp="$(g6rd_now)"
  body="$(mktemp "${RUNNER_TEMP}/enqueue.XXXXXX")"
  if ! latency="$(curl --silent --show-error --max-time 10 \
    --header "Authorization: Bearer $(g6rd_secret dev-auth-token)" \
    --header 'Content-Type: application/json' \
    --header "Idempotency-Key: ${key}" \
    --header "If-Match: revision-${revision}" \
    --data '{"kind":"synthetic_noop"}' \
    --output "${body}" \
    -w '%{http_code} %{time_total}' \
    "http://127.0.0.1:$(g6rd_api_port)/api/v1/nodes/${node_id}/synthetic-commands")"; then
    latency="000 0.000"
  fi
  command_id="$(jq -er '.command_id // empty' "${body}" 2>/dev/null || true)"
  rm -f "${body}"
  jq -cn --arg at "${stamp}" \
    --arg node "${node_id}" --arg key "${key}" \
    --arg outcome "${latency% *}" --argjson latency_seconds "${latency#* }" \
    --arg command_id "${command_id:-}" \
    '{at:$at,node:$node,idempotency_key:$key,status:($outcome|tonumber),latency_seconds:$latency_seconds,command_id:$command_id}' \
    >>"${log}"
  [[ "${latency% *}" == 20* && -n "${command_id}" ]]
}

# Reads through the same gateway path; the SLO population records one row
# per logical read request.
g6rd_read_nodes() {
  local log="${1:-${G6RD_STATE}/read-log.jsonl}" stamp latency
  stamp="$(g6rd_now)"
  latency="$(curl --silent --show-error --max-time 10 \
    --header "Authorization: Bearer $(g6rd_secret dev-auth-token)" \
    -o /dev/null -w '%{http_code} %{time_total}' \
    "http://127.0.0.1:$(g6rd_api_port)/api/v1/nodes?limit=1")" || latency="000 0.000"
  jq -cn --arg at "${stamp}" --arg status "${latency% *}" \
    --argjson latency_seconds "${latency#* }" \
    '{at:$at,status:($status|tonumber),latency_seconds:$latency_seconds}' >>"${log}"
  [[ "${latency% *}" == 200 ]]
}

# ---------------------------------------------------------------------------
# g6-probe wrappers. The probe runs as a compose service sharing the
# transport runtime volume (uid 65534, the control-plane uid transportd
# requires on its Unix socket).
# ---------------------------------------------------------------------------

g6rd_probe() {
  g6rd_compose --profile probe run --rm --no-deps g6-probe "$@"
}

g6rd_probe_node_connection() {
  local expect_path="${1:?expected path is required}"
  shift
  local node_ids=("${@:?at least one node id is required}")
  local args=()
  local node
  for node in "${node_ids[@]}"; do
    args+=(--node-id "${node}")
  done
  g6rd_probe node-connection \
    --socket /run/ocserv-platform/transportd.sock \
    --expect-path "${expect_path}" \
    "${args[@]}"
}

# ---------------------------------------------------------------------------
# Agent fleet. One supervisor container per agent; identity, journal, and
# secrets live on per-agent bind directories so agent state survives
# transportd handovers and the reconnect storm.
# ---------------------------------------------------------------------------

g6rd_agent_count() {
  if [[ "${FD_ID}" == "fd-a" ]]; then
    printf '%s\n' "${G6_AGENTS_A:-28}"
  else
    printf '%s\n' "${G6_AGENTS_B:-27}"
  fi
}

g6rd_agent_dir() {
  local index="${1:?agent index is required}"
  printf '%s/agent-%s-%02d\n' "${G6RD_AGENTS}" "${FD_ID}" "${index}"
}

g6rd_prepare_agent_material() {
  local index="${1:?agent index is required}"
  local dir
  dir="$(g6rd_agent_dir "${index}")"
  mkdir -p "${dir}/identity" "${dir}/journal" "${dir}/secrets" "${dir}/state"
  : >"${dir}/state/synthetic-barrier"
  chmod 0700 "${dir}/identity" "${dir}/secrets"
  cp -f "${G6RD_SECRETS}/command-verification.pem" \
    "${dir}/secrets/command-verification-agent.pem"
  cp -f "${G6RD_SECRETS}/command-verification.pem" \
    "${dir}/secrets/command-verification-privd.pem"
  cp -f "${G6RD_SECRETS}/seal-user-password.key" "${dir}/secrets/"
  cp -f "${G6RD_SECRETS}/seal-user-password-sha256" "${dir}/secrets/"
  cp -f "${G6RD_SECRETS}/seal-p12.key" "${dir}/secrets/"
  cp -f "${G6RD_SECRETS}/seal-p12-sha256" "${dir}/secrets/"
  cp -f "${G6RD_SECRETS}/relay-ca.pem" "${dir}/secrets/relay-ca.pem"
  cp -f "${G6RD_SECRETS}/relay-token" "${dir}/secrets/relay-token"
  chmod 0640 "${dir}/secrets/"*.pem
  chmod 0600 "${dir}/secrets/"*.key "${dir}/secrets/relay-token"
  docker run --rm --network none --log-driver none \
    -v "${dir}/secrets:/fix" postgres:17.10-bookworm \
    sh -c 'chown 0:65532 /fix/*; chmod 0755 /fix; chmod 0640 /fix/*' >/dev/null
}

g6rd_write_agent_overlay() {
  local count="${1:?agent count is required}" index dir name
  {
    echo "services:"
    for index in $(seq 1 "${count}"); do
      dir="$(g6rd_agent_dir "${index}")"
      name="g6-${FD_ID}-$(printf '%02d' "${index}")"
      cat <<EOF
  agent-${FD_ID}-$(printf '%02d' "${index}"):
    restart: "no"
    read_only: true
    cap_drop: [ALL]
    cap_add: [SETUID, SETGID]
    security_opt:
      - no-new-privileges:true
    init: true
    build:
      context: ../..
      dockerfile: rust/g6-agent.Dockerfile
    environment:
      G6_MODE: run
      G6_AGENT_NAME: "${name}"
      G6_CONTROLLER_ENDPOINT_ID: "${OCSERV_CONTROLLER_ENDPOINT_ID:-}"
      G6_RELAY_URL_A: "${G6_RELAY_URL_A:?}"
      G6_RELAY_URL_B: "${G6_RELAY_URL_B:?}"
    extra_hosts:
      - "host.docker.internal:host-gateway"
      - "relay-a:host-gateway"
      - "relay-b:host-gateway"
    volumes:
      - type: bind
        source: ${dir}/identity
        target: /run/ocservia-agent/identity
      - type: bind
        source: ${dir}/journal
        target: /run/ocservia-agent/journal
      - type: bind
        source: ${dir}/secrets
        target: /run/ocservia-agent/secrets
        read_only: true
      - type: bind
        source: ${dir}/state
        target: /run/ocservia-agent/state
    tmpfs:
      - /tmp:size=16m,mode=0700
      - /run/ocserv-platform:size=8m,uid=0,gid=65532,mode=0750
EOF
    done
  } >"${G6RD_AGENT_COMPOSE}"
}

# Compose validates overlay bind sources even when `down` is cleaning a
# partially prepared run. Create only the empty directory skeleton here; live
# phases still require g6rd_prepare_agent_material to install every key.
g6rd_prepare_agent_cleanup_dirs() {
  local index dir
  for index in $(seq 1 "$(g6rd_agent_count)"); do
    dir="$(g6rd_agent_dir "${index}")"
    mkdir -p "${dir}/identity" "${dir}/journal" "${dir}/secrets" "${dir}/state"
  done
}

# The agent process (uid 65532) owns its identity, journal, and state binds;
# hand the directories over through a short-lived root container the way the
# stage-6 harness reclaims PostgreSQL directories.
g6rd_chown_agent_dirs() {
  local index dir
  for index in $(seq 1 "$(g6rd_agent_count)"); do
    dir="$(g6rd_agent_dir "${index}")"
    [[ -d "${dir}" ]] || continue
    docker run --rm -v "${dir}:/chown" postgres:17.10-bookworm \
      chown -R 65532:65532 /chown/identity /chown/journal /chown/state >/dev/null 2>&1
  done
}

g6rd_release_synthetic_barriers() {
  local index
  for index in $(seq 1 "$(g6rd_agent_count)"); do
    rm -f "$(g6rd_agent_dir "${index}")/state/synthetic-barrier"
  done
}

# ---------------------------------------------------------------------------
# Resource sampler. One process on fd-b samples the era-2 control plane
# (api, worker, scheduler), transportd #2, one local agent, and the promoted
# PostgreSQL every three seconds into the resource-samples CSV. Sampling one
# failure domain keeps the gap metric on a single runner clock.
# ---------------------------------------------------------------------------

g6rd_sampler_row() {
  local component="$1" instance="$2" container="$3" pid_expr="$4" tasks_expr="$5" queue="$6" db="$7" stamp="$8"
  local rss fd tasks
  rss="$(g6rd_compose exec -T "${container}" sh -c \
    "pid=\$(${pid_expr}); awk '/VmRSS/{print \$2}' /proc/\$pid/status" 2>/dev/null | tr -d '[:space:]')"
  fd="$(g6rd_compose exec -T "${container}" sh -c \
    "pid=\$(${pid_expr}); ls /proc/\$pid/fd 2>/dev/null | wc -l" 2>/dev/null | tr -d '[:space:]')"
  tasks="$(g6rd_compose exec -T "${container}" sh -c "${tasks_expr}" 2>/dev/null | tr -d '[:space:]')"
  [[ "${rss}" =~ ^[0-9]+$ ]] || return 1
  [[ "${fd}" =~ ^[0-9]+$ ]] || return 1
  [[ "${tasks}" =~ ^[0-9]+$ ]] || return 1
  [[ "${queue}" =~ ^[0-9]*$ ]] || return 1
  [[ "${db}" =~ ^[0-9]*$ ]] || return 1
  rss="$((rss * 1024))"
  printf '%s,%s,%s,%s,%s,%s,%s,%s,%s,%s\n' \
    "${stamp}" "${component}" "${instance}" "${rss}" "${fd}" "${tasks}" "${queue}" "${db}" \
    "${G6RD_ENVIRONMENT_ID}" "${G6RD_CANDIDATE_SHA}"
}

g6rd_sampler_tick() {
  local out_file="${1:?output csv is required}"
  local queue db stamp
  # one clock reading per tick: per-row stamps would let sequential docker
  # execs stretch a tick past the five-second sample-gap bound
  stamp="$(g6rd_now)"
  db="$(g6rd_psql -Atc 'SELECT count(*) FROM pg_stat_activity' 2>/dev/null)" || return 1
  queue="$(g6rd_psql -Atc \
    'SELECT count(*) FROM outbox_events WHERE published_at IS NULL' 2>/dev/null)" || return 1
  [[ "${db}" =~ ^[0-9]+$ && "${queue}" =~ ^[0-9]+$ ]] || return 1
  {
    g6rd_sampler_row controller "api-${FD_ID}" api 'echo 1' \
      'curl -s 127.0.0.1:6060/debug/pprof/goroutine?debug=1 | sed -n "1s/.*total \([0-9]*\).*/\1/p"' \
      0 "" "${stamp}"
    g6rd_sampler_row controller "worker-${FD_ID}" worker 'echo 1' \
      'curl -s 127.0.0.1:6060/debug/pprof/goroutine?debug=1 | sed -n "1s/.*total \([0-9]*\).*/\1/p"' \
      0 "" "${stamp}"
    g6rd_sampler_row controller "scheduler-${FD_ID}" scheduler 'echo 1' \
      'curl -s 127.0.0.1:6060/debug/pprof/goroutine?debug=1 | sed -n "1s/.*total \([0-9]*\).*/\1/p"' \
      0 "" "${stamp}"
    # shellcheck disable=SC2016  # the sed program must reach the container verbatim
    g6rd_sampler_row transportd "transportd-${FD_ID}" transportd 'echo 1' \
      'sed -n "\$s/.*\"tasks_alive\":\([0-9]*\).*/\1/p" /run/transport-stats/tasks.json' \
      0 "" "${stamp}"
    # shellcheck disable=SC2016  # the sed program must reach the container verbatim
    g6rd_sampler_row agent "agent-${FD_ID}-01" "agent-${FD_ID}-01" 'cat /run/ocservia-agent/state/agent.pid' \
      'sed -n "\$s/.*\"tasks_alive\":\([0-9]*\).*/\1/p" /run/ocservia-agent/journal/tasks.json' \
      0 "" "${stamp}"
    g6rd_sampler_row postgres "postgres-${FD_ID}" postgres 'echo 1' \
      'ls /proc | grep -c "^[0-9]"' \
      "${queue}" "${db}" "${stamp}"
  } >>"${out_file}"
}

# The sampler and the fencing/leadership watchers run as detached loops in
# their own processes: the phase script launches each through
# g6rd_spawn_harness_loop with the identity variables re-exported, so the
# loop body itself is ordinary sourced shell with ordinary quoting.
g6rd_sampler_loop() {
  while [[ ! -e "${G6RD_STATE}/sampler-stop" ]]; do
    if ! g6rd_sampler_tick "${G6RD_SAMPLER_OUT}"; then
      g6rd_now >"${G6RD_STATE}/sampler-failed-at"
      return 1
    fi
    sleep 3
  done
}

g6rd_spawn_harness_loop() {
  local log_file="${1:?loop log file is required}"
  shift
  # shellcheck disable=SC2016  # the loop body is a separately quoted script
  nohup env \
    G6RD_ROOT="${G6RD_ROOT}" \
    RUNNER_TEMP="${RUNNER_TEMP}" \
    RUN_ID="${RUN_ID}" \
    FD_ID="${FD_ID}" \
    FD_ALIAS="${FD_ALIAS}" \
    G6_AUTHORITY="${G6_AUTHORITY}" \
    G6RD_CANDIDATE_SHA="${G6RD_CANDIDATE_SHA}" \
    G6RD_ENVIRONMENT_ID="${G6RD_ENVIRONMENT_ID}" \
    G6RD_SAMPLER_OUT="${G6RD_SAMPLER_OUT:-}" \
    bash -c '
      # shellcheck source=scripts/g6-readiness-lib.sh disable=SC1091
      source "${G6RD_ROOT}/scripts/g6-readiness-lib.sh"
      g6rd_init_environment >/dev/null 2>&1 || exit 1
      g6rd_export_common_env >/dev/null 2>&1 || true
      "$@"
    ' _ "$@" >"${log_file}" 2>&1 &
  echo $!
}

# Append-only history of the authoritative fencing and leadership tables:
# each loop records only changed result sets, so the epoch-events artifact
# can derive every owner and scheduler transition with its observation time
# on one clock. The fd-b watchers dial the forwarded primary before the
# promotion and the local promoted primary after it.
g6rd_watch_fencing_history() {
  local port rows last=""
  while [[ ! -e "${G6RD_STATE}/watchers-stop" ]]; do
    port=15432
    [[ -e "${G6RD_STATE}/promoted" ]] && port=5432
    rows="$(G6_DB_PORT="${port}" g6rd_psql -Atc \
      "SELECT encode(node_id,'hex')||':'||owner_instance_id||':'||owner_incarnation||':'||encode(connection_id,'hex')||':'||owner_epoch||':'||to_char(lease_until AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"')||':'||to_char(updated_at AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"') FROM connection_owner_fencing ORDER BY node_id" \
      2>/dev/null || true)"
    if [[ -n "${rows}" && "${rows}" != "${last}" ]]; then
      printf '%s\n' "${rows}" >>"${G6RD_STATE}/fencing-history.jsonl"
      last="${rows}"
    fi
    sleep 1
  done
}

g6rd_watch_leadership_history() {
  local port rows last=""
  while [[ ! -e "${G6RD_STATE}/watchers-stop" ]]; do
    port=15432
    [[ -e "${G6RD_STATE}/promoted" ]] && port=5432
    rows="$(G6_DB_PORT="${port}" g6rd_psql -Atc \
      "SELECT instance_id||':'||incarnation||':'||epoch||':'||to_char(lease_until AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"')||':'||to_char(updated_at AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"') FROM scheduler_leadership WHERE id=1" \
      2>/dev/null || true)"
    if [[ -n "${rows}" && "${rows}" != "${last}" ]]; then
      printf '%s\n' "${rows}" >>"${G6RD_STATE}/leadership-history.jsonl"
      last="${rows}"
    fi
    sleep 1
  done
}

g6rd_start_sampler() {
  local out_file="${1:?output csv is required}"
  local header='timestamp,component,instance,rss_bytes,fd_count,tasks,queue_depth,db_connections,environment_id,candidate_sha'
  [[ -s "${out_file}" ]] || printf '%s\n' "${header}" >"${out_file}"
  rm -f "${G6RD_STATE}/sampler-stop" "${G6RD_STATE}/sampler-failed-at"
  G6RD_SAMPLER_OUT="${out_file}" g6rd_spawn_harness_loop \
    "${G6RD_LOGS}/sampler.log" g6rd_sampler_loop \
    >"${G6RD_STATE}/sampler.pid"
}

g6rd_stop_sampler() {
  local pid
  touch "${G6RD_STATE}/sampler-stop"
  [[ -s "${G6RD_STATE}/sampler.pid" ]] || return 0
  pid="$(<"${G6RD_STATE}/sampler.pid")"
  kill "${pid}" 2>/dev/null || true
  local _
  for _ in 1 2 3 4 5; do
    kill -0 "${pid}" 2>/dev/null || break
    sleep 1
  done
  kill -9 "${pid}" 2>/dev/null || true
  rm -f "${G6RD_STATE}/sampler.pid"
  [[ ! -e "${G6RD_STATE}/sampler-failed-at" ]] || {
    echo "resource sampler failed closed at $(<"${G6RD_STATE}/sampler-failed-at")" >&2
    return 1
  }
}

# ---------------------------------------------------------------------------
# Diagnostics and scoped cleanup.
# ---------------------------------------------------------------------------

g6rd_strip_secrets_from_artifacts() {
  local leaked=0 secret hit
  for secret in owner-password app-password replication-password dev-auth-token relay-token \
    oidc-client-secret session-key requester-session-cookie approver-session-cookie; do
    [[ -s "${G6RD_SECRETS}/${secret}" ]] || continue
    while IFS= read -r hit; do
      [[ -z "${hit}" ]] && continue
      rm -f -- "${hit}"
      leaked=1
    done < <(grep -RIlF -f "${G6RD_SECRETS}/${secret}" "${ARTIFACT_DIR}" 2>/dev/null || true)
  done
  if grep -RIlE -- 'BEGIN ([A-Z ]+ )?PRIVATE KEY|ocpasswd|session[_ -]?cookie' \
    "${ARTIFACT_DIR}" >/dev/null 2>&1; then
    while IFS= read -r hit; do rm -f -- "${hit}"; done \
      < <(grep -RIlE -- 'BEGIN ([A-Z ]+ )?PRIVATE KEY|ocpasswd|session[_ -]?cookie' "${ARTIFACT_DIR}" || true)
    leaked=1
  fi
  ((leaked == 0)) || {
    echo "an artifact file contained secret material and was removed" >&2
    return 1
  }
}

g6rd_reclaim_directory() {
  local dir="${1:?directory is required}"
  [[ -d "${dir}" ]] || return 0
  docker run --rm -v "${dir}:/reclaim" postgres:17.10-bookworm \
    chown -R "$(id -u):$(id -g)" /reclaim >/dev/null 2>&1 || {
      echo "cleanup: ownership reclaim failed for ${dir}" >&2
      return 1
    }
}

g6rd_diagnostics() {
  mkdir -p "${ARTIFACT_DIR}"
  g6rd_compose ps --all >"${ARTIFACT_DIR}/compose-ps-${FD_ID}.txt" 2>&1 || true
  if [[ -s "${G6RD_AGENT_COMPOSE}" ]]; then
    g6rd_agent_compose ps --all >"${ARTIFACT_DIR}/compose-ps-agents-${FD_ID}.txt" 2>&1 || true
    g6rd_agent_compose logs --no-color --tail 200 >"${ARTIFACT_DIR}/agents-${FD_ID}.log" 2>&1 || true
  fi
  g6rd_compose logs --no-color postgres api worker scheduler transportd relay \
    >"${ARTIFACT_DIR}/services-${FD_ID}.log" 2>&1 || true
  docker system df >"${ARTIFACT_DIR}/docker-storage-${FD_ID}.txt" 2>&1 || true
  cp -f "${G6RD_LOGS}"/tunnel-*.log "${ARTIFACT_DIR}/" 2>/dev/null || true
  for log in "${G6RD_LOGS}"/*.log; do
    [[ -f "${log}" ]] || continue
    sed -E 's#(postgres(ql)?://[^:/]+:)[^@]+@#\1[redacted]@#g' \
      "${log}" >"${ARTIFACT_DIR}/$(basename "${log}")" || true
  done
  printf 'fd=%s alias=%s environment_id=%s authority=%s\n' \
    "${FD_ID}" "${FD_ALIAS}" "${G6RD_ENVIRONMENT_ID}" "${G6_AUTHORITY}" \
    >"${ARTIFACT_DIR}/failure-domain-${FD_ID}.txt"
  g6rd_strip_secrets_from_artifacts
}

g6rd_cleanup() {
  local status=0 volume image pid
  g6rd_stop_sampler || status=1
  if [[ -s "${G6RD_STATE}/load-dispatch-barrier.pid" ]]; then
    pid="$(<"${G6RD_STATE}/load-dispatch-barrier.pid")"
    if kill -0 "${pid}" 2>/dev/null; then
      kill "${pid}" 2>/dev/null || status=1
      wait "${pid}" 2>/dev/null || true
    fi
    rm -f -- "${G6RD_STATE}/load-dispatch-barrier.pid"
  fi
  g6rd_tunnel_stop || {
    echo "cleanup: tunnel stop failed" >&2
    status=1
  }
  if [[ -s "${G6RD_AGENT_COMPOSE}" ]]; then
    g6rd_prepare_agent_cleanup_dirs
    if ! g6rd_agent_compose down --volumes --remove-orphans --rmi local \
      >"${G6RD_LOGS}/compose-down-agents.log" 2>&1; then
      echo "cleanup: agent compose down failed" >&2
      status=1
    fi
  fi
  if ! g6rd_compose --profile bootstrap --profile probe down --volumes --remove-orphans --rmi local \
    >"${G6RD_LOGS}/compose-down.log" 2>&1; then
    echo "cleanup: compose down failed; output follows:" >&2
    sed -n '1,40p' "${G6RD_LOGS}/compose-down.log" >&2 || true
    status=1
  fi
  for volume in postgres-data controller-secrets transport-runtime trust-runtime transport-stats; do
    if docker volume inspect "${COMPOSE_PROJECT}_${volume}" >/dev/null 2>&1; then
      echo "scoped volume cleanup failed: ${COMPOSE_PROJECT}_${volume}" >&2
      status=1
    fi
  done
  local network
  for network in agent-shared agent-isolated; do
    if docker network inspect "${COMPOSE_PROJECT}_${network}" >/dev/null 2>&1; then
      echo "scoped network cleanup failed: ${COMPOSE_PROJECT}_${network}" >&2
      status=1
    fi
  done
  g6rd_reclaim_directory "${G6RD_ARCHIVE}" || status=1
  g6rd_reclaim_directory "${G6RD_BASEBACKUP}" || status=1
  g6rd_reclaim_directory "${G6RD_RESTORE}" || status=1
  g6rd_reclaim_directory "${G6RD_WORK}" || status=1
  rm -rf -- "${G6RD_WORK}" "${RUNNER_TEMP}"/g6-readiness-* || {
    echo "cleanup: work directory removal failed" >&2
    status=1
  }
  for image in $(docker images --format '{{.Repository}}:{{.Tag}}' \
    | grep -F "ocservia-g6-rd-${RUN_ID}" || true); do
    echo "scoped image cleanup failed: ${image}" >&2
    status=1
  done
  return "${status}"
}
