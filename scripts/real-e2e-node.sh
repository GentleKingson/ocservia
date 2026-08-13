#!/usr/bin/env bash
set -Eeuo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUN_ID="${RUN_ID:?RUN_ID is required}"
ARTIFACT_DIR="${ARTIFACT_DIR:?ARTIFACT_DIR is required}"
WORK="${RUNNER_TEMP:-/tmp}/ocservia-real-e2e-node-${RUN_ID}"
TARGET="${RUNNER_TEMP:-/tmp}/ocservia-real-e2e-target-${RUN_ID}"
IDENTITY="${WORK}/identity"
SECRETS="${WORK}/secrets"
OUTBOX="${WORK}/outbox"

case "${RUN_ID}" in
  *[!a-zA-Z0-9._-]*) echo "RUN_ID contains unsafe characters" >&2; exit 2 ;;
esac

require_endpoint() {
  [[ "${1:-}" =~ ^[0-9a-f]{64}$ ]] || { echo "invalid Controller EndpointID" >&2; return 1; }
}

prepare_endpoint() {
  local ready_dir="${1:?controller-ready directory is required}"
  local controller endpoint
  controller="$(<"${ready_dir}/controller-endpoint-id")"
  require_endpoint "${controller}"
  [[ "$(<"${ready_dir}/runner-instance")" != "$(hostname)" ]] || {
    echo "Controller and Agent resolved to the same runner instance" >&2
    return 1
  }
  mkdir -p "${IDENTITY}" "${SECRETS}" "${OUTBOX}/agent-endpoint" "${ARTIFACT_DIR}"
  chmod 0700 "${WORK}" "${IDENTITY}" "${SECRETS}"
  (cd "${ROOT}/rust" && CARGO_TARGET_DIR="${TARGET}" cargo build --locked --release --package ocservia-agent)
  endpoint="$("${TARGET}/release/ocservia-agent" --identity-dir "${IDENTITY}" \
    --controller "${controller}" --prepare-enrollment)"
  require_endpoint "${endpoint}"
  printf '%s\n' "${endpoint}" >"${OUTBOX}/agent-endpoint/agent-endpoint-id"
  hostname >"${OUTBOX}/agent-endpoint/runner-instance"
  printf '%s\n' "${controller}" >"${WORK}/controller-endpoint-id"
}

enroll() {
  local token_dir="${1:?enrollment token directory is required}"
  local controller user_hash p12_hash node_id
  controller="$(<"${WORK}/controller-endpoint-id")"
  require_endpoint "${controller}"
  install -m 0600 "${token_dir}/enrollment-token" "${SECRETS}/enrollment-token"
  openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "${SECRETS}/user-password-seal.pem" >/dev/null 2>&1
  openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "${SECRETS}/p12-password-seal.pem" >/dev/null 2>&1
  openssl pkey -in "${SECRETS}/user-password-seal.pem" -pubout -outform DER -out "${SECRETS}/user-password-seal.der" >/dev/null 2>&1
  openssl pkey -in "${SECRETS}/p12-password-seal.pem" -pubout -outform DER -out "${SECRETS}/p12-password-seal.der" >/dev/null 2>&1
  user_hash="$(sha256sum "${SECRETS}/user-password-seal.der" | awk '{print $1}')"
  p12_hash="$(sha256sum "${SECRETS}/p12-password-seal.der" | awk '{print $1}')"
  [[ "${user_hash}" != "${p12_hash}" ]]
  node_id="$("${TARGET}/release/ocservia-agent" \
    --identity-dir "${IDENTITY}" \
    --controller "${controller}" \
    --enrollment-token-file "${SECRETS}/enrollment-token" \
    --enrollment-environment development \
    --user-password-seal-key-id real-e2e-user-v1 \
    --user-password-seal-public-key-sha256 "${user_hash}" \
    --p12-password-seal-key-id real-e2e-p12-v1 \
    --p12-password-seal-public-key-sha256 "${p12_hash}")"
  [[ "${node_id}" =~ ^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$ ]] || {
    echo "enrollment did not return a UUIDv7 node ID" >&2
    return 1
  }
  mkdir -p "${OUTBOX}/enrollment-result"
  printf '%s\n' "${node_id}" >"${OUTBOX}/enrollment-result/node-id"
  hostname >"${OUTBOX}/enrollment-result/runner-instance"
}

collect_diagnostics() {
  local leaked=0 hit
  mkdir -p "${ARTIFACT_DIR}"
  printf 'runner=%s\nidentity_mode=persistent\nagent_uid=%s\n' "$(hostname)" "$(id -u)" \
    >"${ARTIFACT_DIR}/node-summary.txt"
  df -h >"${ARTIFACT_DIR}/disk.txt"
  for secret in "${SECRETS}/enrollment-token" "${SECRETS}/user-password-seal.pem" \
    "${SECRETS}/p12-password-seal.pem" "${IDENTITY}/endpoint.key"; do
    [[ -s "${secret}" ]] || continue
    while IFS= read -r hit; do
      rm -f -- "${hit}"
      leaked=1
    done < <(grep -RIlF -f "${secret}" "${ARTIFACT_DIR}" 2>/dev/null || true)
  done
  if grep -RIlE -- 'BEGIN ([A-Z ]+ )?PRIVATE KEY|ocpasswd|session[_ -]?cookie' "${ARTIFACT_DIR}" >/dev/null 2>&1; then
    while IFS= read -r hit; do rm -f -- "${hit}"; done \
      < <(grep -RIlE -- 'BEGIN ([A-Z ]+ )?PRIVATE KEY|ocpasswd|session[_ -]?cookie' "${ARTIFACT_DIR}" || true)
    leaked=1
  fi
  ((leaked == 0)) || {
    echo "a node diagnostic file contained secret material and was removed" >&2
    return 1
  }
}

cleanup_node() {
  rm -rf -- "${WORK}" "${TARGET}" \
    "${RUNNER_TEMP:-/tmp}/real-e2e-controller-ready" \
    "${RUNNER_TEMP:-/tmp}/real-e2e-enrollment-token"
}

case "${1:-}" in
  prepare) prepare_endpoint "${2:-}" ;;
  enroll) enroll "${2:-}" ;;
  diagnostics) collect_diagnostics ;;
  cleanup) cleanup_node ;;
  *) echo "usage: $0 {prepare CONTROLLER_DIR|enroll TOKEN_DIR|diagnostics|cleanup}" >&2; exit 2 ;;
esac
