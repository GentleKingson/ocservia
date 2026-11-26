import { nextTick } from "vue";
import type { ConfigPlan, Operation } from "@/api/models";
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
  beginNodeMutation,
  finishNodeMutation,
  readNodeWorkflowReference,
  rememberNodeWorkflowReference,
  type NodeWorkflowContext,
} from "../src/features/node-workflow";
import {
  useNodeDetailHarness,
  workflowCertificate as certificate,
  workflowOperation as operation,
  workflowPlan as plan,
  type NodeDetailViewState as View,
} from "./node-workflow-harness";

const harness = useNodeDetailHarness(mocks);

beforeEach(harness.reset);
afterEach(harness.teardown);

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
    const view = await harness.mount();
    mocks.fleet.operationTracking = true;
    mocks.fleet.operationNodeId = "node-a";
    mocks.fleet.latestOperation = { ...operation, state: "offline_pending" };
    expect(view.page.currentOperation?.id).toBe(operation.id);
    expect(view.page.operationBusy).toBe(true);

    await harness.changeContext(view, "node");
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
      const view = await harness.mount();
      const { pending } = await startPollingWorkflow(view, workflow);
      await nextTick();
      expect(vi.getTimerCount()).toBe(1);
      await harness.changeContext(view, kind);
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
    "ignores late plan %s and finally in another node",
    async (result) => {
      const view = await harness.mount();
      await view.configuration.openConfigPlan();
      const old = Promise.withResolvers<ConfigPlan>();
      const fresh = Promise.withResolvers<ConfigPlan>();
      mocks.createConfigPlan
        .mockReturnValueOnce(old.promise)
        .mockReturnValueOnce(fresh.promise);
      view.configuration.configReason = "old";
      const first = view.configuration.submitConfigPlan();
      await harness.changeContext(view, "node");
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
    const view = await harness.mount();
    await view.configuration.openConfigPlan();
    view.configuration.configReason = "plan";
    await view.configuration.submitConfigPlan();
    await harness.changeContext(view, "node");
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
      const view = await harness.mount();
      const config = workflow === "config";
      const old = Promise.withResolvers<unknown>();
      const read = config ? mocks.getConfigPlan : mocks.getCertificate;
      read.mockReturnValueOnce(old.promise);
      const { pending } = await startPollingWorkflow(view, workflow);
      await vi.advanceTimersByTimeAsync(500);
      const signal = read.mock.calls[0]?.[1] as AbortSignal;
      await harness.changeContext(view, "close");
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
    const view = await harness.mount();
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
    const view = await harness.mount();
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
    let view = await harness.mount();
    await view.configuration.openConfigPlan();
    const accepted = Promise.withResolvers<ConfigPlan>();
    mocks.createConfigPlan.mockReturnValueOnce(accepted.promise);
    view.configuration.configReason = "plan";
    const pending = view.configuration.submitConfigPlan();
    harness.unmount();
    accepted.resolve(plan);
    await pending;
    view = await harness.mount();
    await view.configuration.openConfigPlan();
    expect(mocks.getConfigPlan).toHaveBeenCalledWith(
      plan.id,
      expect.any(AbortSignal),
    );
    expect(view.configuration.configPlan?.id).toBe(plan.id);
    expect(mocks.createConfigPlan).toHaveBeenCalledTimes(1);
  });

  it("retains a late accepted Apply ID without tracking it in the new node", async () => {
    const view = await harness.mount();
    await view.configuration.openConfigPlan();
    view.configuration.configReason = "plan";
    await view.configuration.submitConfigPlan();
    const accepted = Promise.withResolvers<Operation>();
    mocks.applyConfigPlan.mockReturnValueOnce(accepted.promise);
    view.configuration.configApplyReason = "apply";
    const pending = view.configuration.submitConfigApply();
    await harness.changeContext(view, "node");
    await view.configuration.openConfigPlan();
    accepted.resolve(operation);
    await pending;
    expect(view.configuration.configDialog).toBe(true);
    expect(mocks.fleet.trackOperation).not.toHaveBeenCalled();
    mocks.route.params.nodeId = "node-a";
    mocks.fleet.selected = { id: "node-a", config_revision: 7 };
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

  it.each(["reopen", "remount"])(
    "blocks a second mutation while acknowledgement is pending across %s",
    async (kind) => {
      let view = await harness.mount();
      await view.configuration.openConfigPlan();
      const accepted = Promise.withResolvers<ConfigPlan>();
      mocks.createConfigPlan.mockReturnValueOnce(accepted.promise);
      view.configuration.configReason = "first";
      const first = view.configuration.submitConfigPlan();
      view.configuration.configDialog = false;
      if (kind === "remount") {
        harness.unmount();
        view = await harness.mount();
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

  it.each(["config", "certificate"])(
    "rejects a restored %s operation from another node",
    async (kind) => {
      const view = await harness.mount();
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
        node_id: "node-b",
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
    const view = await harness.mount();
    view.page.openPolicy("alice");
    expect(view.page.policyDialog).toEqual({ username: "alice" });
    await harness.changeContext(view, "node");
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
    const view = await harness.mount();
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
    const view = await harness.mount();
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
      const view = await harness.mount();
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
      else harness.unmount();
      expect(signal.aborted).toBe(true);
      pending.resolve({} as never);
      await submit;
      expect(view.userGroup.desiredPassword).toBe("");
      expect(create).not.toHaveBeenCalled();
    },
  );
});
