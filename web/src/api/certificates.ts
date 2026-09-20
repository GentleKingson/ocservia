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

export async function createCertificateP12(
  certificateId: string,
  request: CertificateP12Request,
  signal?: AbortSignal,
): Promise<ArtifactGrant> {
  return certificates.createCertificateP12(
    {
      certificateId,
      idempotencyKey: newIdempotencyKey(),
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
