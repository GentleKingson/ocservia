#!/bin/sh
# G6 readiness managed-node supervisor. One container per production node:
# privd starts as root with its fixed-path read fixtures, then the Agent
# runs as the unprivileged ocservia-agent account against that supervisor
# socket, mirroring the deployed two-principal unit split.
#
# G6_MODE selects the lifecycle phase:
#   prepare - provision the persistent endpoint identity and print its hex ID
#   enroll  - one-shot enrollment against the issued token, print the node id
#   run     - start privd and keep the Agent running until SIGTERM
#
# Bind layout (per agent, owned by the harness):
#   /run/ocservia-agent/identity  persistent endpoint identity (0700)
#   /run/ocservia-agent/journal   durable command journal and task stats
#   /run/ocservia-agent/secrets   verification and seal keys (read-only)
#   /run/ocservia-agent/state     node id, synthetic barrier, and pid files
#   /run/ocservia-privd            root-only persistent attestation state
set -eu

G6_MODE="${G6_MODE:-run}"
BASE=/run/ocservia-agent
IDENTITY="$BASE/identity"
JOURNAL="$BASE/journal"
SECRETS="$BASE/secrets"
STATE="$BASE/state"
PRIVD_STATE=/run/ocservia-privd
SOCKET_DIR=/run/ocserv-platform

require_env() {
    for name in "$@"; do
        eval "value=\"\${${name}:-}\""
        [ -n "${value}" ] || {
            echo "agent supervisor: ${name} is required" >&2
            exit 2
        }
    done
}

seal_sha256() {
    cat "$SECRETS/seal-$1-sha256"
}

agent_identity_args() {
    printf '%s\n' \
        --identity-dir "$IDENTITY" \
        --journal "$JOURNAL/agent.db" \
        --controller "$G6_CONTROLLER_ENDPOINT_ID"
}

agent_base_args() {
    agent_identity_args
    printf '%s\n' \
        --relay-mode custom \
        --relay-url "$G6_RELAY_URL_A" \
        --relay-url "$G6_RELAY_URL_B" \
        --relay-token-file "$SECRETS/relay-token" \
        --relay-ca-file "$SECRETS/relay-ca.pem"
}

as_agent() {
    exec setpriv --reuid 65532 --regid 65532 --clear-groups "$@"
}

load_seal_descriptors() {
    USER_SEAL_ID=g6-user-seal-v1
    P12_SEAL_ID=g6-p12-seal-v1
    USER_SEAL_SHA256="$(seal_sha256 user-password)"
    P12_SEAL_SHA256="$(seal_sha256 p12)"
}

case "$G6_MODE" in
prepare)
    require_env G6_CONTROLLER_ENDPOINT_ID
    # shellcheck disable=SC2046  # one flag per line, values contain no spaces
    as_agent /usr/local/bin/ocservia-agent $(agent_identity_args) --prepare-enrollment
    ;;
enroll)
    require_env G6_CONTROLLER_ENDPOINT_ID G6_RELAY_URL_A G6_RELAY_URL_B \
        G6_ENROLLMENT_TOKEN_FILE G6_ENROLLMENT_ENVIRONMENT
    load_seal_descriptors
    # shellcheck disable=SC2046  # one flag per line, values contain no spaces
    as_agent /usr/local/bin/ocservia-agent $(agent_base_args) \
        --enrollment-token-file "$G6_ENROLLMENT_TOKEN_FILE" \
        --enrollment-environment "$G6_ENROLLMENT_ENVIRONMENT" \
        --user-password-seal-key-id "$USER_SEAL_ID" \
        --user-password-seal-public-key-sha256 "$USER_SEAL_SHA256" \
        --p12-password-seal-key-id "$P12_SEAL_ID" \
        --p12-password-seal-public-key-sha256 "$P12_SEAL_SHA256"
    ;;
run)
    require_env G6_CONTROLLER_ENDPOINT_ID G6_RELAY_URL_A G6_RELAY_URL_B
    load_seal_descriptors
    NODE_ID="${G6_NODE_ID:-$(cat "$STATE/node-id" 2>/dev/null || true)}"
    [ -n "$NODE_ID" ] || {
        echo "agent supervisor: node id is missing (enroll first)" >&2
        exit 2
    }
    /usr/local/bin/ocservia-privd \
        --socket "$SOCKET_DIR/privd.sock" \
        --agent-uid 65532 \
        --node-id "$NODE_ID" \
        --controller-command-key-file "$SECRETS/command-verification-privd.pem" \
        --attestation-key-file "$PRIVD_STATE/attestation.key" \
        --user-password-seal-key-file "$SECRETS/seal-user-password.key" \
        --user-password-seal-key-id "$USER_SEAL_ID" \
        --user-password-seal-public-key-sha256 "$USER_SEAL_SHA256" \
        --p12-password-seal-key-file "$SECRETS/seal-p12.key" \
        --p12-password-seal-key-id "$P12_SEAL_ID" \
        --p12-password-seal-public-key-sha256 "$P12_SEAL_SHA256" &
    privd_pid=$!
    attempts=0
    while [ ! -S "$SOCKET_DIR/privd.sock" ] && [ "$attempts" -lt 100 ]; do
        kill -0 "$privd_pid" 2>/dev/null || {
            wait "$privd_pid"
            echo "agent supervisor: privd exited during startup" >&2
            exit 1
        }
        sleep 0.1
        attempts=$((attempts + 1))
    done
    [ -S "$SOCKET_DIR/privd.sock" ] || {
        echo "agent supervisor: privd socket never appeared" >&2
        kill "$privd_pid" 2>/dev/null || true
        exit 1
    }
    # shellcheck disable=SC2317,SC2329  # invoked by the signal trap below
    shutdown() {
        kill "$agent_pid" "$privd_pid" 2>/dev/null || true
        wait "$agent_pid" 2>/dev/null || true
        wait "$privd_pid" 2>/dev/null || true
        exit 0
    }
    trap shutdown TERM INT
    # shellcheck disable=SC2046  # one flag per line, values contain no spaces
    as_agent /usr/local/bin/ocservia-agent $(agent_base_args) \
        --privd-socket "$SOCKET_DIR/privd.sock" \
        --node-id "$NODE_ID" \
        --controller-command-key-file "$SECRETS/command-verification-agent.pem" \
        --user-password-seal-key-id "$USER_SEAL_ID" \
        --user-password-seal-public-key-sha256 "$USER_SEAL_SHA256" \
        --p12-password-seal-key-id "$P12_SEAL_ID" \
        --p12-password-seal-public-key-sha256 "$P12_SEAL_SHA256" \
        --synthetic-barrier-file "$STATE/synthetic-barrier" \
        --stats-file "$JOURNAL/tasks.json" &
    agent_pid=$!
    echo "$agent_pid" >"$STATE/agent.pid"
    echo "$privd_pid" >"$STATE/privd.pid"
    wait "$agent_pid"
    status=$?
    kill "$privd_pid" 2>/dev/null || true
    wait "$privd_pid" 2>/dev/null || true
    exit "$status"
    ;;
*)
    echo "agent supervisor: unknown G6_MODE '$G6_MODE'" >&2
    exit 2
    ;;
esac
