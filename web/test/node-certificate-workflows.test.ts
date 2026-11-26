import { nextTick } from "vue";
import type { ArtifactGrant, Certificate } from "@/api/models";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const mocks = vi.hoisted(() => {
  const fleet: Record<string, unknown> = {};
  return {
    fleet,
    route: { params: { nodeId: "node-a" } },
    cachedWorkspaceId: vi.fn(),
    createConfigPlan: vi.fn(),
    getConfigPlan: vi.fn(),
    applyConfigPlan: vi.fn(),
    listNodeCertificates: vi.fn(),
    createCertificate: vi.fn(),
    getCertificate: vi.fn(),
    issueCertificate: vi.fn(),
    createCertificateP12: vi.fn(),
    revokeCertificate: vi.fn(),
    downloadCertificateArtifact: vi.fn(),
    getOperation: vi.fn(),
    getUserPasswordSealingKey: vi.fn(),
    getUserPolicy: vi.fn(),
    setUserPolicy: vi.fn(),
  };
});
vi.mock("../src/shared/fleet", () => ({ useFleetStore: () => mocks.fleet }));
vi.mock("vue-router", () => ({ useRoute: () => mocks.route }));
vi.mock("vue-i18n", () => ({ useI18n: () => ({ t: (key: string) => key }) }));
vi.mock("../src/api/workspace", async (original) => ({
  ...(await original<typeof import("../src/api/workspace")>()),
  cachedWorkspaceId: mocks.cachedWorkspaceId,
}));
vi.mock("../src/api/configuration", async (original) => ({
  ...(await original<typeof import("../src/api/configuration")>()),
  createConfigPlan: mocks.createConfigPlan,
  getConfigPlan: mocks.getConfigPlan,
  applyConfigPlan: mocks.applyConfigPlan,
}));
vi.mock("../src/api/certificates", async (original) => ({
  ...(await original<typeof import("../src/api/certificates")>()),
  listNodeCertificates: mocks.listNodeCertificates,
  createCertificate: mocks.createCertificate,
  getCertificate: mocks.getCertificate,
  issueCertificate: mocks.issueCertificate,
  createCertificateP12: mocks.createCertificateP12,
  revokeCertificate: mocks.revokeCertificate,
  downloadCertificateArtifact: mocks.downloadCertificateArtifact,
}));
vi.mock("../src/api/operations", async (original) => ({
  ...(await original<typeof import("../src/api/operations")>()),
  getOperation: mocks.getOperation,
}));
vi.mock("../src/api/users", async (original) => ({
  ...(await original<typeof import("../src/api/users")>()),
  getUserPasswordSealingKey: mocks.getUserPasswordSealingKey,
  getUserPolicy: mocks.getUserPolicy,
  setUserPolicy: mocks.setUserPolicy,
}));

import {
  readCertificateGrant,
  type NodeWorkflowContext,
} from "../src/features/node-workflow";
import {
  certApiFor,
  p12Grant,
  useNodeDetailHarness,
  workflowCertificate as certificate,
  workflowOperation as operation,
} from "./node-workflow-harness";

const harness = useNodeDetailHarness(mocks);

beforeEach(harness.reset);
afterEach(harness.teardown);

describe("node certificate workflow isolation", () => {
  it.each(["success", "error"])(
    "ignores late certificate %s and finally after reopen",
    async (result) => {
      const view = await harness.mount();
      const old = Promise.withResolvers<Certificate[]>();
      const fresh = Promise.withResolvers<Certificate[]>();
      mocks.listNodeCertificates
        .mockReturnValueOnce(old.promise)
        .mockReturnValueOnce(fresh.promise);
      const first = view.certificates.openCertificate();
      const signal = mocks.listNodeCertificates.mock
        .calls[0]?.[1] as AbortSignal;
      view.certificates.certificateDialog = false;
      await nextTick();
      const second = view.certificates.openCertificate();
      if (result === "success") old.resolve([certificate]);
      else old.reject(new Error("old failure"));
      await first;
      expect(signal.aborted).toBe(true);
      expect(view.certificates.certificate).toBeUndefined();
      expect(view.certificates.certificateError).toBe("");
      expect(view.certificates.certificateLoading).toBe(true);
      fresh.resolve([]);
      await second;
      expect(view.certificates.certificateLoading).toBe(false);
    },
  );

  it.each([
    "submitCertificateIssue",
    "createP12",
    "revokeCurrentCertificate",
  ] as const)(
    "isolates late %s completion after a Workspace switch",
    async (method) => {
      const view = await harness.mount();
      mocks.listNodeCertificates.mockResolvedValueOnce([certificate]);
      await view.certificates.openCertificate();
      const accepted = Promise.withResolvers<unknown>();
      const api = certApiFor(mocks, method);
      api.mockReturnValueOnce(accepted.promise);
      view.certificates.certificateReason = "request";
      const pending = view.certificates[method]();
      await harness.changeContext(view, "workspace");
      mocks.cachedWorkspaceId.mockReturnValue("workspace-b");
      await view.certificates.openCertificate();
      accepted.resolve(
        method === "submitCertificateIssue"
          ? certificate
          : method === "createP12"
            ? p12Grant({ download_token: "secret", password: "secret" })
            : operation,
      );
      await pending;
      expect(view.certificates.certificate).toBeUndefined();
      expect(view.certificates.certificateGrant).toBeUndefined();
      expect(mocks.fleet.trackOperation).not.toHaveBeenCalled();
      expect(mocks.getCertificate).not.toHaveBeenCalled();
      expect(api).toHaveBeenCalledTimes(1);
    },
  );

  it.each(["close", "node", "workspace", "unmount"])(
    "delivers an already requested artifact after %s",
    async (kind) => {
      const view = await harness.mount();
      mocks.listNodeCertificates.mockResolvedValue([certificate]);
      await view.certificates.openCertificate();
      view.certificates.certificateReason = "export";
      await view.certificates.createP12();
      const blob = Promise.withResolvers<Blob>();
      const click = vi.fn();
      const createObjectURL = vi
        .spyOn(URL, "createObjectURL")
        .mockReturnValue("blob:p12");
      const revokeObjectURL = vi
        .spyOn(URL, "revokeObjectURL")
        .mockImplementation(() => {});
      const createElement = vi.fn(() => ({ href: "", download: "", click }));
      vi.stubGlobal("document", { createElement });
      mocks.downloadCertificateArtifact.mockReturnValueOnce(blob.promise);
      const pending = view.certificates.downloadP12();
      await harness.changeContext(view, kind);
      const reopening =
        kind === "unmount"
          ? Promise.resolve()
          : view.certificates.openCertificate();
      if (kind === "close") await view.certificates.downloadP12();
      blob.resolve(new Blob(["p12"]));
      await pending;
      await reopening;
      expect(createElement).toHaveBeenCalledExactlyOnceWith("a");
      expect(click).toHaveBeenCalledTimes(1);
      expect(createObjectURL).toHaveBeenCalledTimes(1);
      await vi.advanceTimersByTimeAsync(999);
      expect(revokeObjectURL).not.toHaveBeenCalled();
      await vi.advanceTimersByTimeAsync(1);
      expect(revokeObjectURL).toHaveBeenCalledExactlyOnceWith("blob:p12");
      expect(mocks.downloadCertificateArtifact).toHaveBeenCalledExactlyOnceWith(
        "artifact-a",
        "one-time-token",
      );
      expect(
        view.certificates.certificateGrant?.download_token,
      ).toBeUndefined();
      if (kind === "close" || kind === "workspace")
        expect(view.certificates.certificateGrant?.password).toBe(
          "one-time-secret",
        );
      expect(view.certificates.certificateError).toBe("");
    },
  );

  it("recovers an accepted P12 grant after unmount without persisting its credentials or resending", async () => {
    let view = await harness.mount();
    mocks.listNodeCertificates.mockResolvedValue([certificate]);
    await view.certificates.openCertificate();
    view.certificates.certificateReason = "export";
    const saved = vi.spyOn(sessionStorage, "setItem");
    const accepted = Promise.withResolvers<unknown>();
    mocks.createCertificateP12.mockReturnValueOnce(accepted.promise);
    const pending = view.certificates.createP12();
    harness.unmount();
    accepted.resolve(
      p12Grant({
        password: "private-password",
        download_token: "private-token",
      }),
    );
    await pending;
    expect(JSON.stringify(saved.mock.calls)).not.toContain("private-");
    expect(JSON.stringify(saved.mock.calls)).toContain(operation.id);
    view = await harness.mount();
    await view.certificates.openCertificate();
    expect(view.certificates.certificateOperation?.id).toBe(operation.id);
    expect(view.certificates.certificateGrant).toMatchObject({
      password: "private-password",
      download_token: "private-token",
    });
    expect(mocks.createCertificateP12).toHaveBeenCalledTimes(1);
    expect(mocks.fleet.trackOperation).not.toHaveBeenCalled();
  });

  it.each([
    "submitCertificateRequest",
    "submitCertificateIssue",
    "createP12",
    "revokeCurrentCertificate",
  ] as const)(
    "waits for pending %s across remount without resending",
    async (method) => {
      let view = await harness.mount();
      mocks.listNodeCertificates.mockResolvedValue([certificate]);
      await view.certificates.openCertificate();
      const api = certApiFor(mocks, method);
      const accepted = Promise.withResolvers<unknown>();
      api.mockReturnValueOnce(accepted.promise);
      view.certificates.certificateReason = "first";
      const first = view.certificates[method]();
      harness.unmount();
      view = await harness.mount();
      const reopening = view.certificates.openCertificate();
      await nextTick();
      view.certificates.certificateReason = "second";
      await view.certificates[method]();
      const calls = api.mock.calls.length;
      accepted.resolve(
        method === "createP12"
          ? p12Grant({
              download_token: "private-token",
              password: "private-password",
            })
          : method === "revokeCurrentCertificate"
            ? operation
            : certificate,
      );
      await first;
      await reopening;
      expect(calls).toBe(1);
      expect(api).toHaveBeenCalledTimes(1);
      expect(view.certificates.certificateLoading).toBe(false);
      if (method === "createP12")
        expect(view.certificates.certificateGrant?.password).toBe(
          "private-password",
        );
    },
  );

  it("keeps grant credentials isolated and clears them at expiry", async () => {
    const view = await harness.mount();
    mocks.listNodeCertificates.mockResolvedValue([certificate]);
    await view.certificates.openCertificate();
    view.certificates.certificateReason = "export";
    await view.certificates.createP12();
    const context: NodeWorkflowContext = {
      nodeId: "node-a",
      workspaceId: "workspace-a",
      signal: new AbortController().signal,
    };
    const response = (await mocks.createCertificateP12.mock.results[0]
      ?.value) as ArtifactGrant;
    expect(
      readCertificateGrant({ ...context, nodeId: "node-b" }, certificate.id),
    ).toBeUndefined();
    expect(
      readCertificateGrant(
        { ...context, workspaceId: "workspace-b" },
        certificate.id,
      ),
    ).toBeUndefined();
    expect(
      readCertificateGrant(context, "different-certificate"),
    ).toBeUndefined();
    await view.certificates.createP12();
    expect(mocks.createCertificateP12).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(60_000);
    expect(readCertificateGrant(context, certificate.id)).toBeUndefined();
    expect(view.certificates.certificateGrant?.password).toBeUndefined();
    expect(view.certificates.certificateGrant?.download_token).toBeUndefined();
    expect(response.password).toBeUndefined();
    expect(response.download_token).toBeUndefined();
    expect(vi.getTimerCount()).toBe(0);
  });
});
