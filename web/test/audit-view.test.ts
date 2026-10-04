import { createRenderer, nextTick, ssrContextKey } from "vue";
import { ResponseError } from "@ocservia/api-client";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
const mocks = vi.hoisted(() => ({
  listAuditEvents: vi.fn(),
  getWorkspace: vi.fn(),
  workspaceContext: vi.fn(),
}));
vi.mock("../src/api/events", () => mocks);
vi.mock("../src/api/workspace", () => ({
  ...mocks,
  workspaceChangedEvent: "workspace",
}));
vi.mock("vue-i18n", () => ({ useI18n: () => ({ t: (key: string) => key }) }));
import AuditView from "../src/views/AuditView.vue";
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
let unmount: (() => void) | undefined;
const record = {
  id: "audit-a",
  action: "node.approve",
  occurred_at: "2026-09-30T00:00:00Z",
};
interface View {
  items: object[];
  rows: { item: object; key: string }[];
  search: string;
  resultFilter: string;
  expanded: string[];
  toggle(key: string): void;
  error: string;
  initialized: boolean;
  loading: boolean;
  refresh(): Promise<void>;
}
async function flush() {
  for (let i = 0; i < 8; i++) await nextTick();
}
async function mount(): Promise<View> {
  const app = renderer.createApp({ ...AuditView, render: () => null });
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
  mocks.getWorkspace.mockResolvedValue({ id: "workspace-a" });
  mocks.workspaceContext.mockReturnValue({ id: "workspace-a", generation: 1 });
  mocks.listAuditEvents.mockResolvedValue({ items: [record] });
});
afterEach(() => {
  unmount?.();
  vi.unstubAllGlobals();
});
it("loads recent records and supports manual refresh", async () => {
  const view = await mount();
  expect(view.items).toEqual([record]);
  expect(view.initialized).toBe(true);
  await view.refresh();
  expect(mocks.listAuditEvents).toHaveBeenCalledTimes(2);
});
it("shows a confirmed empty result", async () => {
  mocks.listAuditEvents.mockResolvedValue({ items: [] });
  const view = await mount();
  expect(view.initialized).toBe(true);
  expect(view.items).toEqual([]);
  expect(view.error).toBe("");
});
it.each([403, 500])(
  "distinguishes HTTP %s from an empty result and retries",
  async (status) => {
    mocks.listAuditEvents.mockRejectedValueOnce(
      new ResponseError(new Response(null, { status })),
    );
    const view = await mount();
    expect(view.initialized).toBe(false);
    expect(view.error).toBe(
      status === 403 ? "auditForbidden" : "auditUnavailable",
    );
    await view.refresh();
    expect(view.error).toBe("");
    expect(view.items).toEqual([record]);
  },
);
it("keeps old records on a refresh failure", async () => {
  const view = await mount();
  mocks.listAuditEvents.mockRejectedValueOnce(new Error("offline"));
  await view.refresh();
  expect(view.items).toEqual([record]);
  expect(view.error).toBe("auditUnavailable");
});
it("clears old workspace data and rejects a late response", async () => {
  const view = await mount();
  let resolve!: (page: object) => void;
  mocks.listAuditEvents.mockImplementationOnce(
    () =>
      new Promise((r) => {
        resolve = r;
      }),
  );
  const old = view.refresh();
  await flush();
  mocks.workspaceContext.mockReturnValue({ id: "workspace-b", generation: 2 });
  window.dispatchEvent(new Event("workspace"));
  expect(view.items).toEqual([]);
  mocks.listAuditEvents.mockResolvedValue({ items: [] });
  await flush();
  resolve({ items: [record] });
  await old;
  expect(view.items).toEqual([]);
});
it("filters only the loaded slice and resets expansion on workspace change", async () => {
  const failed = {
    id: "audit-b",
    action: "user.disable",
    actor_id: "admin",
    result: "failed",
  };
  mocks.listAuditEvents.mockResolvedValue({
    items: [{ ...record, result: "intent" }, failed],
  });
  const view = await mount();
  view.resultFilter = "failed";
  await flush();
  expect(view.rows.map((row) => row.item)).toEqual([failed]);
  view.resultFilter = "";
  view.search = "NODE.APP";
  await flush();
  expect(view.rows).toHaveLength(1);
  view.search = "missing";
  await flush();
  expect(view.rows).toEqual([]);
  expect(mocks.listAuditEvents).toHaveBeenCalledTimes(1);
  view.toggle("audit-a0");
  view.toggle("audit-b1");
  view.toggle("audit-a0");
  expect(view.expanded).toEqual(["audit-b1"]);
  window.dispatchEvent(new Event("workspace"));
  expect(view.expanded).toEqual([]);
});
