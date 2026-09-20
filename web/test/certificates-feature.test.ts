import { effectScope, ref, type EffectScope } from "vue";
import type {
  Certificate,
  NodeObservedState,
  Operation,
} from "@ocservia/api-client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { getOperation } from "../src/api/operations";
import { useNodeCertificates } from "../src/features/certificates/useNodeCertificates";

const api = vi.hoisted(() => ({
  createCertificate: vi.fn(),
  createCertificateP12: vi.fn(),
  downloadCertificateArtifact: vi.fn(),
  getCertificate: vi.fn(),
  issueCertificate: vi.fn(),
  listNodeCertificates: vi.fn(),
  revokeCertificate: vi.fn(),
}));
vi.mock("../src/api/certificates", () => api);
vi.mock("../src/api/operations", () => ({ getOperation: vi.fn() }));

const scopes: EffectScope[] = [];
const certificate = {
  id: "certificate-a",
  nodeId: "node-a",
  workspaceId: "workspace-a",
  operationId: "csr-operation",
  version: 3,
  state: "csr_ready",
} as Certificate;
const otherCertificate = {
  ...certificate,
  id: "certificate-b",
  operationId: "other-csr-operation",
};

function setup() {
  const currentNode = ref({
    id: "node-a",
    name: "Node A",
    version: 31,
  } as NodeObservedState);
  const readReady = ref(true);
  const workspace = ref({ id: "workspace-a", generation: 1 });
  const trackOperation = vi.fn().mockResolvedValue(undefined);
  const scope = effectScope();
  scopes.push(scope);
  const feature = scope.run(() =>
    useNodeCertificates({
      currentNode,
      readReady,
      workspaceContext: () => workspace.value,
      trackOperation,
      t: (key) => key,
    }),
  );
  if (!feature) throw new Error("Certificate scope did not start");
  return { feature, currentNode, readReady, workspace, trackOperation, scope };
}

beforeEach(() => {
  vi.useFakeTimers();
  vi.resetAllMocks();
  const storage = new Map<string, string>();
  vi.stubGlobal("sessionStorage", {
    getItem: (key: string) => storage.get(key) ?? null,
    setItem: (key: string, value: string) => storage.set(key, value),
  });
  api.listNodeCertificates.mockResolvedValue([]);
  api.createCertificate.mockResolvedValue(certificate);
  api.issueCertificate.mockResolvedValue({ ...certificate, state: "issued" });
});
afterEach(async () => {
  scopes.splice(0).forEach((scope) => {
    scope.stop();
  });
  await vi.advanceTimersByTimeAsync(10 * 60_000);
  expect(vi.getTimerCount()).toBe(0);
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe("certificate feature without a page or store", () => {
  it("keeps local forms and preserves CSR, issue and P12 inputs and tracking", async () => {
    const { feature, trackOperation } = setup();
    const other = setup().feature;
    await feature.openCertificate();
    feature.certificateDnsNames.value =
      " node.example, node.example, alternate.example ";
    feature.certificateReason.value = " certificate ";
    await feature.submitCertificateRequest();
    expect(other.certificateDialog.value).toBe(false);
    expect(other.certificate.value).toBeUndefined();
    expect(api.createCertificate).toHaveBeenCalledExactlyOnceWith("node-a", {
      expectedVersion: 31,
      commonName: "Node A",
      dnsNames: new Set(["node.example", "alternate.example"]),
      keyBits: 3072,
      reason: "certificate",
    });
    feature.certificateApproval.value = " approval ";
    await feature.submitCertificateIssue();
    expect(api.issueCertificate).toHaveBeenCalledExactlyOnceWith(
      "certificate-a",
      {
        approvalId: "approval",
        reason: "certificate",
      },
    );
    api.createCertificateP12.mockResolvedValue({
      artifactId: "artifact-a",
      operation: { id: "p12-operation", nodeId: "node-a", state: "queued" },
      downloadToken: "one-time-token",
      password: "one-time-password",
      expiresAt: new Date(Date.now() + 60_000).toISOString(),
    });
    await feature.createP12();
    expect(api.createCertificateP12).toHaveBeenCalledExactlyOnceWith(
      "certificate-a",
      {
        expectedVersion: 31,
        certificateVersion: 3,
        approvalId: "approval",
        reason: "certificate",
      },
    );
    expect(trackOperation).toHaveBeenCalledExactlyOnceWith("p12-operation");
    expect(feature.certificateDialog.value).toBe(true);
    expect(feature.certificateGrant.value?.downloadToken).toBe(
      "one-time-token",
    );
  });

  it("requires a successful authorized read before opening", async () => {
    const { feature, readReady } = setup();
    readReady.value = false;
    await feature.openCertificate();
    expect(feature.certificateDialog.value).toBe(false);
    expect(api.listNodeCertificates).not.toHaveBeenCalled();
  });

  it.each([
    {
      scenario: "the receipt certificate is still active",
      records: [otherCertificate, certificate],
      selectedId: certificate.id,
      restoresOperation: true,
    },
    {
      scenario: "the receipt certificate is revoked",
      records: [{ ...certificate, state: "revoked" }, otherCertificate],
      selectedId: "certificate-b",
      restoresOperation: false,
    },
    {
      scenario: "the receipt certificate is missing",
      records: [otherCertificate],
      selectedId: "certificate-b",
      restoresOperation: false,
    },
    {
      scenario: "no active certificate remains",
      records: [{ ...certificate, state: "revoked" }],
      selectedId: undefined,
      restoresOperation: false,
    },
  ])(
    "binds the restored operation to the selected certificate when $scenario",
    async ({ records, selectedId, restoresOperation }) => {
      const { feature, trackOperation } = setup();
      await feature.openCertificate();
      feature.certificateReason.value = "certificate";
      await feature.submitCertificateRequest();
      feature.certificateDialog.value = false;

      const operation = {
        id: "csr-operation",
        nodeId: "node-a",
        state: "succeeded",
      } as Operation;
      api.listNodeCertificates.mockResolvedValueOnce(records);
      vi.mocked(getOperation).mockResolvedValue(operation);
      await feature.openCertificate();

      expect(feature.certificate.value?.id).toBe(selectedId);
      expect(feature.certificateOperation.value).toEqual(
        restoresOperation ? operation : undefined,
      );
      if (restoresOperation)
        expect(getOperation).toHaveBeenCalledExactlyOnceWith(
          operation.id,
          expect.any(AbortSignal),
        );
      else expect(getOperation).not.toHaveBeenCalled();
      expect(api.createCertificate).toHaveBeenCalledTimes(1);
      expect(trackOperation).not.toHaveBeenCalled();
      expect(feature.certificateLoading.value).toBe(false);
      expect(feature.certificateError.value).toBe("");
    },
  );

  it.each(["node", "workspace"])(
    "fences mutations with the supplied %s context",
    async (kind) => {
      const { feature, currentNode, workspace } = setup();
      await feature.openCertificate();
      feature.certificateReason.value = "certificate";
      if (kind === "node")
        currentNode.value = { ...currentNode.value, id: "node-b" };
      else workspace.value = { id: "workspace-a", generation: 3 };
      await feature.submitCertificateRequest();
      expect(api.createCertificate).not.toHaveBeenCalled();
    },
  );

  it("aborts reads and rejects late state writes when its own scope is disposed", async () => {
    const { feature, scope } = setup();
    let resolve!: (records: Certificate[]) => void;
    api.listNodeCertificates.mockReturnValueOnce(
      new Promise<Certificate[]>((done) => {
        resolve = done;
      }),
    );
    const pending = feature.openCertificate();
    const signal = api.listNodeCertificates.mock.calls[0]?.[1] as AbortSignal;
    scope.stop();
    expect(signal.aborted).toBe(true);
    resolve([certificate]);
    await pending;
    expect(feature.certificateDialog.value).toBe(false);
    expect(feature.certificate.value).toBeUndefined();
    expect(feature.certificateError.value).toBe("");
    expect(feature.certificateLoading.value).toBe(false);
  });
});
