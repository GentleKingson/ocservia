#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/env.sh
source "${ROOT}/scripts/env.sh"

(cd "${ROOT}/control-plane" && go test ./internal/certificates ./internal/operations ./internal/localslice ./internal/api -count=1)
(cd "${ROOT}/rust" && cargo test -p ocservia-agent-protocol -p ocservia-ocserv-adapter -p ocservia-privd -p ocservia-agent -p ocservia-transportd)

grep -Fq '/artifacts/{artifact_id}' "${ROOT}/openapi/openapi.yaml"
grep -Fq 'X-Artifact-Token' "${ROOT}/openapi/openapi.yaml"
grep -Fq 'rpc FetchArtifact' "${ROOT}/proto/ocserv/platform/transport/v1/transport.proto"
grep -Fq 'content_size BETWEEN 1 AND 67108864' "${ROOT}/control-plane/migrations/000016_certificate_secret_lifecycle.up.sql"
grep -Fq 'Opaque external secret references only' "${ROOT}/control-plane/migrations/000016_certificate_secret_lifecycle.up.sql"
grep -Fq 'certificate_p12_is_encrypted_bounded_and_replayable' "${ROOT}/rust/crates/ocserv-adapter/src/lib.rs"

if grep -REnE 'ca_private_key|private_key_pem|plaintext_password|artifact_(source_)?path|caller_path|target_path' \
  "${ROOT}/control-plane/internal/certificates" \
  "${ROOT}/control-plane/migrations/000016_certificate_secret_lifecycle.up.sql" \
  "${ROOT}/proto/ocserv/platform/agent/v1/agent.proto" \
  "${ROOT}/proto/ocserv/platform/transport/v1/transport.proto"; then
  echo "I17 exposed CA/private-key/plaintext-secret or caller-selected artifact path material" >&2
  exit 1
fi

printf 'I17 certificate, one-time artifact, secret-reference, and typed-boundary checks passed\n'
