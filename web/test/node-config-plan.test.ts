import { createRenderer, nextTick, reactive, ssrContextKey } from "vue";
import type { NodeObservedState } from "@ocservia/api-client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const mocks = vi.hoisted(() => {
  const fleet: Record<string, unknown> = {};
  return {
    createConfigPlan: vi.fn(),
    workspaceContext: vi.fn(),
    fleet,
    route: { params: { nodeId: "node-a" } },
  };
});

vi.mock("../src/shared/fleet", () => ({ useFleetStore: () => mocks.fleet }));
vi.mock("vue-router", () => ({ useRoute: () => mocks.route }));
vi.mock("vue-i18n", () => ({ useI18n: () => ({ t: (key: string) => key }) }));
vi.mock("../src/api/client", async (original) => ({
  ...(await original<typeof import("../src/api/client")>()),
  createConfigPlan: mocks.createConfigPlan,
  workspaceContext: mocks.workspaceContext,
}));

import NodeDetailView from "../src/views/NodeDetailView.vue";
import { workspaceChangedEvent } from "../src/api/client";

// Exercise the actual SFC setup without adding a DOM or another test framework.
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

interface ConfigView {
  configReason: string;
  configError: string;
  canSubmitConfigPlan: boolean;
  openConfigPlan(): void;
  submitConfigPlan(): Promise<void>;
}

let unmount: (() => void) | undefined;

async function mount(revision: unknown = 7): Promise<ConfigView> {
  mocks.fleet.selected = {
    id: "node-a",
    version: 31,
    configRevision: revision,
  };
  const app = renderer.createApp({ ...NodeDetailView, render: () => null });
  app.provide(ssrContextKey, {});
  const instance = app.mount({});
  unmount = () => {
    app.unmount();
  };
  await nextTick();
  await nextTick();
  return (instance.$ as unknown as { setupState: ConfigView }).setupState;
}

beforeEach(() => {
  vi.stubGlobal("window", new EventTarget());
  mocks.route = reactive({ params: { nodeId: "node-a" } });
  mocks.fleet = reactive({
    initialized: true,
    selecting: false,
    selectionError: "",
    userGroupState: [],
    select: vi.fn().mockResolvedValue(undefined),
    connect: vi.fn().mockResolvedValue(undefined),
  });
  mocks.workspaceContext.mockReturnValue({ id: "workspace-a", generation: 1 });
  mocks.createConfigPlan.mockResolvedValue({
    id: "plan",
    validation: "valid",
    state: "succeeded",
  });
});

afterEach(() => {
  unmount?.();
  vi.clearAllMocks();
  vi.unstubAllGlobals();
});

describe("node configuration revision", () => {
  it.each([0, 7, Number.MAX_SAFE_INTEGER])(
    "submits the read configuration revision %s, not node.Version",
    async (revision) => {
      const view = await mount(revision);
      view.openConfigPlan();
      expect(view.canSubmitConfigPlan).toBe(true);
      view.configReason = "review configuration";
      await view.submitConfigPlan();
      expect(mocks.createConfigPlan).toHaveBeenCalledExactlyOnceWith(
        "node-a",
        expect.objectContaining({ expectedRevision: revision }),
      );
    },
  );

  it.each([null, -1, 1.5, Number.MAX_SAFE_INTEGER + 1, "7", NaN])(
    "does not submit unknown or imprecise revision %s",
    async (revision) => {
      const view = await mount(revision);
      view.openConfigPlan();
      expect(view.canSubmitConfigPlan).toBe(false);
      view.configReason = "review configuration";
      await view.submitConfigPlan();
      expect(mocks.createConfigPlan).not.toHaveBeenCalled();
    },
  );

  it("does not treat an omitted field from an older server as zero", async () => {
    const view = await mount();
    delete (
      mocks.fleet.selected as NodeObservedState & { configRevision?: number }
    ).configRevision;
    view.openConfigPlan();
    view.configReason = "review configuration";
    await view.submitConfigPlan();
    expect(mocks.createConfigPlan).not.toHaveBeenCalled();
  });

  it("does not reuse a retained node after a failed read", async () => {
    const view = await mount();
    mocks.fleet.selectionError = "unavailable";
    view.openConfigPlan();
    view.configReason = "review configuration";
    await view.submitConfigPlan();
    expect(mocks.createConfigPlan).not.toHaveBeenCalled();
  });

  it("does not move a captured revision to another node", async () => {
    const view = await mount();
    view.openConfigPlan();
    view.configReason = "review configuration";
    mocks.route.params.nodeId = "node-b";
    mocks.fleet.selected = { id: "node-b", version: 31, configRevision: 0 };
    await nextTick();
    await view.submitConfigPlan();
    expect(mocks.createConfigPlan).not.toHaveBeenCalled();
  });

  it("invalidates the captured revision on Workspace changes, including A to B to A", async () => {
    const view = await mount();
    view.openConfigPlan();
    view.configReason = "review configuration";
    mocks.workspaceContext.mockReturnValue({
      id: "workspace-a",
      generation: 3,
    });
    window.dispatchEvent(new Event(workspaceChangedEvent));
    await nextTick();
    await nextTick();
    await view.submitConfigPlan();
    expect(mocks.createConfigPlan).not.toHaveBeenCalled();
  });

  it("preserves the captured revision and reports stale rejection without retry", async () => {
    const view = await mount(7);
    view.openConfigPlan();
    view.configReason = "review configuration";
    mocks.fleet.selected = { id: "node-a", version: 32, configRevision: 8 };
    mocks.createConfigPlan.mockRejectedValueOnce(new Error("stale revision"));
    await view.submitConfigPlan();
    expect(mocks.createConfigPlan).toHaveBeenCalledExactlyOnceWith(
      "node-a",
      expect.objectContaining({ expectedRevision: 7 }),
    );
    expect(view.configError).toBe("stale revision");
  });
});
