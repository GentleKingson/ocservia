import { createRenderer, nextTick, reactive, ssrContextKey } from "vue";
import { ResponseError, type Approval } from "@ocservia/api-client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const mocks = vi.hoisted(() => ({
  getApproval: vi.fn(),
  approveRequest: vi.fn(),
  getWorkspace: vi.fn(),
  workspaceContext: vi.fn(),
  push: vi.fn(),
  route: { params: { approvalId: "approval-a" } },
}));
vi.mock("../src/api/approvals", () => mocks);
vi.mock("../src/api/workspace", () => ({
  ...mocks,
  workspaceChangedEvent: "workspace",
}));
vi.mock("vue-router", () => ({
  useRoute: () => mocks.route,
  useRouter: () => ({ push: mocks.push }),
}));
vi.mock("vue-i18n", () => ({ useI18n: () => ({ t: (key: string) => key }) }));
import ApprovalsView from "../src/views/ApprovalsView.vue";

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
  approval?: Approval;
  reason: string;
  reviewed: boolean;
  summary?: string;
  error: string;
  canApprove: boolean;
  approve(): Promise<void>;
  loadApproval(): Promise<void>;
}
const pending = {
  id: "approval-a",
  workspaceId: "workspace-a",
  requesterId: "requester",
  action: "service.reload",
  resourceType: "node",
  resourceId: "node-a",
  reason: "maintenance",
  status: "pending",
  requestHash: "ab".repeat(32),
  requestSummary: {
    action: "service.reload",
    resource_id: "node-a",
    enabled: false,
  },
  expiresAt: "2026-09-21T01:00:00Z",
  createdAt: "2026-09-21T00:00:00Z",
} as Approval;
let unmount: (() => void) | undefined;
async function flush() {
  for (let i = 0; i < 8; i++) await nextTick();
}
async function mount(): Promise<View> {
  const app = renderer.createApp({ ...ApprovalsView, render: () => null });
  app.provide(ssrContextKey, {});
  const instance = app.mount({});
  unmount = () => {
    app.unmount();
  };
  await flush();
  return (instance.$ as unknown as { setupState: View }).setupState;
}
function review(view: View) {
  view.reviewed = true;
  view.reason = " independently reviewed ";
}
beforeEach(() => {
  vi.useFakeTimers();
  vi.setSystemTime(new Date("2026-09-21T00:00:00Z"));
  vi.resetAllMocks();
  vi.stubGlobal("window", new EventTarget());
  mocks.route = reactive({ params: { approvalId: "approval-a" } });
  mocks.workspaceContext.mockReturnValue({ id: "workspace-a", generation: 1 });
  mocks.getWorkspace.mockResolvedValue({ id: "workspace-a" });
  mocks.getApproval.mockResolvedValue({ ...pending });
  mocks.approveRequest.mockResolvedValue({
    ...pending,
    status: "approved",
    approverId: "approver",
  });
});
afterEach(() => {
  unmount?.();
  expect(vi.getTimerCount()).toBe(0);
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe("approval review", () => {
  it("shows bound content and sends only the hash explicitly reviewed", async () => {
    const view = await mount();
    expect(view.summary).toContain('"enabled": false');
    expect(view.canApprove).toBe(false);
    review(view);
    expect(view.canApprove).toBe(true);
    await view.approve();
    expect(mocks.approveRequest).toHaveBeenCalledExactlyOnceWith("approval-a", {
      expectedRequestHash: pending.requestHash,
      reason: "independently reviewed",
    });
    expect(mocks.getApproval).toHaveBeenCalledTimes(1);
    expect(view.approval?.status).toBe("approved");
    review(view);
    await view.approve();
    expect(mocks.approveRequest).toHaveBeenCalledTimes(1);
  });
  it.each(["hash", "summary", "expired", "consumed"])(
    "refuses incomplete/nonpending evidence: %s",
    async (kind) => {
      mocks.getApproval.mockResolvedValue({
        ...pending,
        ...(kind === "hash" ? { requestHash: undefined } : {}),
        ...(kind === "summary" ? { requestSummary: {} } : {}),
        ...(kind === "expired" ? { expiresAt: "2026-09-20T00:00:00Z" } : {}),
        ...(kind === "consumed" ? { status: "consumed" } : {}),
      });
      const view = await mount();
      review(view);
      await view.approve();
      expect(mocks.approveRequest).not.toHaveBeenCalled();
    },
  );
  it("expires while open without another fetch", async () => {
    const view = await mount();
    review(view);
    await vi.advanceTimersByTimeAsync(3_600_001);
    expect(view.canApprove).toBe(false);
    await view.approve();
    expect(mocks.approveRequest).not.toHaveBeenCalled();
  });
  it.each(["node-workspace", "a-b-a"])(
    "does not decide across a changed workspace: %s",
    async (kind) => {
      const view = await mount();
      review(view);
      mocks.workspaceContext.mockReturnValue({
        id: kind === "a-b-a" ? "workspace-a" : "workspace-b",
        generation: 3,
      });
      await view.approve();
      expect(mocks.approveRequest).not.toHaveBeenCalled();
    },
  );
  it("rejects a cross-workspace response even for an otherwise authorized user", async () => {
    mocks.getApproval.mockResolvedValue({
      ...pending,
      workspaceId: "workspace-b",
    });
    const view = await mount();
    expect(view.approval).toBeUndefined();
    expect(view.error).toBe("approvalWorkspaceMismatch");
  });
  it.each(["lost", "self-approval"])(
    "requires explicit readback after %s, never retries a POST",
    async (kind) => {
      mocks.approveRequest.mockRejectedValue(
        kind === "lost"
          ? new Error("connection lost")
          : new ResponseError(new Response(null, { status: 403 })),
      );
      const view = await mount();
      review(view);
      await view.approve();
      expect(view.approval).toBeUndefined();
      expect(view.error).toBe(
        kind === "lost" ? "approvalDecisionUnconfirmed" : "approvalForbidden",
      );
      review(view);
      await view.approve();
      expect(mocks.approveRequest).toHaveBeenCalledTimes(1);
      mocks.getApproval.mockResolvedValue({ ...pending, status: "approved" });
      await view.loadApproval();
      expect(view.approval?.status).toBe("approved");
      expect(mocks.approveRequest).toHaveBeenCalledTimes(1);
    },
  );
  it("does not publish an old request's response after route navigation", async () => {
    let resolve: ((value: Approval) => void) | undefined;
    mocks.getApproval.mockReturnValueOnce(
      new Promise<Approval>((r) => {
        resolve = r;
      }),
    );
    const view = await mount();
    mocks.getApproval.mockResolvedValue({ ...pending, id: "approval-b" });
    mocks.route.params.approvalId = "approval-b";
    await flush();
    resolve?.(pending);
    await flush();
    expect(view.approval?.id).toBe("approval-b");
  });
  it("ignores a completed decision after switching workspace", async () => {
    let resolve: ((value: Approval) => void) | undefined;
    mocks.approveRequest.mockReturnValueOnce(
      new Promise<Approval>((r) => {
        resolve = r;
      }),
    );
    const view = await mount();
    review(view);
    const result = view.approve();
    mocks.workspaceContext.mockReturnValue({
      id: "workspace-b",
      generation: 2,
    });
    mocks.getApproval.mockRejectedValue(new Error("forbidden"));
    window.dispatchEvent(new Event("workspace"));
    await flush();
    resolve?.({ ...pending, status: "approved" });
    await result;
    expect(view.approval).toBeUndefined();
    expect(view.error).toBe("forbidden");
  });
});
