import { nextTick, reactive } from "vue";
import type {
  ArtifactGrant,
  Certificate,
  ConfigPlan,
  Operation,
} from "@/api/models";
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

import { mountNodeDetail } from "./node-detail-renderer";
import {
  beginNodeMutation,
  finishNodeMutation,
  readCertificateGrant,
  readNodeWorkflowReference,
  rememberNodeWorkflowReference,
  type NodeWorkflowContext,
} from "../src/features/node-workflow";

interface View {
  configuration: {
    configDialog: boolean;
    configLoading: boolean;
    configError: string;
    configPlan?: ConfigPlan;
    configOperation?: Operation;
    configReason: string;
    configApplyReason: string;
    openConfigPlan(): Promise<void>;
    submitConfigPlan(): Promise<void>;
    submitConfigApply(): Promise<void>;
  };
  certificates: {
    certificateDialog: boolean;
    certificateLoading: boolean;
    certificateError: string;
    certificate?: Certificate;
    certificateOperation?: Operation;
    certificateGrant?: { downloadToken?: string; password?: string };
    certificateCommonName: string;
    certificateReason: string;
    openCertificate(): Promise<void>;
    submitCertificateRequest(): Promise<void>;
    submitCertificateIssue(): Promise<void>;
    createP12(): Promise<void>;
    downloadP12(): Promise<void>;
    revokeCurrentCertificate(): Promise<void>;
  };
  userGroup: {
    desiredDialog: { kind: string } | undefined;
    desiredPassword: string;
    desiredName: string;
    desiredReason: string;
    desiredLoading: boolean;
    desiredError: string;
    openDesired(kind: "create" | "rotate"): void;
    submitDesired(): Promise<void>;
  };
  page: {
    currentOperation: Operation | undefined;
    operationBusy: boolean;
    detailState: string;
    detailLoading: boolean;
    selectRouteNode(): Promise<void>;
    policyDialog: { username: string } | undefined;
    openPolicy(username: string): void;
  };
}

const plan = {
  id: "plan-a",
  nodeId: "node-a",
  workspaceId: "workspace-a",
  operationId: "plan-operation",
  expectedRevision: 7,
  state: "succeeded",
  validation: "valid",
  warnings: [],
} as unknown as ConfigPlan;
const certificate = {
  id: "certificate-a",
  nodeId: "node-a",
  workspaceId: "workspace-a",
  operationId: "csr-operation",
  version: 3,
  state: "csr_ready",
} as Certificate;
const operation = {
  id: "operation-a",
  nodeId: "node-a",
  state: "succeeded",
} as Operation;
const p12Grant = (
  overrides: Partial<{ downloadToken: string; password: string }> = {},
) => ({
  artifactId: "artifact-a",
  operation,
  downloadToken: "one-time-token",
  password: "one-time-secret",
  expiresAt: new Date(Date.now() + 60_000).toISOString(),
  ...overrides,
});
type CertMethod =
  | "submitCertificateRequest"
  | "submitCertificateIssue"
  | "createP12"
  | "revokeCurrentCertificate";
function certApi(method: CertMethod) {
  return {
    submitCertificateRequest: mocks.createCertificate,
    submitCertificateIssue: mocks.issueCertificate,
    createP12: mocks.createCertificateP12,
    revokeCurrentCertificate: mocks.revokeCertificate,
  }[method];
}

let unmount: (() => void) | undefined;

async function mount(): Promise<View> {
  mocks.fleet.selected = {
    id: "node-a",
    name: "Node A",
    version: 31,
    configRevision: 7,
  };
  const { app, instance } = mountNodeDetail();
  unmount = () => {
    app.unmount();
    unmount = undefined;
  };
  await nextTick();
  await nextTick();
  type Owner<K extends keyof View> = { $: { setupState: View[K] } };
  return {
    page: (instance as unknown as Owner<"page">).$.setupState,
    configuration: (instance.$refs.configuration as Owner<"configuration">).$
      .setupState,
    certificates: (instance.$refs.certificates as Owner<"certificates">).$
      .setupState,
    userGroup: (instance.$refs.userGroup as Owner<"userGroup">).$.setupState,
  };
}

beforeEach(() => {
  vi.useFakeTimers();
  vi.resetAllMocks();
  vi.stubGlobal("window", new EventTarget());
  const storage = new Map<string, string>();
  vi.stubGlobal("sessionStorage", {
    getItem: (key: string) => storage.get(key) ?? null,
    setItem: (key: string, value: string) => storage.set(key, value),
    removeItem: (key: string) => storage.delete(key),
  });
  mocks.route = reactive({ params: { nodeId: "node-a" } });
  mocks.fleet = reactive({
    sessions: [],
    ipBans: [],
    initialized: true,
    selecting: false,
    selectionError: "",
    userGroupState: [],
    select: vi.fn().mockResolvedValue(undefined),
    start: vi.fn(),
    stop: vi.fn(),
    trackOperation: vi.fn().mockResolvedValue(undefined),
  });
  mocks.cachedWorkspaceId.mockReturnValue("workspace-a");
  mocks.createConfigPlan.mockResolvedValue(plan);
  mocks.getConfigPlan.mockResolvedValue(plan);
  mocks.applyConfigPlan.mockResolvedValue(operation);
  mocks.listNodeCertificates.mockResolvedValue([]);
  mocks.createCertificate.mockResolvedValue(certificate);
  mocks.getCertificate.mockResolvedValue(certificate);
  mocks.issueCertificate.mockResolvedValue({ ...certificate, state: "issued" });
  mocks.createCertificateP12.mockResolvedValue(p12Grant());
  mocks.revokeCertificate.mockResolvedValue(operation);
  mocks.getOperation.mockResolvedValue(operation);
  mocks.getUserPolicy.mockResolvedValue(undefined);
  mocks.setUserPolicy.mockResolvedValue(undefined);
});
afterEach(async () => {
  unmount?.();
  // Grants intentionally survive an SPA unmount, but expire in memory.
  await vi.advanceTimersByTimeAsync(10 * 60_000);
  expect(vi.getTimerCount()).toBe(0);
  vi.useRealTimers();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

async function changeContext(view: View, kind: string) {
  if (kind === "node") {
    mocks.route.params.nodeId = "node-b";
    mocks.fleet.selected = {
      id: "node-b",
      name: "Node B",
      version: 31,
      configRevision: 7,
    };
  } else if (kind === "unmount") {
    unmount?.();
  } else {
    view.configuration.configDialog = false;
    view.certificates.certificateDialog = false;
  }
  await nextTick();
  await nextTick();
}

async function startPollingWorkflow(
  view: View,
  kind: "config" | "certificate",
): Promise<{ pending: Promise<void> }> {
  if (kind === "config") {
    await view.configuration.openConfigPlan();
    mocks.createConfigPlan.mockResolvedValueOnce({ ...plan, state: "queued" });
    view.configuration.configReason = "plan";
    return { pending: view.configuration.submitConfigPlan() };
  }
  await view.certificates.openCertificate();
  mocks.createCertificate.mockResolvedValueOnce({
    ...certificate,
    state: "csr_pending",
  });
  view.certificates.certificateReason = "CSR";
  return { pending: view.certificates.submitCertificateRequest() };
}

describe("node workflow context isolation", () => {
  it("binds the displayed operation and action lock to the route node", async () => {
    const view = await mount();
    mocks.fleet.operationTracking = true;
    mocks.fleet.operationNodeId = "node-a";
    mocks.fleet.latestOperation = { ...operation, state: "offline_pending" };
    expect(view.page.currentOperation?.id).toBe(operation.id);
    expect(view.page.operationBusy).toBe(true);

    await changeContext(view, "node");
    expect(view.page.currentOperation).toBeUndefined();
    expect(view.page.operationBusy).toBe(false);
  });

  it.each([
    ["config", "node"],
    ["config", "close"],
    ["config", "unmount"],
    ["certificate", "node"],
    ["certificate", "close"],
    ["certificate", "unmount"],
  ] as const)(
    "stops %s polling on %s without another mutation",
    async (workflow, kind) => {
      const view = await mount();
      const { pending } = await startPollingWorkflow(view, workflow);
      await nextTick();
      expect(vi.getTimerCount()).toBe(1);
      await changeContext(view, kind);
      expect(vi.getTimerCount()).toBe(0);
      await vi.advanceTimersByTimeAsync(500);
      await pending;
      const isConfig = workflow === "config";
      expect(
        isConfig ? mocks.getConfigPlan : mocks.getCertificate,
      ).not.toHaveBeenCalled();
      expect(
        isConfig ? mocks.createConfigPlan : mocks.createCertificate,
      ).toHaveBeenCalledTimes(1);
      expect(
        isConfig
          ? view.configuration.configPlan
          : view.certificates.certificate,
      ).toBeUndefined();
    },
  );

  it.each(["success", "error"])(
    "ignores late certificate %s and finally after reopen",
    async (result) => {
      const view = await mount();
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

  it.each(["success", "error"])(
    "ignores late plan %s and finally in another node",
    async (result) => {
      const view = await mount();
      await view.configuration.openConfigPlan();
      const old = Promise.withResolvers<ConfigPlan>();
      const fresh = Promise.withResolvers<ConfigPlan>();
      mocks.createConfigPlan
        .mockReturnValueOnce(old.promise)
        .mockReturnValueOnce(fresh.promise);
      view.configuration.configReason = "old";
      const first = view.configuration.submitConfigPlan();
      await changeContext(view, "node");
      await view.configuration.openConfigPlan();
      view.configuration.configReason = "new";
      const second = view.configuration.submitConfigPlan();
      if (result === "success") old.resolve(plan);
      else old.reject(new Error("old failure"));
      await first;
      expect(view.configuration.configPlan).toBeUndefined();
      expect(view.configuration.configError).toBe("");
      expect(view.configuration.configLoading).toBe(true);
      fresh.resolve({ ...plan, id: "plan-b" });
      await second;
      expect(view.configuration.configPlan?.id).toBe("plan-b");
    },
  );

  it("does not apply a retained plan after a node switch", async () => {
    const view = await mount();
    await view.configuration.openConfigPlan();
    view.configuration.configReason = "plan";
    await view.configuration.submitConfigPlan();
    await changeContext(view, "node");
    view.configuration.configApplyReason = "apply";
    await view.configuration.submitConfigApply();
    expect(view.configuration.configDialog).toBe(false);
    expect(mocks.applyConfigPlan).not.toHaveBeenCalled();
  });

  it.each([
    ["config", "success"],
    ["config", "error"],
    ["certificate", "success"],
    ["certificate", "error"],
  ] as const)(
    "fences an in-flight %s polling %s",
    async (workflow, outcome) => {
      const view = await mount();
      const config = workflow === "config";
      const old = Promise.withResolvers<unknown>();
      const read = config ? mocks.getConfigPlan : mocks.getCertificate;
      read.mockReturnValueOnce(old.promise);
      const { pending } = await startPollingWorkflow(view, workflow);
      await vi.advanceTimersByTimeAsync(500);
      const signal = read.mock.calls[0]?.[1] as AbortSignal;
      await changeContext(view, "close");
      expect(signal.aborted).toBe(true);
      if (config) await view.configuration.openConfigPlan();
      else {
        mocks.listNodeCertificates.mockResolvedValueOnce([certificate]);
        await view.certificates.openCertificate();
      }
      if (outcome === "success")
        old.resolve({ ...(config ? plan : certificate), id: "late" });
      else old.reject(new Error("late read"));
      await pending;
      expect(
        config
          ? view.configuration.configPlan?.id
          : view.certificates.certificate?.id,
      ).toBe(config ? plan.id : certificate.id);
      expect(
        config
          ? view.configuration.configError
          : view.certificates.certificateError,
      ).toBe("");
    },
  );

  it("invalidates a same-tick A to B to A navigation", async () => {
    const view = await mount();
    await view.configuration.openConfigPlan();
    const old = Promise.withResolvers<ConfigPlan>();
    mocks.createConfigPlan.mockReturnValueOnce(old.promise);
    view.configuration.configReason = "plan";
    const pending = view.configuration.submitConfigPlan();
    mocks.route.params.nodeId = "node-b";
    mocks.route.params.nodeId = "node-a";
    await nextTick();
    await nextTick();
    const reopening = view.configuration.openConfigPlan();
    old.resolve(plan);
    await pending;
    await reopening;
    expect(mocks.getConfigPlan).toHaveBeenCalledWith(
      plan.id,
      expect.any(AbortSignal),
    );
    expect(view.configuration.configPlan?.id).toBe(plan.id);
  });

  it("never restores another Workspace's reference", async () => {
    const view = await mount();
    await view.configuration.openConfigPlan();
    view.configuration.configReason = "plan";
    await view.configuration.submitConfigPlan();
    mocks.cachedWorkspaceId.mockReturnValue("workspace-b");
    await nextTick();
    await view.configuration.openConfigPlan();
    expect(mocks.getConfigPlan).not.toHaveBeenCalled();
    expect(view.configuration.configPlan).toBeUndefined();
  });

  it("retains a late accepted plan across unmount and reads it without resubmitting", async () => {
    let view = await mount();
    await view.configuration.openConfigPlan();
    const accepted = Promise.withResolvers<ConfigPlan>();
    mocks.createConfigPlan.mockReturnValueOnce(accepted.promise);
    view.configuration.configReason = "plan";
    const pending = view.configuration.submitConfigPlan();
    unmount?.();
    accepted.resolve(plan);
    await pending;
    view = await mount();
    await view.configuration.openConfigPlan();
    expect(mocks.getConfigPlan).toHaveBeenCalledWith(
      plan.id,
      expect.any(AbortSignal),
    );
    expect(view.configuration.configPlan?.id).toBe(plan.id);
    expect(mocks.createConfigPlan).toHaveBeenCalledTimes(1);
  });

  it("retains a late accepted Apply ID without tracking it in the new node", async () => {
    const view = await mount();
    await view.configuration.openConfigPlan();
    view.configuration.configReason = "plan";
    await view.configuration.submitConfigPlan();
    const accepted = Promise.withResolvers<Operation>();
    mocks.applyConfigPlan.mockReturnValueOnce(accepted.promise);
    view.configuration.configApplyReason = "apply";
    const pending = view.configuration.submitConfigApply();
    await changeContext(view, "node");
    await view.configuration.openConfigPlan();
    accepted.resolve(operation);
    await pending;
    expect(view.configuration.configDialog).toBe(true);
    expect(mocks.fleet.trackOperation).not.toHaveBeenCalled();
    mocks.route.params.nodeId = "node-a";
    mocks.fleet.selected = { id: "node-a", configRevision: 7 };
    await nextTick();
    await nextTick();
    await view.configuration.openConfigPlan();
    expect(mocks.getOperation).toHaveBeenCalledWith(
      operation.id,
      expect.any(AbortSignal),
    );
    expect(view.configuration.configOperation?.id).toBe(operation.id);
    expect(mocks.applyConfigPlan).toHaveBeenCalledTimes(1);
  });

  it.each([
    "submitCertificateIssue",
    "createP12",
    "revokeCurrentCertificate",
  ] as const)(
    "isolates late %s completion after a Workspace switch",
    async (method) => {
      const view = await mount();
      mocks.listNodeCertificates.mockResolvedValueOnce([certificate]);
      await view.certificates.openCertificate();
      const accepted = Promise.withResolvers<unknown>();
      const api = certApi(method);
      api.mockReturnValueOnce(accepted.promise);
      view.certificates.certificateReason = "request";
      const pending = view.certificates[method]();
      await changeContext(view, "workspace");
      mocks.cachedWorkspaceId.mockReturnValue("workspace-b");
      await view.certificates.openCertificate();
      accepted.resolve(
        method === "submitCertificateIssue"
          ? certificate
          : method === "createP12"
            ? p12Grant({ downloadToken: "secret", password: "secret" })
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
      const view = await mount();
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
      await changeContext(view, kind);
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
      expect(view.certificates.certificateGrant?.downloadToken).toBeUndefined();
      if (kind === "close" || kind === "workspace")
        expect(view.certificates.certificateGrant?.password).toBe(
          "one-time-secret",
        );
      expect(view.certificates.certificateError).toBe("");
    },
  );

  it("recovers an accepted P12 grant after unmount without persisting its credentials or resending", async () => {
    let view = await mount();
    mocks.listNodeCertificates.mockResolvedValue([certificate]);
    await view.certificates.openCertificate();
    view.certificates.certificateReason = "export";
    const saved = vi.spyOn(sessionStorage, "setItem");
    const accepted = Promise.withResolvers<unknown>();
    mocks.createCertificateP12.mockReturnValueOnce(accepted.promise);
    const pending = view.certificates.createP12();
    unmount?.();
    accepted.resolve(
      p12Grant({
        password: "private-password",
        downloadToken: "private-token",
      }),
    );
    await pending;
    expect(JSON.stringify(saved.mock.calls)).not.toContain("private-");
    expect(JSON.stringify(saved.mock.calls)).toContain(operation.id);
    view = await mount();
    await view.certificates.openCertificate();
    expect(view.certificates.certificateOperation?.id).toBe(operation.id);
    expect(view.certificates.certificateGrant).toMatchObject({
      password: "private-password",
      downloadToken: "private-token",
    });
    expect(mocks.createCertificateP12).toHaveBeenCalledTimes(1);
    expect(mocks.fleet.trackOperation).not.toHaveBeenCalled();
  });

  it.each(["reopen", "remount"])(
    "blocks a second mutation while acknowledgement is pending across %s",
    async (kind) => {
      let view = await mount();
      await view.configuration.openConfigPlan();
      const accepted = Promise.withResolvers<ConfigPlan>();
      mocks.createConfigPlan.mockReturnValueOnce(accepted.promise);
      view.configuration.configReason = "first";
      const first = view.configuration.submitConfigPlan();
      view.configuration.configDialog = false;
      if (kind === "remount") {
        unmount?.();
        view = await mount();
      }
      const reopening = view.configuration.openConfigPlan();
      await nextTick();
      view.configuration.configReason = "second";
      await view.configuration.submitConfigPlan();
      const callsBeforeAcknowledgement =
        mocks.createConfigPlan.mock.calls.length;
      accepted.resolve(plan);
      await first;
      await reopening;
      expect(callsBeforeAcknowledgement).toBe(1);
      expect(mocks.createConfigPlan).toHaveBeenCalledTimes(1);
    },
  );

  it.each(["config", "certificate"] as const)(
    "does not let an obsolete %s ticket replace the newer reference",
    (kind) => {
      const context: NodeWorkflowContext = {
        nodeId: "node-a",
        workspaceId: "workspace-a",
        signal: new AbortController().signal,
      };
      const first = beginNodeMutation(context, kind);
      if (!first) throw new Error("First mutation did not acquire its ticket");
      expect(beginNodeMutation(context, kind)).toBeUndefined();
      finishNodeMutation(first);
      const second = beginNodeMutation(context, kind);
      if (!second)
        throw new Error("Second mutation did not acquire its ticket");
      finishNodeMutation(first);
      expect(beginNodeMutation(context, kind)).toBeUndefined();
      rememberNodeWorkflowReference(second, {
        resourceId: "new",
        operationId: "new-operation",
      });
      rememberNodeWorkflowReference(first, {
        resourceId: "old",
        operationId: "old-operation",
      });
      expect(readNodeWorkflowReference(context, kind)).toEqual({
        resourceId: "new",
        operationId: "new-operation",
      });
    },
  );

  it.each([
    "submitCertificateRequest",
    "submitCertificateIssue",
    "createP12",
    "revokeCurrentCertificate",
  ] as const)(
    "waits for pending %s across remount without resending",
    async (method) => {
      let view = await mount();
      mocks.listNodeCertificates.mockResolvedValue([certificate]);
      await view.certificates.openCertificate();
      const api = certApi(method);
      const accepted = Promise.withResolvers<unknown>();
      api.mockReturnValueOnce(accepted.promise);
      view.certificates.certificateReason = "first";
      const first = view.certificates[method]();
      unmount?.();
      view = await mount();
      const reopening = view.certificates.openCertificate();
      await nextTick();
      view.certificates.certificateReason = "second";
      await view.certificates[method]();
      const calls = api.mock.calls.length;
      accepted.resolve(
        method === "createP12"
          ? p12Grant({
              downloadToken: "private-token",
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
    const view = await mount();
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
    expect(view.certificates.certificateGrant?.downloadToken).toBeUndefined();
    expect(response.password).toBeUndefined();
    expect(response.downloadToken).toBeUndefined();
    expect(vi.getTimerCount()).toBe(0);
  });

  it.each(["config", "certificate"])(
    "rejects a restored %s operation from another node",
    async (kind) => {
      const view = await mount();
      if (kind === "config") {
        await view.configuration.openConfigPlan();
        view.configuration.configReason = "plan";
        await view.configuration.submitConfigPlan();
        view.configuration.configApplyReason = "apply";
        await view.configuration.submitConfigApply();
      } else {
        mocks.listNodeCertificates.mockResolvedValue([certificate]);
        await view.certificates.openCertificate();
        view.certificates.certificateReason = "export";
        await view.certificates.createP12();
        view.certificates.certificateDialog = false;
      }
      mocks.getOperation.mockResolvedValueOnce({
        ...operation,
        nodeId: "node-b",
      });
      if (kind === "config") await view.configuration.openConfigPlan();
      else await view.certificates.openCertificate();
      expect(
        kind === "config"
          ? view.configuration.configOperation
          : view.certificates.certificateOperation,
      ).toBeUndefined();
    },
  );

  it("opens the policy dialog only for the current node and closes it on context change", async () => {
    const view = await mount();
    view.page.openPolicy("alice");
    expect(view.page.policyDialog).toEqual({ username: "alice" });
    await changeContext(view, "node");
    expect(view.page.policyDialog).toBeUndefined();
    mocks.fleet.selected = undefined;
    view.page.openPolicy("bob");
    expect(view.page.policyDialog).toBeUndefined();
  });
});

it.each([
  ["notFound", "not-found"],
  ["unavailable", "unavailable"],
])(
  "uses the node response to classify %s rather than a cached list",
  async (error, expected) => {
    const view = await mount();
    mocks.fleet.nodes = [{ id: "some-other-node" }];
    mocks.fleet.selected = undefined;
    mocks.fleet.selectionError = error;
    await view.page.selectRouteNode();
    expect(view.page.detailLoading).toBe(false);
    expect(view.page.detailState).toBe(expected);
  },
);

describe("transient password workflow", () => {
  it("clears the input before a failed key read and requires re-entry", async () => {
    const view = await mount();
    const pending = Promise.withResolvers<never>();
    mocks.getUserPasswordSealingKey.mockReturnValue(pending.promise);
    view.userGroup.openDesired("create");
    view.userGroup.desiredPassword = "temporary password fixture";
    view.userGroup.desiredName = "alice";
    view.userGroup.desiredReason = "create fixture";
    const submit = view.userGroup.submitDesired();
    expect(view.userGroup.desiredPassword).toBe("");
    expect(view.userGroup.desiredLoading).toBe(true);
    pending.reject(new Error("unavailable"));
    await submit;
    expect(view.userGroup.desiredError).toBe("passwordKeyUnavailable");
    expect(view.userGroup.desiredLoading).toBe(false);
    expect(view.userGroup.desiredDialog?.kind).toBe("create");
    expect(view.userGroup.desiredPassword).toBe("");
  });
  it.each(["node", "close", "unmount"])(
    "discards a late key after %s and clears inputs",
    async (change) => {
      const view = await mount();
      const pending = Promise.withResolvers<never>();
      mocks.getUserPasswordSealingKey.mockReturnValue(pending.promise);
      const create = vi.fn();
      mocks.fleet.createUser = create;
      view.userGroup.openDesired("create");
      view.userGroup.desiredPassword = "temporary password fixture";
      view.userGroup.desiredReason = "create fixture";
      const submit = view.userGroup.submitDesired();
      const signal = mocks.getUserPasswordSealingKey.mock
        .calls[0]?.[1] as AbortSignal;
      if (change === "node") {
        mocks.route.params.nodeId = "node-b";
        await nextTick();
      } else if (change === "close") view.userGroup.desiredDialog = undefined;
      else unmount?.();
      expect(signal.aborted).toBe(true);
      pending.resolve({} as never);
      await submit;
      expect(view.userGroup.desiredPassword).toBe("");
      expect(create).not.toHaveBeenCalled();
    },
  );
});
