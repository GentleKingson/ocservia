#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/env.sh
source "${ROOT}/scripts/env.sh"

if (($# > 1)); then
  echo "usage: $0 [base-ref]" >&2
  exit 2
fi
base_ref="${1:-origin/main}"
if ! base_sha="$(git -C "${ROOT}" rev-parse --verify --end-of-options "${base_ref}^{commit}" 2>/dev/null)"; then
  echo "contract comparison baseline is not a commit: ${base_ref}" >&2
  exit 1
fi
if [[ "${base_sha}" == "$(git -C "${ROOT}" rev-parse HEAD)" ]]; then
  echo "contract comparison baseline must differ from the candidate" >&2
  exit 1
fi
temporary="$(mktemp -d)"
trap 'rm -rf "${temporary}"' EXIT INT TERM
if git -C "${ROOT}" cat-file -e "${base_sha}:proto/buf.yaml" 2>/dev/null; then
  mkdir -p "${temporary}/baseline"
  git -C "${ROOT}" archive "${base_sha}" proto | tar -x -C "${temporary}/baseline"
  # v2.0.0 resets the Agent wire contract. Accept only the exact old and new
  # schemas from that transition so comparisons for future edits still run.
  agent_proto=proto/ocserv/platform/agent/v1/agent.proto
  if [[ "$(git -C "${ROOT}" hash-object "${temporary}/baseline/${agent_proto}")" == 6fad3171cfb96d08d8cd9e42b66633db9ffa269b &&
        "$(git -C "${ROOT}" hash-object "${agent_proto}")" == c7c9254d1340927460e841c27cce03dc4c57733e ]]; then
    cp "${ROOT}/${agent_proto}" "${temporary}/baseline/${agent_proto}"
    # The private transport schema imports Agent types removed by the reset.
    # Buf already excludes it from compatibility checks.
    transport_proto=proto/ocserv/platform/transport/v1/transport.proto
    cp "${ROOT}/${transport_proto}" "${temporary}/baseline/${transport_proto}"
  fi
  (cd "${ROOT}" && buf breaking proto --against "${temporary}/baseline/proto")
fi

if git -C "${ROOT}" cat-file -e "${base_sha}:openapi/openapi.yaml" 2>/dev/null; then
  git -C "${ROOT}" show "${base_sha}:openapi/openapi.yaml" >"${temporary}/main.yaml"
  # Normalize deliberate contract changes in the comparison baseline so
  # unrelated OpenAPI breaks still fail the check.
  (cd "${ROOT}/web" && node --input-type=module - \
    "${temporary}/main.yaml" <<'EOF'
import { readFileSync, writeFileSync } from "node:fs";
import { parse, stringify } from "yaml";

const [, , baselinePath] = process.argv;
const baseline = parse(readFileSync(baselinePath, "utf8"));

const normalizeSecurityMigrations = (baselineDocument) => {
  const schemas = baselineDocument.components?.schemas;
  if (!schemas) {
    throw new Error("OpenAPI component schemas are missing");
  }

  if (schemas.ConfigSecretRef) {
    schemas.ConfigSecretRef.required = ["secret_ref_id"];
    schemas.ConfigSecretRef.properties = {
      secret_ref_id: { $ref: "#/components/schemas/UuidV7" },
    };
  }

  if (!schemas.UserPasswordSealedSecretV1) {
    schemas.UserPasswordSealedSecretV1 = {
      type: "object",
      additionalProperties: false,
      required: ["version", "purpose", "key_id", "ciphertext"],
      properties: {
        version: { type: "integer", format: "int32", enum: [1] },
        purpose: { type: "string", enum: ["user_password"] },
        key_id: {
          type: "string",
          minLength: 1,
          maxLength: 128,
          pattern: "^[A-Za-z0-9_.-]+$",
        },
        ciphertext: {
          type: "string",
          format: "byte",
          minLength: 44,
          maxLength: 5464,
          writeOnly: true,
          description: "Purpose-specific RSA-OAEP-SHA256 ciphertext; plaintext and cross-purpose ciphertext are never accepted.",
        },
      },
    };
  }
  for (const name of ["UserCreateRequest", "PasswordRotateRequest"]) {
    if (!schemas[name]) continue;
    schemas[name].required = (schemas[name].required ?? []).filter((field) => field !== "secret_key_id");
    delete schemas[name].properties?.secret_key_id;
    schemas[name].properties.sealed_password = {
      $ref: "#/components/schemas/UserPasswordSealedSecretV1",
    };
  }

  for (const name of ["CertificateRevokeRequest", "CertificateP12Request"]) {
    if (!schemas[name]) continue;
    schemas[name].required = ["expected_version", "certificate_version", "reason"];
    schemas[name].properties.certificate_version = { type: "integer", format: "int64", minimum: 1 };
    delete schemas[name].properties?.approval_id;
    if (name === "CertificateP12Request") {
      schemas[name].required.splice(2, 0, "artifact_request_id");
      schemas[name].properties.artifact_request_id = { $ref: "#/components/schemas/UuidV7" };
    }
  }

  // Retire schemas owned by removed multi-user, OIDC, RBAC, enrollment-token,
  // privd-attestation, batch-user, and independent-approval APIs.
  for (const name of [
    "ApprovalRequest", "ApprovalDecision", "ApprovalPage", "Approval",
    "ConfigPlanApprovalSummary", "CertificateApprovalSummary",
    "CertificateIssueApprovalSummary", "CertificateActionApprovalSummary",
    "AuthMethods", "RoleBindingRequest", "RoleBinding",
    "EnrollmentTokenRequest", "EnrollmentToken", "NodeApproval",
    "PrivdAttestationCredentialRequest", "PrivdAttestationCredential",
    "PrivdAttestationRegistration", "PrivdAttestationRevokeRequest",
    "PrivdAttestationKeyState",
    "UserBatchItemRequest", "UserBatchRequest", "UserBatchItem", "UserBatch",
    "UserOperationMetrics",
  ]) {
    delete schemas[name];
  }
  if (baselineDocument.components?.parameters) {
    delete baselineDocument.components.parameters.ApprovalId;
  }

  for (const path of [
    "/approval-requests",
    "/approval-requests/{approval_id}",
    "/approval-requests/{approval_id}:approve",
    "/local-users",
    "/local-users/{identity_id}:disable",
    "/local-users/{identity_id}:reset-password",
    "/role-bindings",
    "/enrollment-tokens",
    "/nodes/{node_id}/approval",
    "/nodes/{node_id}/privd-attestation-credentials",
    "/nodes/{node_id}/privd-attestation-keys:register",
    "/nodes/{node_id}/privd-attestation-keys:revoke",
    "/user-batches",
    "/user-batches/{batch_id}",
    "/user-operations/metrics",
    "/auth/methods",
    "/auth/callback",
  ]) {
    delete baselineDocument.paths[path];
  }

  const loginPath = baselineDocument.paths["/auth/login"];
  if (loginPath) {
    delete loginPath.get;
  }

  for (const path of ["/nodes/{node_id}/revocation", "/nodes/{node_id}/service:reload"]) {
    const operation = baselineDocument.paths[path]?.post;
    if (!operation?.parameters) continue;
    operation.parameters = operation.parameters.filter(
      (parameter) => parameter?.$ref !== "#/components/parameters/ApprovalId",
    );
  }

  if (schemas.ConfigPlanRequest?.properties) {
    delete schemas.ConfigPlanRequest.properties.node_variables;
  }
  if (schemas.ConfigPlan?.properties) {
    delete schemas.ConfigPlan.properties.approval_id;
    delete schemas.ConfigPlan.properties.approval_status;
  }

  for (const name of [
    "AgentUpgradeRequest", "ConfigApplyRequest", "CertificateIssueRequest",
    "AgentRolloutCreateRequest", "AgentRollout",
  ]) {
    if (!schemas[name]) continue;
    schemas[name].required = (schemas[name].required ?? []).filter((field) => field !== "approval_id");
    delete schemas[name].properties?.approval_id;
  }

  // obt1_ bootstrap tokens are retired; only oit1_ installation tokens remain.
  if (schemas.NodeBootstrapToken?.properties?.token) {
    schemas.NodeBootstrapToken.properties.token.pattern =
      "^oit1_[A-Za-z0-9_-]+\\.[A-Za-z0-9_-]{43}$";
  }
  if (schemas.NodeBootstrapTokenRequest?.properties?.installation) {
    schemas.NodeBootstrapTokenRequest.properties.installation.default = true;
  }
};

// The handler has always returned verification JSON for a failed audit.
// Correct only that erroneous baseline response; no runtime contract changed.
const normalizeAuditConflict = (before) => {
  const responses = before.paths["/audit:verify"]?.post?.responses;
  if (responses?.["409"]?.$ref !== "#/components/responses/Problem") return;
  responses["409"] = {
    description: "The chain or latest checkpoint failed verification.",
    content: { "application/json": { schema: { $ref: "#/components/schemas/AuditVerification" } } },
  };
};
const normalizeLegacyTelemetry = (before) => {
  const schemas = before.components.schemas;
  if (!schemas.TelemetryTimestamp) return;

  const legacyTimestamp = "#/components/schemas/TelemetryTimestamp";
  for (const schema of Object.values(schemas)) {
    for (const [name, property] of Object.entries(schema.properties ?? {})) {
      if (property.$ref === legacyTimestamp) {
        schema.properties[name] = { type: "string", format: "date-time" };
      }
    }
  }
  const since = before.paths["/nodes/{node_id}/telemetry"]?.get?.parameters
    ?.find((parameter) => parameter.in === "query" && parameter.name === "since");
  if (since?.schema?.$ref === legacyTimestamp) {
    since.schema = { type: "string", format: "date-time" };
  }
  for (const [schema, fields] of [
    ["UserGroupResourceState", [
      "desired_fingerprint", "observed_fingerprint",
      "desired_member_dimensions", "observed_member_dimensions",
    ]],
    ["Certificate", ["public_key_sha256"]],
    ["ConfigPlan", ["current_hash"]],
  ]) {
    for (const field of fields) delete schemas[schema]?.properties?.[field];
  }
  delete schemas.TelemetryTimestamp;
  delete schemas.ResourceMemberDimensions;
};
normalizeLegacyTelemetry(baseline);
normalizeAuditConflict(baseline);
normalizeSecurityMigrations(baseline);
writeFileSync(baselinePath, stringify(baseline));
EOF
  )
  oasdiff breaking --fail-on ERR \
    "${temporary}/main.yaml" "${ROOT}/openapi/openapi.yaml"
fi
