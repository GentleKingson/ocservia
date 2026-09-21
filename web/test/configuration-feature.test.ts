import { effectScope, nextTick, ref, type EffectScope } from "vue";
import type { ConfigPlan, NodeObservedState } from "@ocservia/api-client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { useNodeConfiguration } from "../src/features/configuration/useNodeConfiguration";

const api = vi.hoisted(() => ({
  createConfigPlan: vi.fn(),
  getConfigPlan: vi.fn(),
  applyConfigPlan: vi.fn(),
}));
vi.mock("../src/api/configuration", () => api);
vi.mock("../src/api/operations", () => ({ getOperation: vi.fn() }));

const scopes: EffectScope[] = [];
const plan = {
  id: "plan-a",
  nodeId: "node-a",
  workspaceId: "workspace-a",
  operationId: "plan-operation",
  state: "succeeded",
  validation: "valid",
} as ConfigPlan;

function setup() {
  const currentNode = ref({
    id: "node-a",
    configRevision: 7,
  } as NodeObservedState);
  const readReady = ref(true);
  const workspace = ref({ id: "workspace-a", generation: 1 });
  const trackOperation = vi.fn().mockResolvedValue(undefined);
  const scope = effectScope();
  scopes.push(scope);
  const feature = scope.run(() =>
    useNodeConfiguration({
      currentNode,
      readReady,
      workspaceContext: () => workspace.value,
      trackOperation,
      t: (key) => key,
    }),
  );
  if (!feature) throw new Error("Configuration scope did not start");
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
  api.createConfigPlan.mockResolvedValue(plan);
  api.applyConfigPlan.mockResolvedValue({ id: "apply-operation" });
});
afterEach(() => {
  scopes.splice(0).forEach((scope) => {
    scope.stop();
  });
  expect(vi.getTimerCount()).toBe(0);
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe("configuration feature without a page or store", () => {
  it("keeps its form local and delegates accepted Apply tracking after closing", async () => {
    const { feature, trackOperation } = setup();
    const other = setup().feature;
    await feature.openConfigPlan();
    feature.configReason.value = " plan ";
    feature.configPort.value = 444;
    await feature.submitConfigPlan();
    const request = api.createConfigPlan.mock.calls[0]?.[1] as Parameters<
      typeof import("../src/api/configuration").createConfigPlan
    >[1];
    expect(request.template.directives).toContainEqual({
      name: "tcp-port",
      value: "444",
    });
    for (const [name, value] of [
      ["udp-port", "0"],
      ["device", "vpns"],
      ["dns", "1.1.1.1"],
      ["ipv4-network", "10.42.0.0/24"],
      ["cookie-timeout", "300"],
      ["max-same-clients", "2"],
    ])
      expect(request.template.directives).toContainEqual({ name, value });
    expect(
      new Set(request.template.directives.map((item) => item.name)).size,
    ).toBe(13);
    expect(other.configDialog.value).toBe(false);
    expect(other.configPort.value).toBe(443);
    expect(api.createConfigPlan).toHaveBeenCalledExactlyOnceWith(
      "node-a",
      expect.objectContaining({
        expectedRevision: 7,
        reason: "plan",
        ttlSeconds: 900,
      }),
    );
    feature.configApplyApproval.value = " approval ";
    feature.configApplyReason.value = " apply ";
    trackOperation.mockImplementation(() => {
      expect(feature.configDialog.value).toBe(false);
      return Promise.resolve();
    });
    await feature.submitConfigApply();
    expect(api.applyConfigPlan).toHaveBeenCalledExactlyOnceWith("plan-a", {
      approvalId: "approval",
      reason: "apply",
    });
    expect(trackOperation).toHaveBeenCalledExactlyOnceWith("apply-operation");
  });

  it.each(["read", "node", "workspace"])(
    "uses the supplied %s boundary rather than global state",
    async (kind) => {
      const { feature, currentNode, readReady, workspace } = setup();
      await feature.openConfigPlan();
      feature.configReason.value = "plan";
      if (kind === "read") readReady.value = false;
      else if (kind === "node")
        currentNode.value = { ...currentNode.value, id: "node-b" };
      else workspace.value = { id: "workspace-a", generation: 3 };
      await feature.submitConfigPlan();
      expect(api.createConfigPlan).not.toHaveBeenCalled();
    },
  );

  it("disposes its polling scope without losing the accepted receipt", async () => {
    const { feature, scope } = setup();
    await feature.openConfigPlan();
    api.createConfigPlan.mockResolvedValueOnce({ ...plan, state: "queued" });
    feature.configReason.value = "plan";
    const pending = feature.submitConfigPlan();
    await nextTick();
    expect(vi.getTimerCount()).toBe(1);
    scope.stop();
    await pending;
    expect(feature.configDialog.value).toBe(false);
    expect(feature.configPlan.value).toBeUndefined();
    expect(api.getConfigPlan).not.toHaveBeenCalled();
    api.getConfigPlan.mockResolvedValue(plan);
    const reopened = setup().feature;
    await reopened.openConfigPlan();
    expect(reopened.configPlan.value?.id).toBe(plan.id);
    expect(api.getConfigPlan).toHaveBeenCalledExactlyOnceWith(
      plan.id,
      expect.any(AbortSignal),
    );
    expect(api.createConfigPlan).toHaveBeenCalledTimes(1);
  });
});
