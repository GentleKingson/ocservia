import { createRenderer, nextTick, ssrContextKey } from "vue";
import { ResponseError } from "@ocservia/api-client";
import { afterAll, afterEach, beforeEach, expect, it, vi } from "vitest";
import type { NodeInstallConfig } from "../src/features/nodes/node-install";
const mocks = vi.hoisted(() => {
  vi.stubGlobal("window", new EventTarget());
  return {
    createNodeBootstrapToken: vi.fn(),
    getWorkspace: vi.fn(),
    workspaceContext: vi.fn(),
  };
});
vi.mock("@vueuse/core", async (original) => ({
  ...(await original<object>()),
  defaultWindow: globalThis.window,
}));
vi.mock("../src/api/enrollment", () => mocks);
vi.mock("../src/api/workspace", async (original) => ({
  ...(await original<object>()),
  ...mocks,
  workspaceChangedEvent: "workspace",
}));
import NodeInstallDialog from "../src/components/nodes/NodeInstallDialog.vue";
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
  config: NodeInstallConfig;
  step: number;
  token: string;
  expires: string;
  error: string;
  pending: boolean;
  prepare(): void;
  issue(): Promise<void>;
  next(): void;
  close(): void;
  copyConfig(): Promise<void>;
}
const workspaceId = "019fc0a4-6d92-765c-a8a1-4af556614fa1";
let context = { id: workspaceId, generation: 1 };
let unmount: (() => void) | undefined;
const close = vi.fn();
const token = "obt1_" + "a".repeat(43);
const result = () => ({ token, expiresAt: new Date(Date.now() + 900_000) });
async function mount() {
  const app = renderer.createApp(
    { ...NodeInstallDialog, render: () => null },
    { onClose: close },
  );
  app.provide(ssrContextKey, {});
  const instance = app.mount({});
  unmount = () => {
    app.unmount();
    unmount = undefined;
  };
  for (let i = 0; i < 4; i++) await nextTick();
  const view = (instance.$ as unknown as { setupState: View }).setupState;
  Object.assign(view.config, {
    name: "edge-1",
    release: "v1.2.3",
    controller: "a".repeat(64),
    relay: "https://relay.example.com",
  });
  view.prepare();
  return view;
}
beforeEach(() => {
  vi.resetAllMocks();
  context = { id: workspaceId, generation: 1 };
  mocks.getWorkspace.mockResolvedValue({ id: workspaceId, name: "Workspace" });
  mocks.workspaceContext.mockImplementation(() => ({ ...context }));
  mocks.createNodeBootstrapToken.mockResolvedValue(result());
});
afterEach(() => {
  unmount?.();
  vi.useRealTimers();
});
afterAll(() => vi.unstubAllGlobals());
it("issues only on click, once, and clears the token on continuing", async () => {
  const view = await mount();
  expect(mocks.createNodeBootstrapToken).not.toHaveBeenCalled();
  await Promise.all([view.issue(), view.issue()]);
  expect(view.token).toBe(token);
  expect(view.expires).toBeTruthy();
  await view.issue();
  expect(mocks.createNodeBootstrapToken).toHaveBeenCalledTimes(1);
  view.next();
  expect(view.token).toBe("");
  expect(view.step).toBe(3);
});
it.each(["close", "workspace", "unmount", "pagehide"])(
  "clears received secrets on %s",
  async (action) => {
    const view = await mount();
    await view.issue();
    if (action === "pagehide") window.dispatchEvent(new Event("pagehide"));
    else if (action === "close") view.close();
    else if (action === "workspace")
      window.dispatchEvent(new Event("workspace"));
    else unmount?.();
    expect(view.token).toBe("");
    expect(view.expires).toBe("");
  },
);
it.each(["close", "workspace", "unmount", "generation", "pagehide"])(
  "discards late success on %s",
  async (action) => {
    let resolve!: (value: ReturnType<typeof result>) => void;
    mocks.createNodeBootstrapToken.mockReturnValue(
      new Promise((done) => {
        resolve = done;
      }),
    );
    const view = await mount();
    const request = view.issue();
    if (action === "pagehide") window.dispatchEvent(new Event("pagehide"));
    else if (action === "close") view.close();
    else if (action === "workspace") {
      context = { id: "other", generation: 2 };
      window.dispatchEvent(new Event("workspace"));
    } else if (action === "generation")
      context = { id: workspaceId, generation: 3 };
    else unmount?.();
    resolve(result());
    await request;
    expect(view.token).toBe("");
    expect(view.expires).toBe("");
  },
);
it.each([
  [400, "installBadRequest"],
  [403, "installForbidden"],
  [503, "installUnknown"],
  ["network", "installUnknown"],
])("reports %s without retry or server text", async (status, message) => {
  mocks.createNodeBootstrapToken.mockRejectedValue(
    status === "network"
      ? new TypeError(token)
      : new ResponseError(
          new Response(token, { status: Number(status) }),
          token,
        ),
  );
  const view = await mount();
  await view.issue();
  await view.issue();
  expect(view.error).toBe(message);
  expect(view.token).toBe("");
  expect(mocks.createNodeBootstrapToken).toHaveBeenCalledTimes(1);
});
it("handles malformed success as unknown and does not retry", async () => {
  mocks.createNodeBootstrapToken.mockResolvedValue({
    token,
    expiresAt: new Date("invalid"),
  });
  const view = await mount();
  await view.issue();
  await view.issue();
  expect(view.error).toBe("installUnknown");
  expect(view.token).toBe("");
  expect(mocks.createNodeBootstrapToken).toHaveBeenCalledTimes(1);
});
it("clears an expired token without polling or reissuing", async () => {
  vi.useFakeTimers();
  const view = await mount();
  await view.issue();
  vi.advanceTimersByTime(900_000);
  expect(view.token).toBe("");
  expect(view.error).toBe("installExpired");
  expect(mocks.createNodeBootstrapToken).toHaveBeenCalledTimes(1);
});

it("clears a clipboard failure after successful retry without clearing a token error", async () => {
  const writeText = vi
    .fn<(text: string) => Promise<void>>()
    .mockRejectedValueOnce(new Error("clipboard unavailable"))
    .mockResolvedValue(undefined);
  vi.stubGlobal("navigator", { clipboard: { writeText } });
  const view = await mount();
  await view.copyConfig();
  expect(view.error).toBe("installCopyFailed");
  await view.copyConfig();
  expect(view.error).toBe("");
  view.error = "installUnknown";
  await view.copyConfig();
  expect(view.error).toBe("installUnknown");
});
