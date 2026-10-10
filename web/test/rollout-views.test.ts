import { createRenderer, nextTick, reactive, ssrContextKey } from "vue";
import { ResponseError, type AgentRollout } from "@ocservia/api-client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const mocks = vi.hoisted(() => ({
  getAgentRollout: vi.fn(),
  resumeAgentRollout: vi.fn(),
  listAgentRollouts: vi.fn(),
  createAgentRollout: vi.fn(),
  listOperations: vi.fn(),
  getOperation: vi.fn(),
  getWorkspace: vi.fn(),
  workspaceContext: vi.fn(),
  push: vi.fn(),
  route: { params: { rolloutId: "rollout-a" } },
}));
vi.mock("../src/api/agents", () => mocks);
vi.mock("../src/api/operations", () => mocks);
vi.mock("../src/api/workspace", () => ({
  ...mocks,
  workspaceChangedEvent: "workspace",
}));
vi.mock("vue-router", () => ({
  useRoute: () => mocks.route,
  useRouter: () => ({ push: mocks.push }),
}));
vi.mock("vue-i18n", () => ({ useI18n: () => ({ t: (key: string) => key }) }));
import OperationsView from "../src/views/OperationsView.vue";
import RolloutDetailView from "../src/views/RolloutDetailView.vue";

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

interface Deferred<T> {
  promise: Promise<T>;
  resolve(value: T): void;
  reject(cause: unknown): void;
}
function deferred<T>(): Deferred<T> {
  let resolve!: (value: T) => void;
  let reject!: (cause: unknown) => void;
  const promise = new Promise<T>((done, fail) => {
    resolve = done;
    reject = fail;
  });
  return { promise, resolve, reject };
}

function rollout(id: string, state: string, workspaceId = "workspace-a") {
  return {
    id,
    workspaceId,
    targetVersion: "0.2.0",
    state,
    batchSize: 5,
    stopOnFailure: true,
    reason: "rollout",
    approvalId: "approval",
    createdBy: "operator",
    currentBatch: 0,
    createdAt: "2026-01-01T00:00:00Z",
    updatedAt: "2026-01-01T00:00:00Z",
    nodes: [],
  } as unknown as AgentRollout;
}

function responseError(status: number): ResponseError {
  return new ResponseError(new Response("{}", { status }));
}

let workspace = { id: "workspace-a", generation: 1 };
function switchWorkspace(id: string): void {
  workspace = { id, generation: workspace.generation + 1 };
  window.dispatchEvent(new Event("workspace"));
}

let unmount: (() => void) | undefined;
// Foreground reads requested by a route or workspace change run on a 0 ms timer.
async function flush() {
  for (let i = 0; i < 8; i++) await nextTick();
  await vi.advanceTimersByTimeAsync(0);
  for (let i = 0; i < 8; i++) await nextTick();
}
async function mount<T>(component: object): Promise<T> {
  const app = renderer.createApp({ ...component, render: () => null });
  app.provide(ssrContextKey, {});
  const instance = app.mount({});
  unmount = () => {
    app.unmount();
    unmount = undefined;
  };
  await flush();
  return (instance.$ as unknown as { setupState: T }).setupState;
}

beforeEach(() => {
  vi.useFakeTimers();
  vi.resetAllMocks();
  vi.stubGlobal("window", new EventTarget());
  workspace = { id: "workspace-a", generation: 1 };
  mocks.route = reactive({ params: { rolloutId: "rollout-a" } });
  mocks.workspaceContext.mockImplementation(() => ({ ...workspace }));
  mocks.getWorkspace.mockImplementation(() =>
    Promise.resolve({ id: workspace.id }),
  );
});
afterEach(() => {
  unmount?.();
  expect(vi.getTimerCount()).toBe(0);
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

interface DetailView {
  rollout?: AgentRollout;
  loading: boolean;
  notFound: boolean;
  unavailable: boolean;
  resuming: boolean;
  resumeBlocked: boolean;
  resumeError: string;
  resume(): Promise<void>;
}

describe("rollout detail", () => {
  it("keeps the newer route rollout when an older read finishes last", async () => {
    const slow = deferred<AgentRollout>();
    mocks.getAgentRollout.mockImplementation((id: string) =>
      id === "rollout-a" ? slow.promise : rollout("rollout-b", "running"),
    );
    const view = await mount<DetailView>(RolloutDetailView);
    mocks.route.params.rolloutId = "rollout-b";
    await flush();
    expect(view.rollout?.id).toBe("rollout-b");
    slow.resolve(rollout("rollout-a", "paused"));
    await flush();
    expect(view.rollout?.id).toBe("rollout-b");
    expect(view.loading).toBe(false);
  });

  it("ignores an older read that fails after the route changed", async () => {
    const slow = deferred<AgentRollout>();
    mocks.getAgentRollout.mockImplementation((id: string) =>
      id === "rollout-a" ? slow.promise : rollout("rollout-b", "running"),
    );
    const view = await mount<DetailView>(RolloutDetailView);
    mocks.route.params.rolloutId = "rollout-b";
    await flush();
    slow.reject(responseError(404));
    await flush();
    expect(view.rollout?.id).toBe("rollout-b");
    expect(view.notFound).toBe(false);
    expect(view.unavailable).toBe(false);
  });

  it("drops a read from an earlier visit to the same workspace", async () => {
    const first = deferred<AgentRollout>();
    mocks.getAgentRollout.mockReturnValueOnce(first.promise);
    mocks.getAgentRollout.mockReturnValue(new Promise(() => {}));
    const view = await mount<DetailView>(RolloutDetailView);
    switchWorkspace("workspace-b");
    switchWorkspace("workspace-a");
    first.resolve(rollout("rollout-a", "paused"));
    await flush();
    expect(view.rollout).toBeUndefined();
    expect(view.loading).toBe(true);
  });

  it("reads the new rollout after a terminal one on the same route", async () => {
    mocks.getAgentRollout.mockImplementation((id: string) =>
      Promise.resolve(
        rollout(id, id === "rollout-a" ? "succeeded" : "running"),
      ),
    );
    const view = await mount<DetailView>(RolloutDetailView);
    expect(view.rollout?.state).toBe("succeeded");
    mocks.route.params.rolloutId = "rollout-b";
    await nextTick();
    expect(view.rollout).toBeUndefined();
    await flush();
    expect(view.rollout?.id).toBe("rollout-b");
    expect(mocks.getAgentRollout).toHaveBeenLastCalledWith(
      "rollout-b",
      expect.any(AbortSignal),
    );
  });

  it("hides the old rollout and resume action on a workspace change", async () => {
    mocks.getAgentRollout.mockResolvedValueOnce(rollout("rollout-a", "paused"));
    mocks.getAgentRollout.mockRejectedValue(responseError(404));
    const view = await mount<DetailView>(RolloutDetailView);
    expect(view.rollout?.state).toBe("paused");
    switchWorkspace("workspace-b");
    expect(view.rollout).toBeUndefined();
    await flush();
    expect(mocks.getAgentRollout).toHaveBeenCalledTimes(2);
    expect(view.notFound).toBe(true);
    await view.resume();
    expect(mocks.resumeAgentRollout).not.toHaveBeenCalled();
  });

  it("rejects a rollout reported for another workspace", async () => {
    mocks.getAgentRollout.mockResolvedValue(
      rollout("rollout-a", "paused", "workspace-b"),
    );
    const view = await mount<DetailView>(RolloutDetailView);
    expect(view.rollout).toBeUndefined();
    expect(view.unavailable).toBe(true);
  });

  it.each([
    [404, "notFound"],
    [403, "unavailable"],
    [503, "unavailable"],
  ] as const)("maps a real %i ResponseError to %s", async (status, field) => {
    mocks.getAgentRollout.mockRejectedValue(responseError(status));
    const view = await mount<DetailView>(RolloutDetailView);
    expect(view[field]).toBe(true);
    expect(view.notFound).toBe(status === 404);
  });

  it("does not write a late resume into another rollout", async () => {
    const resume = deferred<AgentRollout>();
    mocks.getAgentRollout.mockImplementation((id: string) =>
      Promise.resolve(rollout(id, "paused")),
    );
    mocks.resumeAgentRollout.mockReturnValue(resume.promise);
    const view = await mount<DetailView>(RolloutDetailView);
    void view.resume();
    mocks.route.params.rolloutId = "rollout-b";
    await flush();
    expect(view.resuming).toBe(false);
    resume.resolve(rollout("rollout-a", "running"));
    await flush();
    expect(view.rollout?.id).toBe("rollout-b");
    expect(view.rollout?.state).toBe("paused");
  });

  it("keeps the resumed state when an older paused read returns", async () => {
    const staleRead = deferred<AgentRollout>();
    mocks.getAgentRollout.mockResolvedValueOnce(rollout("rollout-a", "paused"));
    mocks.getAgentRollout.mockReturnValueOnce(staleRead.promise);
    mocks.getAgentRollout.mockResolvedValue(rollout("rollout-a", "running"));
    mocks.resumeAgentRollout.mockResolvedValue(rollout("rollout-a", "running"));
    const view = await mount<DetailView>(RolloutDetailView);
    await vi.advanceTimersByTimeAsync(2000);
    expect(mocks.getAgentRollout).toHaveBeenCalledTimes(2);
    await view.resume();
    expect(view.rollout?.state).toBe("running");
    staleRead.resolve(rollout("rollout-a", "paused"));
    await flush();
    expect(view.rollout?.state).toBe("running");
    expect(view.resuming).toBe(false);
  });

  it("never resends a resume whose response was lost", async () => {
    mocks.getAgentRollout.mockResolvedValue(rollout("rollout-a", "paused"));
    mocks.resumeAgentRollout.mockRejectedValue(new TypeError("network"));
    const view = await mount<DetailView>(RolloutDetailView);
    mocks.getAgentRollout.mockReturnValue(new Promise(() => {}));
    await view.resume();
    expect(view.resumeError).toBe("rolloutResumeUnconfirmed");
    expect(view.resumeBlocked).toBe(true);
    await view.resume();
    await flush();
    expect(mocks.resumeAgentRollout).toHaveBeenCalledTimes(1);
    expect(mocks.getAgentRollout).toHaveBeenCalledTimes(2);
  });

  it("re-enables resume only after a fresh read confirms the pause", async () => {
    mocks.getAgentRollout.mockResolvedValue(rollout("rollout-a", "paused"));
    mocks.resumeAgentRollout.mockRejectedValue(new TypeError("network"));
    const view = await mount<DetailView>(RolloutDetailView);
    await view.resume();
    await flush();
    expect(view.resumeBlocked).toBe(false);
    expect(view.rollout?.state).toBe("paused");
  });

  it("stops reading and writing after unmount", async () => {
    const read = deferred<AgentRollout>();
    mocks.getAgentRollout.mockReturnValue(read.promise);
    const view = await mount<DetailView>(RolloutDetailView);
    unmount?.();
    read.reject(new TypeError("network"));
    await flush();
    expect(view.unavailable).toBe(false);
    expect(vi.getTimerCount()).toBe(0);
  });
});

interface OperationsState {
  rollouts: AgentRollout[];
  detailError: string;
  refresh(): void;
  inspectOperation(id: string): Promise<void>;
}

describe("operations rollout list", () => {
  beforeEach(() => {
    mocks.listOperations.mockResolvedValue({
      items: [],
      page: { hasMore: false },
    });
  });

  it("clears rows on a workspace change and drops the older page", async () => {
    const slow = deferred<{ rollouts: AgentRollout[] }>();
    mocks.listAgentRollouts.mockResolvedValueOnce({
      rollouts: [rollout("rollout-a", "running")],
    });
    mocks.listAgentRollouts.mockReturnValueOnce(slow.promise);
    mocks.listAgentRollouts.mockResolvedValue({
      rollouts: [rollout("rollout-b", "running", "workspace-b")],
    });
    const view = await mount<OperationsState>(OperationsView);
    expect(view.rollouts.map((item) => item.id)).toEqual(["rollout-a"]);
    view.refresh();
    await flush();
    expect(mocks.listAgentRollouts).toHaveBeenCalledTimes(2);
    switchWorkspace("workspace-b");
    expect(view.rollouts).toEqual([]);
    await flush();
    slow.resolve({ rollouts: [rollout("rollout-a", "paused")] });
    await flush();
    expect(view.rollouts.map((item) => item.id)).toEqual(["rollout-b"]);
  });

  it("refreshes the rollout list from the page refresh action", async () => {
    mocks.listAgentRollouts.mockResolvedValue({ rollouts: [] });
    const view = await mount<OperationsState>(OperationsView);
    view.refresh();
    await flush();
    expect(mocks.listAgentRollouts).toHaveBeenCalledTimes(2);
    expect(mocks.listOperations).toHaveBeenCalledTimes(2);
  });

  it("ignores an operation detail failure from the previous workspace", async () => {
    const detail = deferred<unknown>();
    mocks.listAgentRollouts.mockResolvedValue({ rollouts: [] });
    mocks.getOperation.mockReturnValue(detail.promise);
    const view = await mount<OperationsState>(OperationsView);
    void view.inspectOperation("operation-a");
    await flush();
    switchWorkspace("workspace-b");
    detail.reject(new TypeError("network"));
    await flush();
    expect(view.detailError).toBe("");
  });
});
