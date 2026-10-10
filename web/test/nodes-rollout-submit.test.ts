import { createRenderer, nextTick, ssrContextKey } from "vue";
import { ResponseError } from "@ocservia/api-client";
import { afterAll, afterEach, beforeEach, expect, it, vi } from "vitest";

const mocks = vi.hoisted(() => {
  vi.stubGlobal("window", new EventTarget());
  return {
    createAgentRollout: vi.fn(),
    workspaceContext: vi.fn(),
    push: vi.fn(),
    replace: vi.fn(),
  };
});
// @vueuse/core resolves defaultWindow at import time, without a DOM here.
vi.mock("@vueuse/core", async (original) => ({
  ...(await original<object>()),
  defaultWindow: globalThis.window,
}));
vi.mock("../src/api/agents", () => mocks);
vi.mock("../src/api/workspace", () => ({
  workspaceContext: mocks.workspaceContext,
  workspaceChangedEvent: "workspace",
}));
vi.mock("../src/shared/fleet", () => ({
  useFleetStore: () => ({
    start: () => {},
    stop: () => {},
    nodes: [
      {
        id: "node-a",
        agentUpgradeEligible: true,
        recommendedAgentVersion: "0.2.0",
      },
    ],
  }),
}));
vi.mock("vue-router", () => ({
  useRoute: () => ({ query: {} }),
  useRouter: () => ({ push: mocks.push, replace: mocks.replace }),
}));
vi.mock("vue-i18n", () => ({
  useI18n: () => ({ t: (key: string) => key, locale: { value: "en" } }),
}));
import NodesView from "../src/views/NodesView.vue";

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
  selected: string[];
  rolloutDialog: boolean;
  rolloutReason: string;
  rolloutApprovalId: string;
  rolloutStarting: boolean;
  rolloutError: string;
  submitRollout(): Promise<void>;
}

let workspace = { id: "workspace-a", generation: 1 };
let unmount: (() => void) | undefined;
async function flush() {
  for (let i = 0; i < 8; i++) await nextTick();
}
async function mount(): Promise<View> {
  const app = renderer.createApp({ ...NodesView, render: () => null });
  app.provide(ssrContextKey, {});
  const instance = app.mount({});
  unmount = () => {
    app.unmount();
    unmount = undefined;
  };
  await flush();
  const view = (instance.$ as unknown as { setupState: View }).setupState;
  view.selected = ["node-a"];
  view.rolloutDialog = true;
  view.rolloutReason = "first";
  view.rolloutApprovalId = "approval";
  return view;
}
function created() {
  let resolve!: (value: { id: string }) => void;
  mocks.createAgentRollout.mockReturnValue(
    new Promise((done) => {
      resolve = done;
    }),
  );
  return (id: string) => {
    resolve({ id });
  };
}

beforeEach(() => {
  vi.resetAllMocks();
  workspace = { id: "workspace-a", generation: 1 };
  mocks.workspaceContext.mockImplementation(() => ({ ...workspace }));
});
afterEach(() => unmount?.());
afterAll(() => vi.unstubAllGlobals());

it("navigates to an accepted rollout from the page that submitted it", async () => {
  mocks.createAgentRollout.mockResolvedValue({ id: "rollout-a" });
  const view = await mount();
  await view.submitRollout();
  expect(mocks.push).toHaveBeenCalledExactlyOnceWith({
    name: "rollout-detail",
    params: { rolloutId: "rollout-a" },
  });
  expect(view.rolloutReason).toBe("");
});

it("keeps a late create from navigating after the page unmounts", async () => {
  const resolve = created();
  const view = await mount();
  const submit = view.submitRollout();
  unmount?.();
  resolve("rollout-a");
  await submit;
  expect(mocks.createAgentRollout).toHaveBeenCalledTimes(1);
  expect(mocks.push).not.toHaveBeenCalled();
});

it("keeps a late create from clearing the next workspace's form", async () => {
  const resolve = created();
  const view = await mount();
  const submit = view.submitRollout();
  workspace = { id: "workspace-b", generation: 2 };
  window.dispatchEvent(new Event("workspace"));
  expect(view.rolloutStarting).toBe(false);
  view.rolloutReason = "second";
  resolve("rollout-a");
  await submit;
  expect(view.rolloutReason).toBe("second");
  expect(mocks.push).not.toHaveBeenCalled();
});

it("reports a lost create response as unconfirmed without resending", async () => {
  mocks.createAgentRollout.mockRejectedValue(new TypeError("network"));
  const view = await mount();
  await view.submitRollout();
  expect(view.rolloutError).toBe("rolloutStartUnconfirmed");
  expect(view.rolloutDialog).toBe(true);
  expect(mocks.createAgentRollout).toHaveBeenCalledTimes(1);
});

it("shows a definite server rejection as such", async () => {
  mocks.createAgentRollout.mockRejectedValue(
    new ResponseError(new Response("{}", { status: 409 }), "conflict"),
  );
  const view = await mount();
  await view.submitRollout();
  expect(view.rolloutError).toBe("conflict");
});
