import {
  createRenderer,
  h,
  nextTick,
  reactive,
  ref,
  ssrContextKey,
  type Component,
} from "vue";
import { afterEach, beforeEach, expect, it, vi } from "vitest";

const mocks = vi.hoisted(() => ({
  workspaceContext: vi.fn(),
  loadUserPolicy: vi.fn(),
  saveUserPolicy: vi.fn(),
}));
vi.mock("vue-i18n", () => ({ useI18n: () => ({ t: (key: string) => key }) }));
vi.mock("../src/api/workspace", async (original) => ({
  ...(await original<typeof import("../src/api/workspace")>()),
  workspaceContext: mocks.workspaceContext,
}));
vi.mock("../src/adapters/user-policy", async (original) => ({
  ...(await original<typeof import("../src/adapters/user-policy")>()),
  loadUserPolicy: mocks.loadUserPolicy,
  saveUserPolicy: mocks.saveUserPolicy,
}));

import UserPolicyDialog from "../src/components/nodes/UserPolicyDialog.vue";

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

interface Dialog {
  policyLoading: boolean;
  policyError: string;
  policyForm: { version: number };
  policyReason: string;
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

const form = (version: number) => ({
  period: "none",
  direction: "rxtx",
  quotaValue: 0,
  quotaUnit: "MiB",
  expiresAtLocal: "",
  version,
});
const unmounts: (() => void)[] = [];
const Stub: Component = { ...UserPolicyDialog, render: () => null };

async function mount(props: { nodeId: string | undefined; username: string }) {
  const state = reactive({ ...props, open: true });
  const child = ref<{ $: { setupState: Dialog } }>();
  const close = vi.fn(() => {
    state.open = false;
  });
  const app = renderer.createApp({
    render: () =>
      state.open
        ? h(Stub, {
            ref: child,
            key: state.username,
            nodeId: state.nodeId,
            username: state.username,
            onClose: close,
          })
        : null,
  });
  app.provide(ssrContextKey, {});
  app.mount({});
  unmounts.push(() => {
    app.unmount();
  });
  await nextTick();
  if (!child.value) throw new Error("dialog did not mount");
  const dialog = child.value.$.setupState;
  return { state, dialog, close };
}

beforeEach(() => {
  vi.resetAllMocks();
  mocks.workspaceContext.mockReturnValue({ id: "workspace-a", generation: 1 });
  mocks.loadUserPolicy.mockResolvedValue(form(2));
  mocks.saveUserPolicy.mockResolvedValue(form(3));
});
afterEach(() => {
  unmounts.splice(0).forEach((unmount) => {
    unmount();
  });
});

it("loads on mount and closes after a confirmed save", async () => {
  const { dialog, close } = await mount({ nodeId: "node-a", username: "a" });
  await vi.waitFor(() => {
    expect(dialog.policyLoading).toBe(false);
  });
  expect(mocks.loadUserPolicy).toHaveBeenCalledWith(
    "node-a",
    "a",
    expect.any(AbortSignal),
  );
  expect(dialog.policyForm.version).toBe(2);
  await dialog.submitPolicy();
  expect(mocks.saveUserPolicy).not.toHaveBeenCalled();
  dialog.policyReason = " quota ";
  await dialog.submitPolicy();
  expect(mocks.saveUserPolicy).toHaveBeenCalledWith(
    "node-a",
    "a",
    expect.objectContaining({ version: 2 }),
    "quota",
  );
  expect(close).toHaveBeenCalledOnce();
});

it("ignores a closed dialog's late read in a reopened dialog", async () => {
  const old = deferred<never>();
  const fresh = deferred<ReturnType<typeof form>>();
  mocks.loadUserPolicy
    .mockReturnValueOnce(old.promise)
    .mockReturnValueOnce(fresh.promise);
  const first = await mount({ nodeId: "node-a", username: "alice" });
  first.state.open = false;
  await nextTick();
  const second = await mount({ nodeId: "node-a", username: "alice" });
  old.reject(new Error("old policy"));
  await Promise.resolve();
  expect(second.dialog.policyLoading).toBe(true);
  expect(second.dialog.policyError).toBe("");
  fresh.resolve(form(4));
  await vi.waitFor(() => {
    expect(second.dialog.policyLoading).toBe(false);
  });
  expect(second.dialog.policyForm.version).toBe(4);
});

it.each(
  ["username", "node", "workspace"].flatMap((kind) => [
    [kind, "success"],
    [kind, "failure"],
  ]),
)(
  "keeps a late save from closing or erroring after a %s change (%s)",
  async (kind, outcome) => {
    const { state, dialog, close } = await mount({
      nodeId: "node-a",
      username: "alice",
    });
    await vi.waitFor(() => {
      expect(dialog.policyLoading).toBe(false);
    });
    const saved = deferred<ReturnType<typeof form>>();
    mocks.saveUserPolicy.mockReturnValueOnce(saved.promise);
    dialog.policyReason = "save";
    const pending = dialog.submitPolicy();
    expect(dialog.policyLoading).toBe(true);
    await dialog.submitPolicy();
    expect(mocks.saveUserPolicy).toHaveBeenCalledOnce();
    if (kind === "username") state.username = "bob";
    else if (kind === "node") state.nodeId = "node-b";
    else
      mocks.workspaceContext.mockReturnValue({
        id: "workspace-a",
        generation: 2,
      });
    await nextTick();
    if (outcome === "success") saved.resolve(form(9));
    else saved.reject(new Error("late failure"));
    await pending;
    expect(close).not.toHaveBeenCalled();
    expect(dialog.policyError).toBe("");
    expect(state.open).toBe(true);
  },
);
