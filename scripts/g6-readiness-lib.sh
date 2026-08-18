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
  G6RD_PRE_SEND_BARRIER="${G6RD_RESULT_BARRIER}/pre-send"
  G6RD_TUNNEL_BIN="${G6RD_ROOT}/rust/target/release/ocservia-g6-tunnel"
  COMPOSE_PROJECT="ocservia-g6-rd-${RUN_ID}"
  COMPOSE_FILE="${G6RD_ROOT}/deploy/g6-readiness/compose.yaml"
  G6RD_AGENT_COMPOSE="${G6RD_WORK}/agents-${FD_ID}.yaml"
  export COMPOSE_PROJECT COMPOSE_FILE RUN_ID FD_ID FD_ALIAS G6_AUTHORITY
  umask 077
  mkdir -p "${G6RD_STATE}" "${G6RD_SECRETS}" "${G6RD_OUTBOX}" \
    "${G6RD_ARCHIVE}" "${G6RD_BASEBACKUP}" "${G6RD_RESTORE}" \
    "${G6RD_LOGS}" "${G6RD_AGENTS}" "${G6RD_RESULT_BARRIER}" \
    "${G6RD_PRE_SEND_BARRIER}" "${G6RD_WORK}/relay-secrets/relay" \
    "${G6RD_WORK}/relay-secrets/transportd" \
    "${G6RD_WORK}/relay-secrets/probe" "${ARTIFACT_DIR}"
  chmod 0700 "${G6RD_WORK}" "${G6RD_SECRETS}"
  chmod 0755 "${G6RD_WORK}/relay-secrets/relay" \
    "${G6RD_WORK}/relay-secrets/transportd" \
    "${G6RD_WORK}/relay-secrets/probe"
  chmod 0777 "${G6RD_RESULT_BARRIER}" "${G6RD_PRE_SEND_BARRIER}"
}

g6rd_now() {
  date -u +%Y-%m-%dT%H:%M:%SZ
}

g6rd_prepare_support_image() {
  if ! docker image inspect postgres:17.10-bookworm >/dev/null 2>&1; then
    docker pull postgres:17.10-bookworm
  fi
  docker image inspect postgres:17.10-bookworm >/dev/null
}

g6rd_require_support_image() {
  docker image inspect postgres:17.10-bookworm >/dev/null 2>&1 || {
    echo "required local support image postgres:17.10-bookworm is missing" >&2
    return 1
  }
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
  # and a p12 sealing pair. Hash the same SPKI DER representation that privd
  # derives from each private key when it validates the pinned fingerprint.
  local pair
  for pair in user-password p12; do
    [[ -s "${G6RD_SECRETS}/seal-${pair}.key" ]] && continue
    openssl genpkey -algorithm rsa -pkeyopt rsa_keygen_bits:2048 \
      -out "${G6RD_SECRETS}/seal-${pair}.key" >/dev/null 2>&1
    openssl pkey -in "${G6RD_SECRETS}/seal-${pair}.key" -pubout \
      -out "${G6RD_SECRETS}/seal-${pair}-public.pem" >/dev/null 2>&1
    openssl rsa -in "${G6RD_SECRETS}/seal-${pair}.key" -pubout -outform DER \
      2>/dev/null | openssl dgst -sha256 -r \
      | cut -d ' ' -f1 >"${G6RD_SECRETS}/seal-${pair}-sha256"
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

# Validate the complete cached tree without reading secret contents or starting
# another container for every phase. Runner-owned directories keep retries and
# cleanup possible after files are reassigned to their container principals.
g6rd_relay_material_cache_valid() {
  local dir="${1:?relay material directory is required}"
  [[ -d "${dir}" && ! -L "${dir}" ]] || return 1
  node - "${dir}" >/dev/null 2>&1 <<'NODE'
  const fs = require("node:fs");
  const path = require("node:path");

  const root = process.argv[2];
  const runnerUid = process.getuid();
  const runnerGid = process.getgid();
  const expected = {
    relay: {
      "relay.crt": [65532, 65532, 0o644],
      "relay.key": [65532, 65532, 0o600],
      "relay-token": [65532, 65532, 0o600],
    },
    transportd: {
      "relay-ca.pem": [65532, 65532, 0o644],
      "relay-token": [65532, 65532, 0o600],
    },
    probe: {
      "relay-ca.pem": [65534, 65532, 0o644],
      "relay-token": [65534, 65532, 0o600],
    },
  };

  function metadata(target) {
    const stat = fs.lstatSync(target);
    return [stat, stat.mode & 0o7777];
  }

  function assertDirectory(target, uid, gid, mode) {
    const [stat, actualMode] = metadata(target);
    if (!stat.isDirectory() || stat.isSymbolicLink()
        || stat.uid !== uid || stat.gid !== gid || actualMode !== mode) {
      throw new Error(`invalid directory metadata: ${target}`);
    }
  }

  function assertFile(target, uid, gid, mode) {
    const [stat, actualMode] = metadata(target);
    if (!stat.isFile() || stat.isSymbolicLink()
        || stat.uid !== uid || stat.gid !== gid || actualMode !== mode) {
      throw new Error(`invalid file metadata: ${target}`);
    }
  }

  function assertExactNames(target, expectedNames) {
    const directory = fs.opendirSync(target);
    const names = [];
    try {
      while (names.length <= expectedNames.length) {
        const entry = directory.readSync();
        if (entry === null) break;
        names.push(entry.name);
      }
    } finally {
      directory.closeSync();
    }
    names.sort();
    expectedNames.sort();
    if (names.length !== expectedNames.length
        || names.some((name, index) => name !== expectedNames[index])) {
      throw new Error(`unexpected relay material tree: ${target}`);
    }
  }

  assertDirectory(root, runnerUid, runnerGid, 0o700);
  assertExactNames(root, Object.keys(expected));
  for (const [directory, files] of Object.entries(expected)) {
    const scoped = path.join(root, directory);
    assertDirectory(scoped, runnerUid, runnerGid, 0o755);
    assertExactNames(scoped, Object.keys(files));
    for (const [name, [uid, gid, mode]] of Object.entries(files)) {
      assertFile(path.join(scoped, name), uid, gid, mode);
    }
  }
NODE
}

# Verify each relay client through the numeric principal used by its image.
# The three read-only mounts intentionally expose disjoint directories even
# where two containers currently share a numeric uid.
g6rd_verify_relay_material_principals() {
  local dir="${1:?relay material directory is required}"
  g6rd_require_support_image || return 1
  docker run --rm --pull=never --network none --log-driver none \
    --read-only --cap-drop ALL --security-opt no-new-privileges:true \
    --label "ocservia.g6.run-id=${RUN_ID}" --user 65532:65532 \
    -v "${dir}/relay:/run/relay-secrets:ro" \
    --entrypoint /bin/sh postgres:17.10-bookworm -eu -c \
    'test "$(stat -c "%a" /run/relay-secrets)" = 755
     test "$(stat -c "%u:%g:%a" /run/relay-secrets/relay-token)" = 65532:65532:600
     test -r /run/relay-secrets/relay-token
     test -r /run/relay-secrets/relay.crt
     test -r /run/relay-secrets/relay.key
     test ! -e /run/relay-secrets/relay-ca.pem' >/dev/null || return 1
  docker run --rm --pull=never --network none --log-driver none \
    --read-only --cap-drop ALL --security-opt no-new-privileges:true \
    --label "ocservia.g6.run-id=${RUN_ID}" --user 65532:65532 \
    -v "${dir}/transportd:/run/relay-secrets:ro" \
    --entrypoint /bin/sh postgres:17.10-bookworm -eu -c \
    'test "$(stat -c "%a" /run/relay-secrets)" = 755
     test "$(stat -c "%u:%g:%a" /run/relay-secrets/relay-token)" = 65532:65532:600
     test -r /run/relay-secrets/relay-token
     test -r /run/relay-secrets/relay-ca.pem
     test ! -e /run/relay-secrets/relay.key' >/dev/null || return 1
  docker run --rm --pull=never --network none --log-driver none \
    --read-only --cap-drop ALL --security-opt no-new-privileges:true \
    --label "ocservia.g6.run-id=${RUN_ID}" --user 65534:65532 \
    -v "${dir}/probe:/run/relay-secrets:ro" \
    --entrypoint /bin/sh postgres:17.10-bookworm -eu -c \
    'test "$(stat -c "%a" /run/relay-secrets)" = 755
     test "$(stat -c "%u:%g:%a" /run/relay-secrets/relay-token)" = 65534:65532:600
     test -r /run/relay-secrets/relay-token
     test -r /run/relay-secrets/relay-ca.pem
     test ! -e /run/relay-secrets/relay.key' >/dev/null || return 1
}

# Materialize one private relay-token copy per consuming service principal.
# Sharing one group-readable copy would violate transportd's fail-closed token
# invariant and would expose the relay's private material to the UDS probe.
g6rd_materialize_relay_dir() {
  local dir="${G6RD_WORK}/relay-secrets"
  if [[ -d "${dir}" ]] && g6rd_relay_material_cache_valid "${dir}"; then
    printf '%s\n' "${dir}"
    return 0
  fi
  if ! rm -rf -- "${dir}" \
    || ! mkdir -p "${dir}/relay" "${dir}/transportd" "${dir}/probe" \
    || ! cp -f "${G6RD_SECRETS}/relay-chain.crt" "${dir}/relay/relay.crt" \
    || ! cp -f "${G6RD_SECRETS}/relay-leaf.key" "${dir}/relay/relay.key" \
    || ! cp -f "${G6RD_SECRETS}/relay-token" "${dir}/relay/relay-token" \
    || ! cp -f "${G6RD_SECRETS}/relay-ca.pem" "${dir}/transportd/relay-ca.pem" \
    || ! cp -f "${G6RD_SECRETS}/relay-token" "${dir}/transportd/relay-token" \
    || ! cp -f "${G6RD_SECRETS}/relay-ca.pem" "${dir}/probe/relay-ca.pem" \
    || ! cp -f "${G6RD_SECRETS}/relay-token" "${dir}/probe/relay-token" \
    || ! cmp -s "${G6RD_SECRETS}/relay-token" "${dir}/relay/relay-token" \
    || ! cmp -s "${G6RD_SECRETS}/relay-token" "${dir}/transportd/relay-token" \
    || ! cmp -s "${G6RD_SECRETS}/relay-token" "${dir}/probe/relay-token"; then
    echo "failed to build scoped relay material" >&2
    return 1
  fi
  if ! docker run --rm --pull=never --network none --log-driver none \
    --label "ocservia.g6.run-id=${RUN_ID}" \
    -v "${dir}:/fix" postgres:17.10-bookworm \
    sh -eu -c '
      chown 65532:65532 /fix/relay/* /fix/transportd/*
      chown 65534:65532 /fix/probe/*
      chmod 0755 /fix/relay /fix/transportd /fix/probe
      chmod 0644 /fix/relay/relay.crt /fix/transportd/relay-ca.pem /fix/probe/relay-ca.pem
      chmod 0600 /fix/relay/relay.key /fix/relay/relay-token \
        /fix/transportd/relay-token /fix/probe/relay-token
    ' >/dev/null; then
    echo "failed to assign scoped relay material principals" >&2
    return 1
  fi
  if ! g6rd_relay_material_cache_valid "${dir}" \
    || ! g6rd_verify_relay_material_principals "${dir}"; then
    echo "scoped relay material failed closed validation" >&2
    return 1
  fi
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
  docker run --rm --pull=never --network none --log-driver none \
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
    docker run --rm --pull=never --network none --log-driver none \
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
  printf 'https://relay-a:3443\n'
}

g6rd_relay_url_b() {
  printf 'https://relay-b:3443\n'
}

# Relay URLs are topology, not operator input. Recompute them for every phase
# so a stale shell value cannot collapse both logical relays onto the same
# local listener after the workflow crosses failure domains.
g6rd_export_relay_urls() {
  G6_RELAY_URL_A="$(g6rd_relay_url_a)"
  G6_RELAY_URL_B="$(g6rd_relay_url_b)"
  [[ "${G6_RELAY_URL_A}" != "${G6_RELAY_URL_B}" ]] || {
    echo "G6 relay URLs must remain distinct" >&2
    return 1
  }
  export G6_RELAY_URL_A G6_RELAY_URL_B
}

g6rd_relay_endpoint_ready() {
  local url="${1:?relay URL is required}" connect_port="${2:-}" host port
  if [[ "${url}" =~ ^https://(relay-[ab]):([0-9]{1,5})/?$ ]]; then
    host="${BASH_REMATCH[1]}"
    port="${BASH_REMATCH[2]}"
  else
    echo "unexpected G6 relay URL: ${url}" >&2
    return 2
  fi
  ((port >= 1 && port <= 65535)) || return 2
  connect_port="${connect_port:-${port}}"
  ((connect_port >= 1 && connect_port <= 65535)) || return 2
  curl --fail --silent --show-error --connect-timeout 2 --max-time 4 \
    --noproxy '*' \
    --cacert "${G6RD_SECRETS}/relay-ca.pem" \
    --connect-to "${host}:${port}:127.0.0.1:${connect_port}" \
    "${url%/}/ping" >/dev/null
}

g6rd_wait_for_controller_relay() {
  local connect_port=3443
  g6rd_export_relay_urls
  if [[ "${FD_ID}" == fd-a ]]; then
    connect_port="${G6_RELAY_BIND_PORT:-13443}"
  fi
  g6rd_wait_until 30 2 "controller relay path ${G6_RELAY_URL_A}" \
    g6rd_relay_endpoint_ready "${G6_RELAY_URL_A}" "${connect_port}"
}

g6rd_export_common_env() {
  local required relay_dir
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
  relay_dir="${G6_RELAY_DIR:-}"
  if [[ -z "${relay_dir}" ]]; then
    relay_dir="$(g6rd_materialize_relay_dir)" || return 1
  fi
  export G6_RELAY_DIR="${relay_dir}"
  g6rd_export_relay_urls
  export G6_LOCAL_RELAY_HOST="${G6_LOCAL_RELAY_HOST:-relay-${FD_ID#fd-}}"
  export G6_REMOTE_RELAY_HOST="${G6_REMOTE_RELAY_HOST:-$([[ "${FD_ID}" == fd-a ]] && printf relay-b || printf relay-a)}"
  export G6_API_BIND_PORT="${G6_API_BIND_PORT:-18080}"
  export G6_RELAY_BIND_PORT="${G6_RELAY_BIND_PORT:-13443}"
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
  g6rd_export_relay_urls
  export G6_LOCAL_RELAY_HOST="${G6_LOCAL_RELAY_HOST:-relay-${FD_ID#fd-}}"
  export G6_REMOTE_RELAY_HOST="${G6_REMOTE_RELAY_HOST:-$([[ "${FD_ID}" == fd-a ]] && printf relay-b || printf relay-a)}"
  export G6_API_BIND_PORT="${G6_API_BIND_PORT:-18080}"
  export G6_RELAY_BIND_PORT="${G6_RELAY_BIND_PORT:-13443}"
  export G6_PROBE_CONTROLLER_KEY_DIR="${G6_PROBE_CONTROLLER_KEY_DIR:-${G6RD_WORK}/probe-controller-key}"
}

# Image construction must not wait for or consume the per-run trust bundle.
# Force inert Compose substitutions even when a caller has live material in
# its environment, and create only the bind-directory skeleton Compose parses.
g6rd_prepare_build_environment() {
  unset G6_FD_ID G6_OWNER_PASSWORD G6_APP_PASSWORD G6_REPLICATION_PASSWORD \
    G6_DEV_AUTH_TOKEN G6_ARCHIVE_DIR G6_BASEBACKUP_DIR G6_RESULT_BARRIER_DIR \
    G6_DB_HOST G6_DB_PORT G6_SIGNING_DIR G6_RELAY_DIR G6_RELAY_URL_A \
    G6_RELAY_URL_B G6_LOCAL_RELAY_HOST G6_REMOTE_RELAY_HOST G6_API_BIND_PORT \
    G6_RELAY_BIND_PORT G6_PROBE_CONTROLLER_KEY_DIR OCSERV_CONTROLLER_ENDPOINT_ID
  g6rd_placeholder_env
  mkdir -p "${G6_SIGNING_DIR}" "${G6_RELAY_DIR}/relay" \
    "${G6_RELAY_DIR}/transportd" "${G6_RELAY_DIR}/probe" \
    "${G6_PROBE_CONTROLLER_KEY_DIR}"
  chmod 0755 "${G6_RELAY_DIR}/relay" "${G6_RELAY_DIR}/transportd" \
    "${G6_RELAY_DIR}/probe"
  g6rd_prepare_agent_cleanup_dirs
}

g6rd_compose() {
  if [[ -z "${G6_OWNER_PASSWORD:-}" || -z "${G6_DEV_AUTH_TOKEN:-}" || -z "${G6_FD_ID:-}" ]]; then
    if ! g6rd_export_common_env; then
      g6rd_placeholder_env
    fi
  fi
  local timeout_seconds="${G6RD_COMPOSE_TIMEOUT_SECONDS:-}"
  if [[ -n "${timeout_seconds}" ]]; then
    [[ "${timeout_seconds}" =~ ^[0-9]+$ && "${timeout_seconds}" -ge 1 ]] || {
      echo "G6RD_COMPOSE_TIMEOUT_SECONDS must be a positive integer" >&2
      return 2
    }
    timeout --foreground --signal=TERM --kill-after=5s "${timeout_seconds}s" \
      docker compose --project-name "${COMPOSE_PROJECT}" --file "${COMPOSE_FILE}" "$@"
    return
  fi
  docker compose --project-name "${COMPOSE_PROJECT}" --file "${COMPOSE_FILE}" "$@"
}

g6rd_install_controller_key() {
  local volume="${COMPOSE_PROJECT}_controller-secrets"
  [[ -s "${G6RD_SECRETS}/controller.key" ]] || {
    echo "controller key source is missing" >&2
    return 1
  }
  if ! docker volume inspect "${volume}" >/dev/null 2>&1; then
    # Complete initialization before replacing the generated key. A detached
    # initializer can otherwise race this copy and overwrite the authoritative
    # controller identity after it has been installed.
    g6rd_compose run --rm --no-deps controller-key-init >/dev/null
  fi
  docker run --rm --pull=never \
    -v "${volume}:/secrets" \
    -v "${G6RD_SECRETS}/controller.key:/key:ro" \
    postgres:17.10-bookworm \
    sh -c 'umask 077; cp /key /secrets/controller.key; chown 65532:65532 /secrets/controller.key; chmod 600 /secrets/controller.key; test "$(stat -c "%u:%g:%a:%s" /secrets/controller.key)" = "65532:65532:600:32"'
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
  local timeout_seconds="${G6RD_COMPOSE_TIMEOUT_SECONDS:-}"
  if [[ -n "${timeout_seconds}" ]]; then
    [[ "${timeout_seconds}" =~ ^[0-9]+$ && "${timeout_seconds}" -ge 1 ]] || {
      echo "G6RD_COMPOSE_TIMEOUT_SECONDS must be a positive integer" >&2
      return 2
    }
    timeout --foreground --signal=TERM --kill-after=5s "${timeout_seconds}s" \
      docker compose --project-name "${COMPOSE_PROJECT}" \
      --file "${COMPOSE_FILE}" --file "${G6RD_AGENT_COMPOSE}" "$@"
    return
  fi
  docker compose --project-name "${COMPOSE_PROJECT}" \
    --file "${COMPOSE_FILE}" --file "${G6RD_AGENT_COMPOSE}" "$@"
}

g6rd_agent_journal_query() {
  local service="${1:?agent service is required}" sql="${2:?sql is required}"
  local timeout_seconds="${G6RD_JOURNAL_QUERY_TIMEOUT_SECONDS:-10}"
  [[ "${timeout_seconds}" =~ ^[0-9]+$ && "${timeout_seconds}" -ge 1 ]] || {
    echo "Agent journal query timeout must be a positive integer" >&2
    return 2
  }
  G6RD_COMPOSE_TIMEOUT_SECONDS="${timeout_seconds}" \
    g6rd_agent_compose exec -T --user 65532:65532 "${service}" \
      sqlite3 -readonly /run/ocservia-agent/journal/agent.db "${sql}"
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
  local password timeout_seconds="${G6RD_PSQL_TIMEOUT_SECONDS:-}" database_options=()
  password="$(g6rd_secret owner-password)"
  if [[ -n "${timeout_seconds}" ]]; then
    [[ "${timeout_seconds}" =~ ^[0-9]+$ && "${timeout_seconds}" -ge 1 ]] || {
      echo "G6RD_PSQL_TIMEOUT_SECONDS must be a positive integer" >&2
      return 2
    }
    database_options=(-e PGCONNECT_TIMEOUT -e PGOPTIONS)
  fi
  local command=(docker run --rm --pull=never --network host
    --log-driver none --label "ocservia.g6.run-id=${RUN_ID:?}"
    -e PGPASSWORD "${database_options[@]}"
    postgres:17.10-bookworm psql
    "host=127.0.0.1 port=${G6_DB_PORT:-5432} user=ocservia_owner dbname=ocservia sslmode=disable"
    -v ON_ERROR_STOP=1 "$@")
  if [[ -n "${timeout_seconds}" ]]; then
    PGPASSWORD="${password}" PGCONNECT_TIMEOUT="${timeout_seconds}" \
      PGOPTIONS="-c statement_timeout=$((timeout_seconds * 1000))" \
      timeout --foreground --signal=TERM --kill-after=5s \
        "${timeout_seconds}s" "${command[@]}"
    return
  fi
  PGPASSWORD="${password}" "${command[@]}"
}

g6rd_psql_json() {
  g6rd_psql -Atc "$1"
}

g6rd_archive_has_segment() {
  local segment="${1:?WAL segment is required}"
  [[ "${segment}" =~ ^[0-9A-F]{24}$ ]] || return 1
  docker run --rm --pull=never --entrypoint /bin/sh \
    -v "${G6RD_ARCHIVE}:/archive:ro" postgres:17.10-bookworm \
    -c "test -f /archive/${segment}"
}

g6rd_prepare_postgres_bind_dirs() {
  g6rd_require_support_image
  docker run --rm --pull=never --network none --log-driver none \
    -v "${G6RD_ARCHIVE}:/archive" \
    -v "${G6RD_BASEBACKUP}:/basebackup" \
    postgres:17.10-bookworm \
    sh -c 'chown -R 999:999 /archive /basebackup; chmod 0700 /archive /basebackup' \
    >/dev/null
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

# Unlike the attempt-count helper, this stops starting new attempts at a wall
# clock deadline. Callers must also bound any predicate that can block.
g6rd_wait_until_deadline() {
  local timeout_seconds="${1:?timeout seconds is required}"
  local interval="${2:?interval seconds is required}"
  local description="${3:?description is required}"
  shift 3
  local deadline remaining
  [[ "${timeout_seconds}" =~ ^[0-9]+$ && "${timeout_seconds}" -ge 1 ]] || {
    echo "wait timeout must be a positive integer" >&2
    return 2
  }
  [[ "${interval}" =~ ^[0-9]+$ && "${interval}" -ge 1 ]] || {
    echo "wait interval must be a positive integer" >&2
    return 2
  }
  deadline=$((SECONDS + timeout_seconds))
  while ((SECONDS < deadline)); do
    if "$@" >/dev/null 2>&1; then
      return 0
    fi
    remaining=$((deadline - SECONDS))
    ((remaining > 0)) || break
    if ((remaining < interval)); then
      sleep "${remaining}"
    else
      sleep "${interval}"
    fi
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
  curl --silent --show-error --connect-timeout 3 --max-time 10 \
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
  curl --silent --show-error --connect-timeout 3 --max-time 10 \
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
  local status detail
  revision="$(g6rd_node_revision "${node_id}")" || return 1
  stamp="$(g6rd_now)"
  body="$(mktemp "${RUNNER_TEMP}/enqueue.XXXXXX")"
  if ! latency="$(curl --silent --show-error --connect-timeout 3 --max-time 10 \
    --header "Authorization: Bearer $(g6rd_secret dev-auth-token)" \
    --header 'Content-Type: application/json' \
    --header "Idempotency-Key: ${key}" \
    --header "If-Match: \"revision-${revision}\"" \
    --data '{"kind":"noop"}' \
    --output "${body}" \
    -w '%{http_code} %{time_total}' \
    "http://127.0.0.1:$(g6rd_api_port)/api/v1/nodes/${node_id}/synthetic-commands")"; then
    latency="000 0.000"
  fi
  status="${latency% *}"
  command_id="$(jq -er '.command_id // empty' "${body}" 2>/dev/null || true)"
  if [[ "${status}" != 20* || -z "${command_id}" ]]; then
    if [[ "${status}" == 000 ]]; then
      echo "synthetic enqueue for node ${node_id} failed before an HTTP response" >&2
    else
      detail="$(jq -r 'if type == "object" then (.detail // .title // "unspecified API error") else "invalid JSON response" end' \
        "${body}" 2>/dev/null || printf 'invalid JSON response')"
      echo "synthetic enqueue for node ${node_id} returned HTTP ${status}: ${detail}" >&2
    fi
  fi
  rm -f "${body}"
  jq -cn --arg at "${stamp}" \
    --arg node "${node_id}" --arg key "${key}" \
    --arg outcome "${status}" --argjson latency_seconds "${latency#* }" \
    --arg command_id "${command_id:-}" \
    '{at:$at,node:$node,idempotency_key:$key,status:($outcome|tonumber),latency_seconds:$latency_seconds,command_id:$command_id}' \
    >>"${log}"
  [[ "${status}" == 20* && -n "${command_id}" ]]
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
  local args=() timeout_seconds="${G6RD_NODE_CONNECTION_TIMEOUT_SECONDS:-15}"
  local node
  [[ "${timeout_seconds}" =~ ^[0-9]+$ \
    && "${timeout_seconds}" -ge 1 && "${timeout_seconds}" -le 30 ]] || {
    echo "G6RD_NODE_CONNECTION_TIMEOUT_SECONDS must be between 1 and 30" >&2
    return 1
  }
  for node in "${node_ids[@]}"; do
    args+=(--node-id "${node}")
  done
  if [[ -z "${G6_OWNER_PASSWORD:-}" || -z "${G6_DEV_AUTH_TOKEN:-}" \
    || -z "${G6_FD_ID:-}" ]]; then
    if ! g6rd_export_common_env; then
      g6rd_placeholder_env
    fi
  fi
  # A transport socket can accept the probe while an unhealthy transportd
  # never answers its RPC. Bound the whole Compose invocation so one attempt
  # cannot consume the caller's complete readiness window.
  timeout --foreground --signal=TERM --kill-after=5s "${timeout_seconds}s" \
    docker compose --project-name "${COMPOSE_PROJECT}" --file "${COMPOSE_FILE}" \
    --profile probe run --rm --no-deps g6-probe node-connection \
    --socket /run/ocserv-platform/transportd.sock \
    --signing-key-file /run/ocservia-signing/command-signing.pem \
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
  mkdir -p "${dir}/identity" "${dir}/journal" "${dir}/privd" \
    "${dir}/secrets" "${dir}/state"
  : >"${dir}/state/synthetic-barrier"
  : >"${dir}/state/synthetic-barrier.received"
  chmod 0755 "${dir}/state"
  chmod 0644 "${dir}/state/synthetic-barrier"
  # This contains only a test command UUID. Both the runner and non-root Agent
  # need write access so the harness can reset it between exact crash windows.
  chmod 0666 "${dir}/state/synthetic-barrier.received"
  chmod 0700 "${dir}/identity" "${dir}/privd" "${dir}/secrets"
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
  docker run --rm --pull=never --network none --log-driver none \
    -v "${dir}/secrets:/fix-secrets" -v "${dir}/privd:/fix-privd" \
    postgres:17.10-bookworm sh -c \
    'chown 0:65532 /fix-secrets; chmod 0750 /fix-secrets; chown 0:65532 /fix-secrets/command-verification-agent.pem /fix-secrets/relay-ca.pem /fix-secrets/relay-token /fix-secrets/seal-user-password-sha256 /fix-secrets/seal-p12-sha256; chmod 0440 /fix-secrets/command-verification-agent.pem /fix-secrets/relay-ca.pem /fix-secrets/relay-token /fix-secrets/seal-user-password-sha256 /fix-secrets/seal-p12-sha256; chown 0:0 /fix-secrets/command-verification-privd.pem /fix-secrets/seal-user-password.key /fix-secrets/seal-p12.key; chmod 0400 /fix-secrets/command-verification-privd.pem /fix-secrets/seal-user-password.key /fix-secrets/seal-p12.key; chown 0:0 /fix-privd; chmod 0700 /fix-privd; test "$(stat -c "%u:%g:%a" /fix-secrets)" = 0:65532:750; test "$(stat -c "%u:%g:%a" /fix-privd)" = 0:0:700' \
    >/dev/null
}

g6rd_install_agent_enrollment_token() {
  local index="${1:?agent index is required}"
  local token="${2:?enrollment token is required}"
  local dir
  dir="$(g6rd_agent_dir "${index}")"
  printf '%s\n' "${token}" | docker run --rm --interactive --pull=never \
    --network none --log-driver none -v "${dir}/secrets:/fix" \
    postgres:17.10-bookworm sh -c \
    'umask 077; cat > /fix/enrollment-token; chown 65532:65532 /fix/enrollment-token; chmod 0600 /fix/enrollment-token'
}

g6rd_write_agent_overlay() {
  local count="${1:?agent count is required}" index dir name node_id remote_relay
  g6rd_export_relay_urls
  remote_relay="$([[ "${FD_ID}" == fd-a ]] && printf relay-b || printf relay-a)"
  {
    echo "services:"
    for index in $(seq 1 "${count}"); do
      dir="$(g6rd_agent_dir "${index}")"
      name="g6-${FD_ID}-$(printf '%02d' "${index}")"
      node_id=""
      if [[ -s "${dir}/state/node-id" ]]; then
        node_id="$(<"${dir}/state/node-id")"
        [[ "${node_id}" =~ ^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$ ]] || {
          echo "agent ${name} has an invalid persisted node id" >&2
          return 1
        }
      fi
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
      G6_NODE_ID: "${node_id}"
      G6_CONTROLLER_ENDPOINT_ID: "${OCSERV_CONTROLLER_ENDPOINT_ID:-}"
      G6_RELAY_URL_A: "${G6_RELAY_URL_A:?}"
      G6_RELAY_URL_B: "${G6_RELAY_URL_B:?}"
    extra_hosts:
      - "host.docker.internal:host-gateway"
      - "${remote_relay}:host-gateway"
    volumes:
      - type: bind
        source: ${dir}/identity
        target: /run/ocservia-agent/identity
      - type: bind
        source: ${dir}/journal
        target: /run/ocservia-agent/journal
      - type: bind
        source: ${dir}/privd
        target: /run/ocservia-privd
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

g6rd_stage_agent_node_state() {
  local nodes_file="${1:?local nodes file is required}"
  local index=0 name node_id endpoint extra expected_name persisted state_file temporary
  [[ -s "${nodes_file}" ]] || {
    echo "agent node-state input is empty: ${nodes_file}" >&2
    return 2
  }
  while IFS=$'\t' read -r name node_id endpoint extra; do
    index=$((index + 1))
    expected_name="g6-${FD_ID}-$(printf '%02d' "${index}")"
    [[ "${name}" == "${expected_name}" && -z "${extra}" \
      && "${node_id}" =~ ^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$ \
      && "${endpoint}" =~ ^[0-9a-f]{64}$ ]] || {
      echo "agent node-state row ${index} is invalid for ${FD_ID}" >&2
      return 1
    }
    state_file="$(g6rd_agent_dir "${index}")/state/node-id"
    if [[ -e "${state_file}" && (! -f "${state_file}" || -L "${state_file}") ]]; then
      echo "agent ${name} node-state path is not a regular file" >&2
      return 1
    fi
    if [[ -s "${state_file}" ]]; then
      persisted="$(<"${state_file}")"
      [[ "${persisted}" == "${node_id}" ]] || {
        echo "agent ${name} persisted node id does not match enrollment" >&2
        return 1
      }
    else
      mkdir -p "$(dirname "${state_file}")"
      temporary="${state_file}.tmp"
      printf '%s\n' "${node_id}" >"${temporary}"
      chmod 0600 "${temporary}"
      mv -f -- "${temporary}" "${state_file}"
    fi
  done <"${nodes_file}"
  [[ "${index}" -eq "$(g6rd_agent_count)" ]] || {
    echo "agent node-state count ${index} does not match ${FD_ID} fleet size" >&2
    return 1
  }
}

# Compose validates overlay bind sources even when `down` is cleaning a
# partially prepared run. Create only the empty directory skeleton here; live
# phases still require g6rd_prepare_agent_material to install every key.
g6rd_prepare_agent_cleanup_dirs() {
  local index dir
  for index in $(seq 1 "$(g6rd_agent_count)"); do
    dir="$(g6rd_agent_dir "${index}")"
    mkdir -p "${dir}/identity" "${dir}/journal" "${dir}/privd" \
      "${dir}/secrets" "${dir}/state"
  done
}

# The Agent process (uid 65532) owns the identity and journal binds. Keep the
# state bind owned by the runner: the Agent only reads its node id and synthetic
# barrier there, while the harness must remove that barrier during failover.
g6rd_chown_agent_dirs() {
  local index dir
  for index in $(seq 1 "$(g6rd_agent_count)"); do
    dir="$(g6rd_agent_dir "${index}")"
    [[ -d "${dir}" ]] || continue
    docker run --rm --pull=never -v "${dir}:/chown" postgres:17.10-bookworm \
      chown -R 65532:65532 /chown/identity /chown/journal >/dev/null 2>&1
  done
}

g6rd_agent_service_for_name() {
  local name="${1:?agent name is required}"
  [[ "${name}" =~ ^g6-${FD_ID}-[0-9]{2}$ ]] || {
    echo "agent ${name} does not belong to ${FD_ID}" >&2
    return 2
  }
  printf 'agent-%s\n' "${name#g6-}"
}

g6rd_capture_agent_readiness() {
  local nodes_file="${1:?expected nodes file is required}"
  local response="${G6RD_STATE}/agent-readiness-last.json"
  local temporary="${response}.tmp"
  [[ -s "${nodes_file}" ]] || {
    echo "agent readiness node list is empty: ${nodes_file}" >&2
    return 2
  }
  if ! g6rd_api_session_curl requester '/api/v1/nodes?page_size=100' \
    --header "X-Workspace-ID: ${G6RD_WORKSPACE_ID:?workspace id is required}" \
    >"${temporary}" 2>"${G6RD_STATE}/agent-readiness-api-error.txt"; then
    rm -f -- "${temporary}"
    return 1
  fi
  if ! jq -e '.items | type == "array"' "${temporary}" >/dev/null 2>&1; then
    mv -f -- "${temporary}" "${response}"
    return 1
  fi
  mv -f -- "${temporary}" "${response}"
  local expected
  expected="$(cut -f2 "${nodes_file}" | jq -Rsc \
    'split("\n") | map(select(length > 0))')"
  jq -e --argjson expected "${expected}" '
    .items as $items
    | [$expected[] as $id | $items[] | select(.id == $id)] | unique_by(.id) as $nodes
    | ($expected | length) > 0
      and (($expected | unique | length) == ($expected | length))
      and (($nodes | length) == ($expected | length))
      and all($nodes[];
        .trust_status == "active"
        and .connection_state == "online"
        and .freshness == "fresh"
        and (.last_heartbeat_at | type == "string" and length > 0))
  ' "${response}" >/dev/null
}

g6rd_report_agent_readiness() {
  local response="${G6RD_STATE}/agent-readiness-last.json"
  if [[ -s "${G6RD_STATE}/agent-readiness-api-error.txt" ]]; then
    echo "last controller Agent readiness request error:" >&2
    sed -n '1,20p' "${G6RD_STATE}/agent-readiness-api-error.txt" >&2
  fi
  echo "last controller Agent readiness response:" >&2
  if [[ -s "${response}" ]] && jq -e . "${response}" >/dev/null 2>&1; then
    jq -c '{items: [.items[]? | {
      id, name, trust_status, connection_state, freshness, last_heartbeat_at
    }], page}' "${response}" >&2
  else
    echo "no valid controller response was received" >&2
  fi
}

g6rd_wait_for_agent_readiness() {
  local nodes_file="${1:?expected nodes file is required}"
  local timeout_seconds="${2:?timeout seconds is required}"
  local interval="${3:?interval seconds is required}"
  local description="${4:?description is required}"
  shift 4
  local service ready services=("$@")
  local deadline=$((SECONDS + timeout_seconds))
  while ((SECONDS < deadline)); do
    ready=0
    if g6rd_capture_agent_readiness "${nodes_file}"; then
      ready=1
    fi
    for service in "${services[@]}"; do
      if ! g6rd_agent_service_running "${service}"; then
        echo "${service} exited while waiting for ${description}" >&2
        g6rd_report_agent_readiness
        return 1
      fi
    done
    if ((ready)); then
      return 0
    fi
    sleep "${interval}"
  done
  echo "timed out waiting for ${description}" >&2
  g6rd_report_agent_readiness
  return 1
}

g6rd_agent_service_running() {
  local service="${1:?agent service is required}"
  g6rd_agent_compose ps --status running --services "${service}" 2>/dev/null \
    | grep -Fxq "${service}"
}

# Select stopped local services without treating controller propagation delay
# as a restart signal for newly started, otherwise healthy Agents.
g6rd_agent_services_not_running() {
  local nodes_file="${1:?local nodes file is required}"
  local name _ service
  while IFS=$'\t' read -r name _; do
    service="$(g6rd_agent_service_for_name "${name}")" || return 1
    if ! g6rd_agent_service_running "${service}"; then
      printf '%s\n' "${service}"
    fi
  done <"${nodes_file}"
}

# Select only running local services that the last valid controller snapshot
# did not report ready. The caller handles stopped services first, so one
# transient exit cannot restart healthy late-arriving members of the batch.
g6rd_agent_services_needing_restart() {
  local nodes_file="${1:?local nodes file is required}"
  local response="${G6RD_STATE}/agent-readiness-last.json"
  local response_valid=0 name node_id _ service
  if [[ -s "${response}" ]] && jq -e '.items | type == "array"' "${response}" >/dev/null 2>&1; then
    response_valid=1
  fi
  while IFS=$'\t' read -r name node_id _; do
    service="$(g6rd_agent_service_for_name "${name}")" || return 1
    if ! g6rd_agent_service_running "${service}"; then
      continue
    fi
    if ((response_valid)) && ! jq -e --arg id "${node_id}" '
      .items | any(.id == $id
        and .trust_status == "active"
        and .connection_state == "online"
        and .freshness == "fresh"
        and (.last_heartbeat_at | type == "string" and length > 0))
    ' "${response}" >/dev/null; then
      printf '%s\n' "${service}"
    fi
  done <"${nodes_file}"
}

g6rd_capture_agent_logs() {
  local label="${1:?diagnostic label is required}"
  local log="${G6RD_LOGS}/agents-${FD_ID}-${label}.log"
  g6rd_agent_compose ps --all \
    >"${G6RD_LOGS}/agents-${FD_ID}-${label}-ps.log" 2>&1 || true
  g6rd_agent_compose logs --no-color --tail 120 \
    >"${log}" 2>&1 || true
  grep -E 'agent endpoint creation failed|controller connection|session accepted|handshake|PermissionDenied|Permission denied|cannot create|ancestry|metadata invalid|attestation|Resource temporarily unavailable' \
    "${log}" | tail -40 >&2 || true
}

g6rd_start_agent_fleet() {
  local local_nodes="${1:?local nodes file is required}"
  local expected_nodes="${2:-${local_nodes}}"
  local canary_nodes="${G6RD_STATE}/agent-readiness-canary.tsv"
  local name service index=0
  local services=() restart_services=() batch=()
  [[ -s "${local_nodes}" && -s "${expected_nodes}" ]] || {
    echo "agent readiness requires non-empty local and expected node lists" >&2
    return 2
  }
  if ! g6rd_wait_until 15 2 "controller API endpoint before Agent startup" g6rd_api_ready; then
    echo "last controller readiness probe:" >&2
    g6rd_api_curl /readyz 2>&1 | tail -20 >&2 || true
    return 1
  fi

  head -n 1 "${local_nodes}" >"${canary_nodes}"
  name="$(cut -f1 "${canary_nodes}")"
  service="$(g6rd_agent_service_for_name "${name}")"
  services+=("${service}")
  g6rd_agent_compose up --detach --no-deps "${service}"
  if ! g6rd_wait_for_agent_readiness "${canary_nodes}" 45 2 \
    "${FD_ID} Agent canary to become active" "${service}"; then
    g6rd_capture_agent_logs canary-before-restart
    echo "restarting ${service} after the bounded readiness failure" >&2
    g6rd_agent_compose restart "${service}"
    g6rd_wait_for_agent_readiness "${canary_nodes}" 30 2 \
      "${FD_ID} Agent canary after one restart" "${service}" || {
      g6rd_capture_agent_logs canary-after-restart
      return 1
    }
  fi

  while IFS=$'\t' read -r name _; do
    index=$((index + 1))
    ((index == 1)) && continue
    service="$(g6rd_agent_service_for_name "${name}")"
    services+=("${service}")
    batch+=("${service}")
    if ((${#batch[@]} == 6)); then
      g6rd_agent_compose up --detach --no-deps "${batch[@]}"
      batch=()
      sleep 1
    fi
  done <"${local_nodes}"
  if ((${#batch[@]} > 0)); then
    g6rd_agent_compose up --detach --no-deps "${batch[@]}"
  fi

  if ! g6rd_wait_for_agent_readiness "${local_nodes}" 90 2 \
    "the controller to report the local Agent fleet active" "${services[@]}"; then
    g6rd_capture_agent_logs fleet-before-restart
    readarray -t restart_services < <(g6rd_agent_services_not_running "${local_nodes}")
    if ((${#restart_services[@]} == 0)); then
      readarray -t restart_services < <(g6rd_agent_services_needing_restart "${local_nodes}")
    fi
    if ((${#restart_services[@]} == 0)); then
      echo "no local Agent restart candidate was identified" >&2
      return 1
    fi
    echo "restarting only the unready local Agent services: ${restart_services[*]}" >&2
    g6rd_agent_compose restart "${restart_services[@]}"
    g6rd_wait_for_agent_readiness "${local_nodes}" 60 2 \
      "the local Agent fleet after one targeted restart" "${services[@]}" || {
      g6rd_capture_agent_logs fleet-after-restart
      return 1
    }
  fi

  # Each runner owns recovery only for its local services. The global barrier
  # still requires both failure domains before either scenario can proceed.
  g6rd_wait_for_agent_readiness "${expected_nodes}" 60 2 \
    "the controller to report both Agent fleets active" || {
    g6rd_capture_agent_logs global-readiness-timeout
    return 1
  }
}

# Validate journal observation with the same DAC boundary used by the live
# harness. The Agent owner must be able to read its database, while uid 0 in
# this cap-drop=ALL service must not gain implicit access to the owner-only
# journal tree.
g6rd_verify_agent_journal_observer_principals() {
  local service="${1:?agent service is required}" count
  if ! count="$(g6rd_agent_journal_query "${service}" \
    'SELECT count(*) FROM command_journal')"; then
    echo "the Agent journal owner probe failed for ${service}" >&2
    return 1
  fi
  [[ "${count}" =~ ^[0-9]+$ ]] || {
    echo "the Agent journal owner probe returned invalid output for ${service}" >&2
    return 1
  }
  # shellcheck disable=SC2016  # evaluated by the capless container principal
  if ! G6RD_COMPOSE_TIMEOUT_SECONDS=10 \
    g6rd_agent_compose exec -T --user 0:0 "${service}" \
      sh -eu -c '
        test "$(stat -c "%u:%g:%a" /run/ocservia-agent/journal)" = "65532:65532:700"
        test ! -x /run/ocservia-agent/journal
        test ! -r /run/ocservia-agent/journal/agent.db
        ! sqlite3 -readonly /run/ocservia-agent/journal/agent.db \
          "SELECT count(*) FROM command_journal" >/dev/null 2>&1
      '; then
    echo "the capless uid 0 Agent journal DAC probe failed for ${service}" >&2
    return 1
  fi
  if ! count="$(g6rd_agent_journal_query "${service}" \
    'SELECT count(*) FROM command_journal')" || [[ ! "${count}" =~ ^[0-9]+$ ]]; then
    echo "the Agent journal owner recheck failed for ${service}" >&2
    return 1
  fi
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
    g6rd_sampler_row agent "agent-${FD_ID}-01" "agent-${FD_ID}-01" 'cat /run/ocserv-platform/agent.pid' \
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
  command -v setsid >/dev/null 2>&1 || {
    echo "setsid is required for scoped harness background loops" >&2
    return 1
  }
  # shellcheck disable=SC2016  # the loop body is a separately quoted script
  # Each loop owns a process group. Cleanup can therefore terminate a blocked
  # Docker/psql descendant as well as the wrapper shell instead of orphaning a
  # run-scoped client after its stop sentinel is removed.
  nohup setsid env \
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

g6rd_stop_harness_loop() {
  local pid_file="${1:?loop pid file is required}" pid status=0 _
  [[ -s "${pid_file}" ]] || return 0
  pid="$(<"${pid_file}")"
  [[ "${pid}" =~ ^[1-9][0-9]*$ ]] || {
    echo "invalid harness loop process-group id in ${pid_file}" >&2
    return 1
  }
  kill -TERM -- "-${pid}" 2>/dev/null || true
  for _ in 1 2 3 4 5; do
    kill -0 -- "-${pid}" 2>/dev/null || break
    sleep 1
  done
  if kill -0 -- "-${pid}" 2>/dev/null; then
    kill -KILL -- "-${pid}" 2>/dev/null || true
  fi
  for _ in 1 2 3; do
    kill -0 -- "-${pid}" 2>/dev/null || break
    sleep 1
  done
  if kill -0 -- "-${pid}" 2>/dev/null; then
    echo "harness loop process group ${pid} did not terminate" >&2
    status=1
  fi
  if [[ "${status}" == 0 ]]; then
    rm -f "${pid_file}"
  fi
  return "${status}"
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
    rows="$(G6_DB_PORT="${port}" G6RD_PSQL_TIMEOUT_SECONDS=10 g6rd_psql -Atc \
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
    rows="$(G6_DB_PORT="${port}" G6RD_PSQL_TIMEOUT_SECONDS=10 g6rd_psql -Atc \
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
  local status=0
  touch "${G6RD_STATE}/sampler-stop"
  g6rd_stop_harness_loop "${G6RD_STATE}/sampler.pid" || status=1
  [[ ! -e "${G6RD_STATE}/sampler-failed-at" ]] || {
    echo "resource sampler failed closed at $(<"${G6RD_STATE}/sampler-failed-at")" >&2
    return 1
  }
  return "${status}"
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
  docker run --rm --pull=never -v "${dir}:/reclaim" postgres:17.10-bookworm \
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
    grep -E '^[[:space:]]+G6_RELAY_URL_[AB]:' "${G6RD_AGENT_COMPOSE}" \
      >"${ARTIFACT_DIR}/agent-relay-topology-${FD_ID}.txt" 2>&1 || true
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
  local status=0 volume image pid helper_container
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
  while IFS= read -r helper_container; do
    [[ -n "${helper_container}" ]] || continue
    docker rm --force "${helper_container}" >/dev/null 2>&1 || status=1
  done < <(docker ps --all --quiet --filter "label=ocservia.g6.run-id=${RUN_ID}")
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
  if [[ -n "$(docker ps --all --quiet --filter "label=ocservia.g6.run-id=${RUN_ID}")" ]]; then
    echo "scoped PostgreSQL helper container cleanup failed for ${RUN_ID}" >&2
    status=1
  fi
  return "${status}"
}

g6rd_cleanup_bounded() {
  local timeout_seconds="${G6RD_CLEANUP_TIMEOUT_SECONDS:-150}"
  [[ "${timeout_seconds}" =~ ^[0-9]+$ \
    && "${timeout_seconds}" -ge 30 && "${timeout_seconds}" -le 300 ]] || {
    echo "G6 cleanup timeout must be 30..300 seconds" >&2
    return 2
  }
  local status=0
  # shellcheck disable=SC2016  # the child shell owns its positional argument
  timeout --foreground --signal=TERM --kill-after=15s "${timeout_seconds}s" \
    bash -Eeuo pipefail -c '
      source "$1"
      g6rd_init_environment
      g6rd_cleanup
    ' bash "${G6RD_ROOT}/scripts/g6-readiness-lib.sh" || status=$?
  if [[ "${status}" == 124 || "${status}" == 137 ]]; then
    echo "G6 cleanup exceeded its ${timeout_seconds}-second hard limit" >&2
  fi
  return "${status}"
}
