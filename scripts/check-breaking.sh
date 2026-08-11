#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/env.sh
source "${ROOT}/scripts/env.sh"

if git -C "${ROOT}" cat-file -e origin/main:proto/buf.yaml 2>/dev/null; then
  (cd "${ROOT}" && buf breaking proto --against '.git#branch=origin/main,subdir=proto')
else
  echo "No contract exists on origin/main; validating FILE policy with a breaking fixture."
  temporary="$(mktemp -d)"
  trap 'rm -rf "${temporary}"' EXIT INT TERM
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

if git -C "${ROOT}" cat-file -e origin/main:openapi/openapi.yaml 2>/dev/null; then
  temporary="$(mktemp -d)"
  trap 'rm -rf "${temporary}"' EXIT INT TERM
  git -C "${ROOT}" show origin/main:openapi/openapi.yaml >"${temporary}/main.yaml"
  # Normalize only the deliberate fail-closed security migrations in the
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

  const enrollmentField = "expected_endpoint_id";
  if (!candidateSchemas.EnrollmentTokenRequest?.required?.includes(enrollmentField)) {
    throw new Error(`EnrollmentTokenRequest.${enrollmentField} must remain required`);
  }
  if (!schemas.EnrollmentTokenRequest?.properties?.[enrollmentField]) {
    throw new Error(`EnrollmentTokenRequest.${enrollmentField} is missing from the comparison baseline`);
  }
  schemas.EnrollmentTokenRequest.required = [
    ...new Set([...(schemas.EnrollmentTokenRequest.required ?? []), enrollmentField]),
  ];

  const configSecretRef = structuredClone(schemas.ConfigSecretRef);
  configSecretRef.required = ["secret_ref_id"];
  configSecretRef.properties = {
    secret_ref_id: { $ref: "#/components/schemas/UuidV7" },
  };
  assertExact("ConfigSecretRef", candidateSchemas.ConfigSecretRef, configSecretRef);
  schemas.ConfigSecretRef = configSecretRef;

  const configPlanSummary = structuredClone(schemas.ConfigPlanApprovalSummary);
  configPlanSummary.required = [
    "node_id",
    "expected_revision",
    "candidate_hash",
    "current_hash",
    "diff_redacted",
    "expires_at",
  ];
  configPlanSummary.properties.diff_redacted = {
    type: "string",
    maxLength: 262144,
    description: "Approver-visible secret-safe diff containing exact secret reference identifiers, providers, and versions without provider keys or values.",
  };
  assertExact("ConfigPlanApprovalSummary", candidateSchemas.ConfigPlanApprovalSummary, configPlanSummary);
  schemas.ConfigPlanApprovalSummary = configPlanSummary;

  for (const name of ["CertificateRevokeRequest", "CertificateP12Request"]) {
    const request = structuredClone(schemas[name]);
    request.required = ["expected_version", "certificate_version", "approval_id", "reason"];
    request.properties.certificate_version = { type: "integer", format: "int64", minimum: 1 };
    request.properties.approval_id = { $ref: "#/components/schemas/UuidV7" };
    assertExact(name, candidateSchemas[name], request);
    schemas[name] = request;
  }

  const certificateIssueSummary = structuredClone(schemas.CertificateApprovalSummary);
  assertExact(
    "CertificateIssueApprovalSummary",
    candidateSchemas.CertificateIssueApprovalSummary,
    certificateIssueSummary,
  );
  const certificateActionSummary = {
    type: "object",
    additionalProperties: false,
    required: ["action", "certificate_id", "certificate_version", "node_id"],
    properties: {
      action: {
        type: "string",
        enum: ["certificate.revoke", "certificate.private_key.export"],
      },
      certificate_id: { $ref: "#/components/schemas/UuidV7" },
      certificate_version: { type: "integer", format: "int64", minimum: 1 },
      node_id: { $ref: "#/components/schemas/UuidV7" },
      purpose: { type: "string", maxLength: 512 },
      artifact_request_id: { $ref: "#/components/schemas/UuidV7" },
      serial_number: { type: "string", maxLength: 128 },
      certificate_sha256: { type: "string", pattern: "^[0-9a-f]{64}$" },
    },
  };
  assertExact(
    "CertificateActionApprovalSummary",
    candidateSchemas.CertificateActionApprovalSummary,
    certificateActionSummary,
  );
  const certificateSummary = {
    oneOf: [
      { $ref: "#/components/schemas/CertificateIssueApprovalSummary" },
      { $ref: "#/components/schemas/CertificateActionApprovalSummary" },
    ],
  };
  assertExact("CertificateApprovalSummary", candidateSchemas.CertificateApprovalSummary, certificateSummary);
  schemas.CertificateIssueApprovalSummary = certificateIssueSummary;
  schemas.CertificateActionApprovalSummary = certificateActionSummary;
  schemas.CertificateApprovalSummary = certificateSummary;

  const approvalDecision = structuredClone(schemas.ApprovalDecision);
  approvalDecision.required = ["reason", "expected_request_hash"];
  assertExact("ApprovalDecision", candidateSchemas.ApprovalDecision, approvalDecision);
  schemas.ApprovalDecision = approvalDecision;

  const requestSummary = {
    oneOf: [
      structuredClone(schemas.Approval.properties.request_summary),
      { type: "object", additionalProperties: true },
    ],
  };
  assertExact(
    "Approval.request_summary",
    candidateSchemas.Approval?.properties?.request_summary,
    requestSummary,
  );
  schemas.Approval.properties.request_summary = requestSummary;
};

// Prove the allowlist rejects an unrelated breaking change inside a schema
// that contains an intentional migration.
const guardCandidate = structuredClone(candidate);
guardCandidate.components.schemas.CertificateRevokeRequest.properties.reason.maxLength = 127;
let guardRejected = false;
try {
  normalizeSecurityMigrations(structuredClone(baseline), guardCandidate);
} catch (error) {
  guardRejected = String(error).includes("CertificateRevokeRequest");
}
if (!guardRejected) {
  throw new Error("OpenAPI security migration allowlist accepted an unrelated schema change");
}

normalizeSecurityMigrations(baseline, candidate);
writeFileSync(baselinePath, stringify(baseline));
EOF
  )
  oasdiff breaking --fail-on ERR \
    "${temporary}/main.yaml" "${ROOT}/openapi/openapi.yaml"
else
  echo "No OpenAPI contract exists on origin/main; validating compatibility detection with a fixture."
  temporary="$(mktemp -d)"
  trap 'rm -rf "${temporary}"' EXIT INT TERM
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
