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
  (cd "${ROOT}" && buf breaking proto --against "${temporary}/baseline/proto")
else
  echo "No contract exists on ${base_ref}; validating FILE policy with a breaking fixture."
  cp -R "${ROOT}/proto" "${temporary}/against"
  cp -R "${ROOT}/proto" "${temporary}/candidate"
  sed -i.bak '/string version = 2;/d' \
    "${temporary}/candidate/ocserv/platform/transport/v1/transport.proto"
  rm -f "${temporary}/candidate/ocserv/platform/transport/v1/transport.proto.bak"
  if (cd "${temporary}" && buf breaking candidate --against against); then
    echo "breaking fixture was not rejected" >&2
    exit 1
  fi
fi

if git -C "${ROOT}" cat-file -e "${base_sha}:openapi/openapi.yaml" 2>/dev/null; then
  git -C "${ROOT}" show "${base_sha}:openapi/openapi.yaml" >"${temporary}/main.yaml"
  # Normalize only the deliberate security and single-Admin migrations in the
  # comparison baseline so every unrelated OpenAPI break remains an error.
  (cd "${ROOT}/web" && node --input-type=module - \
    "${temporary}/main.yaml" "${ROOT}/openapi/openapi.yaml" <<'EOF'
import { readFileSync, writeFileSync } from "node:fs";
import { parse, stringify } from "yaml";

const [, , baselinePath, candidatePath] = process.argv;
const baseline = parse(readFileSync(baselinePath, "utf8"));
const candidate = parse(readFileSync(candidatePath, "utf8"));

const canonical = (value) => {
  if (Array.isArray(value)) {
    return value.map(canonical);
  }
  if (value !== null && typeof value === "object") {
    return Object.fromEntries(
      Object.keys(value).sort().map((key) => [key, canonical(value[key])]),
    );
  }
  return value;
};

const assertExact = (name, actual, expected) => {
  if (JSON.stringify(canonical(actual)) !== JSON.stringify(canonical(expected))) {
    throw new Error(`${name} contains changes outside the security migration allowlist`);
  }
};

const normalizeSecurityMigrations = (baselineDocument, candidateDocument) => {
  const schemas = baselineDocument.components?.schemas;
  const candidateSchemas = candidateDocument.components?.schemas;
  if (!schemas || !candidateSchemas) {
    throw new Error("OpenAPI component schemas are missing");
  }

  const configSecretRef = structuredClone(schemas.ConfigSecretRef);
  configSecretRef.required = ["secret_ref_id"];
  configSecretRef.properties = {
    secret_ref_id: { $ref: "#/components/schemas/UuidV7" },
  };
  assertExact("ConfigSecretRef", candidateSchemas.ConfigSecretRef, configSecretRef);
  schemas.ConfigSecretRef = configSecretRef;

  const sealedSecret = {
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
  assertExact(
    "UserPasswordSealedSecretV1",
    candidateSchemas.UserPasswordSealedSecretV1,
    sealedSecret,
  );
  schemas.UserPasswordSealedSecretV1 = sealedSecret;
  for (const name of ["UserCreateRequest", "PasswordRotateRequest"]) {
    const request = structuredClone(schemas[name]);
    request.required = request.required.filter((field) => field !== "secret_key_id");
    delete request.properties.secret_key_id;
    request.properties.sealed_password = {
      $ref: "#/components/schemas/UserPasswordSealedSecretV1",
    };
    assertExact(name, candidateSchemas[name], request);
    schemas[name] = request;
  }

  for (const name of ["CertificateRevokeRequest", "CertificateP12Request"]) {
    const request = structuredClone(schemas[name]);
    request.required = ["expected_version", "certificate_version", "reason"];
    request.properties.certificate_version = { type: "integer", format: "int64", minimum: 1 };
    delete request.properties.approval_id;
    if (name === "CertificateP12Request") {
      request.required.splice(2, 0, "artifact_request_id");
      request.properties.artifact_request_id = { $ref: "#/components/schemas/UuidV7" };
    }
    assertExact(name, candidateSchemas[name], request);
    schemas[name] = request;
  }

  // Retire only schemas owned by removed multi-user, OIDC, RBAC, enrollment-token,
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
    assertExact(name, candidateSchemas[name], undefined);
    delete schemas[name];
  }
  assertExact("ApprovalId", candidateDocument.components?.parameters?.ApprovalId, undefined);
  if (baselineDocument.components?.parameters) {
    delete baselineDocument.components.parameters.ApprovalId;
  }

  if (baselineDocument.components?.securitySchemes?.sessionCookie) {
    const sessionCookie = structuredClone(baselineDocument.components.securitySchemes.sessionCookie);
    sessionCookie.description = "Shared Secure, HttpOnly, SameSite server-side session established by Local or break-glass authentication.";
    assertExact("sessionCookie", candidateDocument.components?.securitySchemes?.sessionCookie, sessionCookie);
    baselineDocument.components.securitySchemes.sessionCookie = sessionCookie;
  }

  // No other route deletion is normalized.
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
    assertExact(path, candidateDocument.paths[path], undefined);
    delete baselineDocument.paths[path];
  }

  assertExact("/auth/login.get", candidateDocument.paths["/auth/login"]?.get, undefined);
  const loginPath = baselineDocument.paths["/auth/login"];
  if (loginPath) {
    delete loginPath.get;
  }

  for (const path of ["/nodes/{node_id}/revocation", "/nodes/{node_id}/service:reload"]) {
    const operation = baselineDocument.paths[path]?.post;
    if (!operation?.parameters) {
      continue;
    }
    const nextParameters = operation.parameters.filter(
      (parameter) => parameter?.$ref !== "#/components/parameters/ApprovalId",
    );
    assertExact(
      `${path}.post.parameters`,
      candidateDocument.paths[path]?.post?.parameters,
      nextParameters,
    );
    operation.parameters = nextParameters;
  }

  const configPlanRequest = structuredClone(schemas.ConfigPlanRequest);
  delete configPlanRequest.properties.node_variables;
  assertExact("ConfigPlanRequest", candidateSchemas.ConfigPlanRequest, configPlanRequest);
  schemas.ConfigPlanRequest = configPlanRequest;

  const configPlan = structuredClone(schemas.ConfigPlan);
  delete configPlan.properties.approval_id;
  delete configPlan.properties.approval_status;
  assertExact("ConfigPlan", candidateSchemas.ConfigPlan, configPlan);
  schemas.ConfigPlan = configPlan;

  for (const name of [
    "AgentUpgradeRequest", "ConfigApplyRequest", "CertificateIssueRequest",
    "AgentRolloutCreateRequest", "AgentRollout",
  ]) {
    const schema = structuredClone(schemas[name]);
    schema.required = (schema.required ?? []).filter((field) => field !== "approval_id");
    delete schema.properties.approval_id;
    if (name === "AgentRolloutCreateRequest") {
      schema.description = "The immutable rollout request hash binds target version, sorted node set, batch size and stop-on-failure policy.";
    }
    assertExact(name, candidateSchemas[name], schema);
    schemas[name] = schema;
  }

  const bootstrapToken = structuredClone(schemas.NodeBootstrapToken);
  bootstrapToken.properties.token.pattern = "^(obt1_[A-Za-z0-9_-]{43}|oit1_[A-Za-z0-9_-]+\\.[A-Za-z0-9_-]{43})$";
  assertExact("NodeBootstrapToken", candidateSchemas.NodeBootstrapToken, bootstrapToken);
  schemas.NodeBootstrapToken = bootstrapToken;
};

// Prove the allowlist rejects an unrelated breaking change inside a migrated
// schema, parameter list, or retired path/method.
for (const [name, mutate] of [
  ["CertificateRevokeRequest", (doc) => { doc.components.schemas.CertificateRevokeRequest.properties.reason.maxLength = 127; }],
  ["CertificateP12Request", (doc) => { doc.components.schemas.CertificateP12Request.properties.artifact_request_id = { type: "string" }; }],
  ["ConfigPlanRequest", (doc) => { doc.components.schemas.ConfigPlanRequest.properties.ttl_seconds.maximum = 7200; }],
  ["ConfigPlan", (doc) => { delete doc.components.schemas.ConfigPlan.properties.materialized_hash; }],
  ["AgentUpgradeRequest", (doc) => { doc.components.schemas.AgentUpgradeRequest.properties.reason.maxLength = 256; }],
  ["ConfigApplyRequest", (doc) => { doc.components.schemas.ConfigApplyRequest.properties.reason.maxLength = 256; }],
  ["CertificateIssueRequest", (doc) => { doc.components.schemas.CertificateIssueRequest.properties.reason.maxLength = 256; }],
  ["AgentRolloutCreateRequest", (doc) => { doc.components.schemas.AgentRolloutCreateRequest.properties.batch_size.maximum = 10; }],
  ["AgentRollout", (doc) => { doc.components.schemas.AgentRollout.required = doc.components.schemas.AgentRollout.required.filter((field) => field !== "state"); }],
  ["NodeBootstrapToken", (doc) => { doc.components.schemas.NodeBootstrapToken.properties.token.pattern = ".*"; }],
  ["sessionCookie", (doc) => { doc.components.securitySchemes.sessionCookie.name = "bad_cookie"; }],
  ["/auth/login.get", (doc) => { doc.paths["/auth/login"].get = { responses: { "200": { description: "bad" } } }; }],
  ["/nodes/{node_id}/revocation.post.parameters", (doc) => { doc.paths["/nodes/{node_id}/revocation"].post.parameters = []; }],
  ["/role-bindings", (doc) => { doc.paths["/role-bindings"] = { post: { responses: { "201": { description: "bad" } } } }; }],
]) {
  const guardCandidate = structuredClone(candidate);
  mutate(guardCandidate);
  let guardRejected = false;
  try {
    normalizeSecurityMigrations(structuredClone(baseline), guardCandidate);
  } catch (error) {
    guardRejected = String(error).includes(name);
  }
  if (!guardRejected) {
    throw new Error(`OpenAPI migration allowlist accepted an unrelated ${name} change`);
  }
}

normalizeSecurityMigrations(baseline, candidate);
writeFileSync(baselinePath, stringify(baseline));
EOF
  )
  oasdiff breaking --fail-on ERR \
    "${temporary}/main.yaml" "${ROOT}/openapi/openapi.yaml"
else
  echo "No OpenAPI contract exists on ${base_ref}; validating compatibility detection with a fixture."
  (cd "${ROOT}/web" && node --input-type=module - \
    "${ROOT}/openapi/openapi.yaml" "${temporary}/candidate.yaml" <<'EOF'
import { readFileSync, writeFileSync } from "node:fs";
import { parse, stringify } from "yaml";

const [, , source, destination] = process.argv;
const document = parse(readFileSync(source, "utf8"));
delete document.paths["/operations"];
writeFileSync(destination, stringify(document));
EOF
  )
  if oasdiff breaking --fail-on ERR \
    "${ROOT}/openapi/openapi.yaml" "${temporary}/candidate.yaml"; then
    echo "OpenAPI breaking fixture was not rejected" >&2
    exit 1
  fi
fi
