import { createRenderer, nextTick, reactive, ssrContextKey } from "vue";
import type { Certificate, ConfigPlan, Operation } from "@ocservia/api-client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const mocks = vi.hoisted(() => {
  const fleet: Record<string, unknown> = {};
  return {
    fleet,
    route: { params: { nodeId: "node-a" } },
    workspaceContext: vi.fn(),
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
    getUserPolicy: vi.fn(),
    setUserPolicy: vi.fn(),
  };
});
vi.mock("../src/shared/fleet", () => ({ useFleetStore: () => mocks.fleet }));
vi.mock("vue-router", () => ({ useRoute: () => mocks.route }));
vi.mock("vue-i18n", () => ({ useI18n: () => ({ t: (key: string) => key }) }));
vi.mock("../src/api/client", async (original) => ({
  ...(await original<typeof import("../src/api/client")>()),
  ...mocks,
}));

import NodeDetailView from "../src/views/NodeDetailView.vue";
import { workspaceChangedEvent } from "../src/api/client";

const renderer = createRenderer<object, object>({
  createElement: () => ({}),
  createText: () => ({}),
  createComment: () => ({}),
  insert: () => {},
  remove: () => {},
  setText: () => {},
  setElementText: () => {},
  parentNode: () => null,
  nextSibling: () => null,
  patchProp: () => {},
});

interface View {
  configDialog: boolean;
  configLoading: boolean;
  configError: string;
  configPlan?: ConfigPlan;
  configOperation?: Operation;
  configReason: string;
  configApplyApproval: string;
  configApplyReason: string;
  certificateDialog: boolean;
  certificateLoading: boolean;
  certificateError: string;
  certificate?: Certificate;
  certificateOperation?: Operation;
  certificateGrant?: { downloadToken?: string; password?: string };
  certificateCommonName: string;
  certificateReason: string;
  certificateApproval: string;
  policyDialog: { username: string } | undefined;
  policyLoading: boolean;
  policyError: string;
  policyForm: { version: number };
  policyReason: string;
  openConfigPlan(): Promise<void>;
  submitConfigPlan(): Promise<void>;
  submitConfigApply(): Promise<void>;
  openCertificate(): Promise<void>;
  submitCertificateRequest(): Promise<void>;
  submitCertificateIssue(): Promise<void>;
  createP12(): Promise<void>;
  downloadP12(): Promise<void>;
  revokeCurrentCertificate(): Promise<void>;
  openPolicy(username: string): Promise<void>;
  submitPolicy(): Promise<void>;
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (error: Error) => void;
  const promise = new Promise<T>((yes, no) => {
    resolve = yes;
    reject = no;
  });
  return { promise, resolve, reject };
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
let unmount: (() => void) | undefined;

async function mount(): Promise<View> {
  mocks.fleet.selected = {
    id: "node-a",
    name: "Node A",
    version: 31,
    configRevision: 7,
  };
  const app = renderer.createApp({ ...NodeDetailView, render: () => null });
  app.provide(ssrContextKey, {});
  const instance = app.mount({});
  unmount = () => {
    app.unmount();
    unmount = undefined;
  };
  await nextTick();
  await nextTick();
  return (instance.$ as unknown as { setupState: View }).setupState;
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
    initialized: true,
    selecting: false,
    selectionError: "",
    userGroupState: [],
    select: vi.fn().mockResolvedValue(undefined),
    connect: vi.fn().mockResolvedValue(undefined),
    trackOperation: vi.fn().mockResolvedValue(undefined),
  });
  mocks.workspaceContext.mockReturnValue({ id: "workspace-a", generation: 1 });
  mocks.createConfigPlan.mockResolvedValue(plan);
  mocks.getConfigPlan.mockResolvedValue(plan);
  mocks.applyConfigPlan.mockResolvedValue(operation);
  mocks.listNodeCertificates.mockResolvedValue([]);
  mocks.createCertificate.mockResolvedValue(certificate);
  mocks.getCertificate.mockResolvedValue(certificate);
  mocks.issueCertificate.mockResolvedValue({ ...certificate, state: "issued" });
  mocks.createCertificateP12.mockResolvedValue({
    operation,
    downloadToken: "one-time-token",
    password: "one-time-secret",
  });
  mocks.revokeCertificate.mockResolvedValue(operation);
  mocks.getOperation.mockResolvedValue(operation);
  mocks.getUserPolicy.mockResolvedValue(undefined);
  mocks.setUserPolicy.mockResolvedValue(undefined);
});
afterEach(() => {
  unmount?.();
  expect(vi.getTimerCount()).toBe(0);
  vi.useRealTimers();
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
  } else if (kind === "workspace") {
    mocks.workspaceContext.mockReturnValue({
      id: "workspace-a",
      generation: 3,
    });
    window.dispatchEvent(new Event(workspaceChangedEvent));
  } else if (kind === "unmount") {
    unmount?.();
  } else {
    view.configDialog = false;
    view.certificateDialog = false;
  }
  await nextTick();
  await nextTick();
}

describe("node workflow context isolation", () => {
  it.each(["node", "workspace", "close", "unmount"])(
    "stops configuration polling on %s without another mutation",
    async (kind) => {
      const view = await mount();
      await view.openConfigPlan();
      mocks.createConfigPlan.mockResolvedValueOnce({
        ...plan,
        state: "queued",
      });
      view.configReason = "plan";
      const pending = view.submitConfigPlan();
      await nextTick();
      expect(vi.getTimerCount()).toBe(1);
      await changeContext(view, kind);
      expect(vi.getTimerCount()).toBe(0);
      await vi.advanceTimersByTimeAsync(500);
      await pending;
      expect(mocks.getConfigPlan).not.toHaveBeenCalled();
      expect(mocks.createConfigPlan).toHaveBeenCalledTimes(1);
      expect(view.configPlan).toBeUndefined();
    },
  );

  it.each(["node", "workspace", "close", "unmount"])(
    "stops CSR polling on %s without another mutation",
    async (kind) => {
      const view = await mount();
      await view.openCertificate();
      mocks.createCertificate.mockResolvedValueOnce({
        ...certificate,
        state: "csr_pending",
      });
      view.certificateReason = "CSR";
      const pending = view.submitCertificateRequest();
      await nextTick();
      await changeContext(view, kind);
      expect(vi.getTimerCount()).toBe(0);
      await vi.advanceTimersByTimeAsync(500);
      await pending;
      expect(mocks.getCertificate).not.toHaveBeenCalled();
      expect(mocks.createCertificate).toHaveBeenCalledTimes(1);
      expect(view.certificate).toBeUndefined();
    },
  );

  it.each(["success", "error"])(
    "ignores late certificate %s and finally after reopen",
    async (result) => {
      const view = await mount();
      const old = deferred<Certificate[]>();
      const fresh = deferred<Certificate[]>();
      mocks.listNodeCertificates
        .mockReturnValueOnce(old.promise)
        .mockReturnValueOnce(fresh.promise);
      const first = view.openCertificate();
      const signal = mocks.listNodeCertificates.mock
        .calls[0]?.[1] as AbortSignal;
      view.certificateDialog = false;
      await nextTick();
      const second = view.openCertificate();
      if (result === "success") old.resolve([certificate]);
      else old.reject(new Error("old failure"));
      await first;
      expect(signal.aborted).toBe(true);
      expect(view.certificate).toBeUndefined();
      expect(view.certificateError).toBe("");
      expect(view.certificateLoading).toBe(true);
      fresh.resolve([]);
      await second;
      expect(view.certificateLoading).toBe(false);
    },
  );

  it.each(["success", "error"])(
    "ignores late plan %s and finally after reopen",
    async (result) => {
      const view = await mount();
      await view.openConfigPlan();
      const old = deferred<ConfigPlan>();
      const fresh = deferred<ConfigPlan>();
      mocks.createConfigPlan
        .mockReturnValueOnce(old.promise)
        .mockReturnValueOnce(fresh.promise);
      view.configReason = "old";
      const first = view.submitConfigPlan();
      view.configDialog = false;
      await nextTick();
      await view.openConfigPlan();
      view.configReason = "new";
      const second = view.submitConfigPlan();
      if (result === "success") old.resolve(plan);
      else old.reject(new Error("old failure"));
      await first;
      expect(view.configPlan).toBeUndefined();
      expect(view.configError).toBe("");
      expect(view.configLoading).toBe(true);
      fresh.resolve({ ...plan, id: "plan-b" });
      await second;
      expect(view.configPlan?.id).toBe("plan-b");
    },
  );

  it("does not apply a retained plan after a node switch", async () => {
    const view = await mount();
    await view.openConfigPlan();
    view.configReason = "plan";
    await view.submitConfigPlan();
    await changeContext(view, "node");
    view.configApplyApproval = "approval";
    view.configApplyReason = "apply";
    await view.submitConfigApply();
    expect(view.configDialog).toBe(false);
    expect(mocks.applyConfigPlan).not.toHaveBeenCalled();
  });

  it.each([
    "config success",
    "config error",
    "certificate success",
    "certificate error",
  ])("fences an in-flight polling response: %s", async (scenario) => {
    const view = await mount();
    const config = scenario.startsWith("config");
    const old = deferred<unknown>();
    const read = config ? mocks.getConfigPlan : mocks.getCertificate;
    read.mockReturnValueOnce(old.promise);
    let pending: Promise<void>;
    if (config) {
      await view.openConfigPlan();
      mocks.createConfigPlan.mockResolvedValueOnce({
        ...plan,
        state: "queued",
      });
      view.configReason = "plan";
      pending = view.submitConfigPlan();
    } else {
      await view.openCertificate();
      mocks.createCertificate.mockResolvedValueOnce({
        ...certificate,
        state: "csr_pending",
      });
      view.certificateReason = "CSR";
      pending = view.submitCertificateRequest();
    }
    await vi.advanceTimersByTimeAsync(500);
    const signal = read.mock.calls[0]?.[1] as AbortSignal;
    await changeContext(view, "close");
    expect(signal.aborted).toBe(true);
    if (config) await view.openConfigPlan();
    else {
      mocks.listNodeCertificates.mockResolvedValueOnce([certificate]);
      await view.openCertificate();
    }
    if (scenario.endsWith("success"))
      old.resolve({ ...(config ? plan : certificate), id: "late" });
    else old.reject(new Error("late read"));
    await pending;
    expect(config ? view.configPlan?.id : view.certificate?.id).toBe(
      config ? plan.id : certificate.id,
    );
    expect(config ? view.configError : view.certificateError).toBe("");
  });

  it("invalidates a same-tick A to B to A navigation and removes its workspace listener", async () => {
    const remove = vi.spyOn(window, "removeEventListener");
    const view = await mount();
    await view.openConfigPlan();
    const old = deferred<ConfigPlan>();
    mocks.createConfigPlan.mockReturnValueOnce(old.promise);
    view.configReason = "plan";
    const pending = view.submitConfigPlan();
    mocks.route.params.nodeId = "node-b";
    mocks.route.params.nodeId = "node-a";
    await nextTick();
    await nextTick();
    await view.openConfigPlan();
    old.resolve(plan);
    await pending;
    expect(view.configPlan).toBeUndefined();
    unmount?.();
    expect(remove).toHaveBeenCalledWith(
      workspaceChangedEvent,
      expect.any(Function),
    );
    const calls = (mocks.fleet.select as ReturnType<typeof vi.fn>).mock.calls
      .length;
    window.dispatchEvent(new Event(workspaceChangedEvent));
    await nextTick();
    expect(mocks.fleet.select).toHaveBeenCalledTimes(calls);
  });

  it("never restores another Workspace's receipt", async () => {
    const view = await mount();
    await view.openConfigPlan();
    view.configReason = "plan";
    await view.submitConfigPlan();
    mocks.workspaceContext.mockReturnValue({
      id: "workspace-b",
      generation: 2,
    });
    window.dispatchEvent(new Event(workspaceChangedEvent));
    await nextTick();
    await nextTick();
    await view.openConfigPlan();
    expect(mocks.getConfigPlan).not.toHaveBeenCalled();
    expect(view.configPlan).toBeUndefined();
  });

  it("retains a late accepted plan across unmount and reads it without resubmitting", async () => {
    let view = await mount();
    await view.openConfigPlan();
    const accepted = deferred<ConfigPlan>();
    mocks.createConfigPlan.mockReturnValueOnce(accepted.promise);
    view.configReason = "plan";
    const pending = view.submitConfigPlan();
    unmount?.();
    accepted.resolve(plan);
    await pending;
    view = await mount();
    await view.openConfigPlan();
    expect(mocks.getConfigPlan).toHaveBeenCalledWith(
      plan.id,
      expect.any(AbortSignal),
    );
    expect(view.configPlan?.id).toBe(plan.id);
    expect(mocks.createConfigPlan).toHaveBeenCalledTimes(1);
  });

  it("retains a late accepted Apply ID without tracking it in the new node", async () => {
    const view = await mount();
    await view.openConfigPlan();
    view.configReason = "plan";
    await view.submitConfigPlan();
    const accepted = deferred<Operation>();
    mocks.applyConfigPlan.mockReturnValueOnce(accepted.promise);
    view.configApplyApproval = "approval";
    view.configApplyReason = "apply";
    const pending = view.submitConfigApply();
    await changeContext(view, "node");
    await view.openConfigPlan();
    accepted.resolve(operation);
    await pending;
    expect(view.configDialog).toBe(true);
    expect(mocks.fleet.trackOperation).not.toHaveBeenCalled();
    mocks.route.params.nodeId = "node-a";
    mocks.fleet.selected = { id: "node-a", configRevision: 7 };
    await nextTick();
    await nextTick();
    await view.openConfigPlan();
    expect(mocks.getOperation).toHaveBeenCalledWith(
      operation.id,
      expect.any(AbortSignal),
    );
    expect(view.configOperation?.id).toBe(operation.id);
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
      await view.openCertificate();
      const accepted = deferred<unknown>();
      const api =
        method === "submitCertificateIssue"
          ? mocks.issueCertificate
          : method === "createP12"
            ? mocks.createCertificateP12
            : mocks.revokeCertificate;
      api.mockReturnValueOnce(accepted.promise);
      view.certificateApproval = "approval";
      view.certificateReason = "request";
      const pending = view[method]();
      await changeContext(view, "workspace");
      await view.openCertificate();
      accepted.resolve(
        method === "submitCertificateIssue"
          ? certificate
          : method === "createP12"
            ? { operation, downloadToken: "secret", password: "secret" }
            : operation,
      );
      await pending;
      expect(view.certificate).toBeUndefined();
      expect(view.certificateGrant).toBeUndefined();
      expect(mocks.fleet.trackOperation).not.toHaveBeenCalled();
      expect(mocks.getCertificate).not.toHaveBeenCalled();
      expect(api).toHaveBeenCalledTimes(1);
    },
  );

  it("does not download a late artifact into another dialog", async () => {
    const view = await mount();
    mocks.listNodeCertificates.mockResolvedValueOnce([certificate]);
    await view.openCertificate();
    view.certificateApproval = "approval";
    view.certificateReason = "export";
    await view.createP12();
    const blob = deferred<Blob>();
    const createElement = vi.fn();
    vi.stubGlobal("document", { createElement });
    mocks.downloadCertificateArtifact.mockReturnValueOnce(blob.promise);
    const pending = view.downloadP12();
    view.certificateDialog = false;
    await nextTick();
    await view.openCertificate();
    blob.resolve(new Blob(["p12"]));
    await pending;
    expect(createElement).not.toHaveBeenCalled();
    expect(view.certificateGrant).toBeUndefined();
    expect(view.certificateError).toBe("");
  });

  it("recovers an accepted P12 operation after unmount without storing its credentials or resending", async () => {
    let view = await mount();
    mocks.listNodeCertificates.mockResolvedValue([certificate]);
    await view.openCertificate();
    view.certificateApproval = "approval";
    view.certificateReason = "export";
    const saved = vi.spyOn(sessionStorage, "setItem");
    const accepted = deferred<unknown>();
    mocks.createCertificateP12.mockReturnValueOnce(accepted.promise);
    const pending = view.createP12();
    unmount?.();
    accepted.resolve({
      operation,
      password: "private-password",
      downloadToken: "private-token",
    });
    await pending;
    expect(JSON.stringify(saved.mock.calls)).not.toContain("private-");
    expect(JSON.stringify(saved.mock.calls)).toContain(operation.id);
    view = await mount();
    await view.openCertificate();
    expect(view.certificateOperation?.id).toBe(operation.id);
    expect(view.certificateGrant).toBeUndefined();
    expect(mocks.createCertificateP12).toHaveBeenCalledTimes(1);
    expect(mocks.fleet.trackOperation).not.toHaveBeenCalled();
  });

  it("guards policy success, errors and finally by dialog generation", async () => {
    const view = await mount();
    const old = deferred<undefined>();
    const fresh = deferred<undefined>();
    mocks.getUserPolicy
      .mockReturnValueOnce(old.promise)
      .mockReturnValueOnce(fresh.promise);
    const first = view.openPolicy("alice");
    view.policyDialog = undefined;
    await nextTick();
    const second = view.openPolicy("alice");
    old.reject(new Error("old policy"));
    await first;
    expect(view.policyLoading).toBe(true);
    expect(view.policyError).toBe("");
    fresh.resolve(undefined);
    await second;
    const saved = deferred<undefined>();
    mocks.setUserPolicy.mockReturnValueOnce(saved.promise);
    view.policyReason = "save";
    const pending = view.submitPolicy();
    await view.openPolicy("bob");
    saved.resolve(undefined);
    await pending;
    expect(view.policyDialog).toEqual({ username: "bob" });
  });
});
