import { mountSetup } from "./renderer";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { flushTicks, responseError } from "./helpers";
const mocks = vi.hoisted(() => ({
  listAuditEvents: vi.fn(),
  verifyAuditChain: vi.fn(),
}));
vi.mock("../src/api/events", () => mocks);
vi.mock("vue-i18n", () => ({ useI18n: () => ({ t: (key: string) => key }) }));
import AuditView from "../src/views/AuditView.vue";
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
  verifying: boolean;
  verification?: {
    workspace_id: string;
    events: number;
    valid: boolean;
    checkpoint_valid: boolean;
    compacted_events?: number;
  };
  verifyError: string;
  refresh(): Promise<void>;
  verifyChain(): Promise<void>;
}
async function mount(): Promise<View> {
  const mounted = mountSetup(AuditView);
  unmount = mounted.unmount;
  await flushTicks();
  return mounted.view as View;
}
beforeEach(() => {
  vi.resetAllMocks();
  vi.stubGlobal("window", new EventTarget());
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
    mocks.listAuditEvents.mockRejectedValueOnce(responseError(status));
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
it("filters only the loaded slice", async () => {
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
  await flushTicks();
  expect(view.rows.map((row) => row.item)).toEqual([failed]);
  view.resultFilter = "";
  view.search = "NODE.APP";
  await flushTicks();
  expect(view.rows).toHaveLength(1);
  view.search = "missing";
  await flushTicks();
  expect(view.rows).toEqual([]);
  expect(mocks.listAuditEvents).toHaveBeenCalledTimes(1);
  view.toggle("id:audit-a");
  view.toggle("id:audit-b");
  view.toggle("id:audit-a");
  expect(view.expanded).toEqual(["id:audit-b"]);
});
it("renders valid and invalid audit evidence decoded by the API", async () => {
  const validResult = {
    workspace_id: "workspace-a",
    events: 12,
    valid: true,
    checkpoint_valid: true,
  };
  mocks.verifyAuditChain.mockResolvedValueOnce(validResult);
  const view = await mount();
  await view.verifyChain();
  expect(view.verification).toEqual(validResult);
  expect(view.verifyError).toBe("");

  mocks.verifyAuditChain.mockResolvedValueOnce({
    workspace_id: "workspace-a",
    events: 12,
    valid: false,
    checkpoint_valid: false,
  });
  await view.verifyChain();
  expect(view.verification).toEqual({
    workspace_id: "workspace-a",
    events: 12,
    valid: false,
    checkpoint_valid: false,
  });
  expect(view.verifyError).toBe("");

  mocks.verifyAuditChain.mockRejectedValueOnce(responseError(403));
  await view.verifyChain();
  expect(view.verification).toBeUndefined();
  expect(view.verifyError).toBe("auditForbidden");
});
it("keeps expansion on the same event across refreshes and keeps unkeyed rows", async () => {
  const newer = { id: "audit-new", action: "node.update" };
  mocks.listAuditEvents.mockResolvedValue({ items: [record] });
  const view = await mount();
  view.toggle("id:audit-a");
  mocks.listAuditEvents.mockResolvedValue({
    items: [newer, record, { action: "no.id" }, { ...record, action: "dup" }],
  });
  await view.refresh();
  await flushTicks();
  expect(view.rows.map((row) => row.key)).toEqual([
    "id:audit-new",
    "id:audit-a",
    "row:2",
    "row:3",
  ]);
  expect(view.rows.filter((row) => view.expanded.includes(row.key))).toEqual([
    { item: record, key: "id:audit-a" },
  ]);
});
