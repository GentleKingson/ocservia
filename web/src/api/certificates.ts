import type {
  CertificateRequest,
  CertificateIssueRequest,
  CertificateP12Request,
  CertificateRevokeRequest,
} from "./models";
import { client, newIdempotencyKey, read, ResponseError } from "./transport";

export function createCertificate(
  nodeId: string,
  request: CertificateRequest,
  signal?: AbortSignal,
) {
  return read(
    client.POST("/nodes/{node_id}/certificates", {
      params: {
        path: { node_id: nodeId },
        header: { "Idempotency-Key": newIdempotencyKey() },
      },
      body: {
        expected_version: request.expectedVersion,
        common_name: request.commonName,
        key_bits: request.keyBits,
        reason: request.reason,
        ...(request.dnsNames ? { dns_names: request.dnsNames } : {}),
      },
      signal: signal ?? null,
    }),
  );
}

export function getCertificate(certificateId: string, signal?: AbortSignal) {
  return read(
    client.GET("/certificates/{certificate_id}", {
      params: { path: { certificate_id: certificateId } },
      signal: signal ?? null,
    }),
  );
}

export async function listNodeCertificates(
  nodeId: string,
  signal?: AbortSignal,
) {
  const { items } = await read(
    client.GET("/nodes/{node_id}/certificates", {
      params: { path: { node_id: nodeId } },
      signal: signal ?? null,
    }),
  );
  return items;
}

export function issueCertificate(
  certificateId: string,
  request: CertificateIssueRequest,
  signal?: AbortSignal,
) {
  return read(
    client.POST("/certificates/{certificate_id}:issue", {
      params: { path: { certificate_id: certificateId } },
      body: request,
      signal: signal ?? null,
    }),
  );
}

// Artifact requests use the domain UUIDv7 contract, independently of browser sessions.
export function newArtifactRequestId(): string {
  const bytes = crypto.getRandomValues(new Uint8Array(16));
  let timestamp = BigInt(Date.now());
  for (let index = 5; index >= 0; index -= 1) {
    bytes[index] = Number(timestamp & 255n);
    timestamp >>= 8n;
  }
  bytes[6] = ((bytes[6] ?? 0) & 15) | 0x70;
  bytes[8] = ((bytes[8] ?? 0) & 63) | 0x80;
  const hex = Array.from(bytes, (value) =>
    value.toString(16).padStart(2, "0"),
  ).join("");
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}

export function createCertificateP12(
  certificateId: string,
  request: CertificateP12Request,
  signal?: AbortSignal,
) {
  return read(
    client.POST("/certificates/{certificate_id}:p12", {
      params: {
        path: { certificate_id: certificateId },
        header: { "Idempotency-Key": request.artifactRequestId },
      },
      body: {
        expected_version: request.expectedVersion,
        certificate_version: request.certificateVersion,
        artifact_request_id: request.artifactRequestId,
        reason: request.reason,
      },
      signal: signal ?? null,
    }),
  );
}

export function revokeCertificate(
  certificateId: string,
  request: CertificateRevokeRequest,
  signal?: AbortSignal,
) {
  return read(
    client.POST("/certificates/{certificate_id}:revoke", {
      params: {
        path: { certificate_id: certificateId },
        header: { "Idempotency-Key": newIdempotencyKey() },
      },
      body: {
        expected_version: request.expectedVersion,
        certificate_version: request.certificateVersion,
        reason: request.reason,
      },
      signal: signal ?? null,
    }),
  );
}

export async function downloadCertificateArtifact(
  artifactId: string,
  token: string,
): Promise<Blob> {
  const { data, response } = await client.GET("/artifacts/{artifact_id}", {
    params: {
      path: { artifact_id: artifactId },
      header: { "X-Artifact-Token": token },
    },
    parseAs: "blob",
  });
  if (!data) throw new ResponseError(response);
  return data;
}
