import { createRenderer, nextTick, ssrContextKey } from "vue";
import type { BuildInfo, Workspace } from "@ocservia/api-client";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
const mocks = vi.hoisted(() => ({
  getWorkspace: vi.fn(),
  getVersion: vi.fn(),
}));
vi.mock("../src/api/workspace", () => ({
  ...mocks,
  workspaceChangedEvent: "workspace",
}));
vi.mock("../src/api/platform", () => mocks);
vi.mock("../src/shared/readiness", () => ({
  useReadinessStore: () => ({ isReady: true }),
}));
import SettingsView from "../src/views/SettingsView.vue";
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
  workspace?: Workspace;
  buildInfo?: BuildInfo;
  loading: boolean;
  unavailable: boolean;
  buildUnavailable: boolean;
  loadWorkspace(): Promise<void>;
}
const build: BuildInfo = { version: "0.2.0", commit: "fixture", role: "all" };
let unmount: (() => void) | undefined;
async function flush() {
  for (let i = 0; i < 8; i++) await nextTick();
}
async function mount(): Promise<View> {
  const app = renderer.createApp({ ...SettingsView, render: () => null });
  app.provide(ssrContextKey, {});
  const instance = app.mount({});
  unmount = () => {
    app.unmount();
  };
  await flush();
  return (instance.$ as unknown as { setupState: View }).setupState;
}
beforeEach(() => {
  vi.resetAllMocks();
  vi.stubGlobal("window", new EventTarget());
  mocks.getWorkspace.mockResolvedValue({
    id: "workspace-a",
    name: "A",
    slug: "a",
    version: 1,
  });
  mocks.getVersion.mockResolvedValue(build);
});
afterEach(() => {
  unmount?.();
  vi.unstubAllGlobals();
});
it("keeps an unconfigured recommendation distinct from a failed read", async () => {
  const view = await mount();
  expect(view.buildInfo?.recommendedAgentVersion).toBeUndefined();
  expect(view.buildUnavailable).toBe(false);
  expect(view.unavailable).toBe(false);
  mocks.getVersion.mockRejectedValueOnce(new Error("unavailable"));
  await view.loadWorkspace();
  expect(view.buildUnavailable).toBe(true);
  expect(view.unavailable).toBe(false);
  expect(view.buildInfo).toBeUndefined();
  mocks.getVersion.mockResolvedValueOnce({
    ...build,
    recommendedAgentVersion: "0.2.0",
  });
  await view.loadWorkspace();
  expect(view.buildUnavailable).toBe(false);
  expect(view.buildInfo?.recommendedAgentVersion).toBe("0.2.0");
});
it("reports workspace failures independently", async () => {
  mocks.getWorkspace.mockRejectedValueOnce(new Error("unavailable"));
  const view = await mount();
  expect(view.unavailable).toBe(true);
  expect(view.loading).toBe(false);
});
it("discards a delayed response after a workspace refresh", async () => {
  let resolveOld!: (value: Workspace) => void;
  mocks.getWorkspace.mockImplementationOnce(
    () =>
      new Promise<Workspace>((resolve) => {
        resolveOld = resolve;
      }),
  );
  const view = await mount();
  mocks.getWorkspace.mockResolvedValueOnce({
    id: "workspace-b",
    name: "B",
    slug: "b",
    version: 1,
  });
  await view.loadWorkspace();
  resolveOld({ id: "workspace-a", name: "A", slug: "a", version: 1 });
  await flush();
  expect(view.workspace?.id).toBe("workspace-b");
  expect(view.loading).toBe(false);
});
