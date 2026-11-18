import {
  CertificatesApi,
  type Certificate,
  type CertificateRequest,
  type CertificateIssueRequest,
  type CertificateP12Request,
  type CertificateRevokeRequest,
  type ArtifactGrant,
  type Operation,
} from "@ocservia/api-client";
import {
  authenticatedFetch,
  configuration,
  devAuthToken,
  newIdempotencyKey,
  requestInit,
} from "./transport";

const certificates = new CertificatesApi(configuration);

export async function createCertificate(
  nodeId: string,
  request: CertificateRequest,
  signal?: AbortSignal,
): Promise<Certificate> {
  return certificates.createCertificate(
    {
      nodeId,
      idempotencyKey: newIdempotencyKey(),
      certificateRequest: request,
    },
    requestInit(signal),
  );
}

export async function getCertificate(
  certificateId: string,
  signal?: AbortSignal,
): Promise<Certificate> {
  return certificates.getCertificate({ certificateId }, requestInit(signal));
}

export async function listNodeCertificates(
  nodeId: string,
  signal?: AbortSignal,
): Promise<Certificate[]> {
  const response = await certificates.listNodeCertificates(
    { nodeId },
    requestInit(signal),
  );
  return response.items;
}

export async function issueCertificate(
  certificateId: string,
  request: CertificateIssueRequest,
  signal?: AbortSignal,
): Promise<Certificate> {
  return certificates.issueCertificate(
    { certificateId, certificateIssueRequest: request },
    requestInit(signal),
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

export async function createCertificateP12(
  certificateId: string,
  request: CertificateP12Request,
  signal?: AbortSignal,
): Promise<ArtifactGrant> {
  return certificates.createCertificateP12(
    {
      certificateId,
      idempotencyKey: request.artifactRequestId,
      certificateP12Request: request,
    },
    requestInit(signal),
  );
}

export async function revokeCertificate(
  certificateId: string,
  request: CertificateRevokeRequest,
  signal?: AbortSignal,
): Promise<Operation> {
  return certificates.revokeCertificate(
    {
      certificateId,
      idempotencyKey: newIdempotencyKey(),
      certificateRevokeRequest: request,
    },
    requestInit(signal),
  );
}

export async function downloadCertificateArtifact(
  artifactId: string,
  token: string,
): Promise<Blob> {
  const response = await authenticatedFetch(
    `/api/v1/artifacts/${encodeURIComponent(artifactId)}`,
    {
      credentials: "same-origin",
      headers: {
        "X-Artifact-Token": token,
        ...(devAuthToken ? { Authorization: `Bearer ${devAuthToken}` } : {}),
      },
    },
  );
  if (!response.ok) throw new Error("Certificate artifact download failed");
  return response.blob();
}
