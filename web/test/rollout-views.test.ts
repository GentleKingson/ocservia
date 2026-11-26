import { nextTick, reactive } from "vue";
import type { AgentRollout } from "@/api/models";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { flushTimers as flush, responseError } from "./helpers";
import { mountSetup } from "./renderer";

const mocks = vi.hoisted(() => ({
  getAgentRollout: vi.fn(),
  resumeAgentRollout: vi.fn(),
  listAgentRollouts: vi.fn(),
  createAgentRollout: vi.fn(),
  listOperations: vi.fn(),
  getOperation: vi.fn(),
  getWorkspaceId: vi.fn(),
  push: vi.fn(),
  route: { params: { rolloutId: "rollout-a" } },
}));
vi.mock("../src/api/agents", () => mocks);
vi.mock("../src/api/operations", () => mocks);
vi.mock("../src/api/workspace", async (original) => ({
  ...(await original<typeof import("../src/api/workspace")>()),
  ...mocks,
}));
vi.mock("vue-router", () => ({
  useRoute: () => mocks.route,
  useRouter: () => ({ push: mocks.push }),
}));
vi.mock("vue-i18n", () => ({ useI18n: () => ({ t: (key: string) => key }) }));
import OperationsView from "../src/views/OperationsView.vue";
import RolloutDetailView from "../src/views/RolloutDetailView.vue";

function rollout(id: string, state: string, workspaceId = "workspace-a") {
  return {
    id,
    workspace_id: workspaceId,
    target_version: "0.2.0",
    state,
    batch_size: 5,
    stop_on_failure: true,
    reason: "rollout",
    created_by: "operator",
    current_batch: 0,
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    nodes: [],
  } as unknown as AgentRollout;
}

let unmount: (() => void) | undefined;

async function mount<T>(component: object): Promise<T> {
  const mounted = mountSetup(component);
  unmount = mounted.unmount;
  await flush();
  return mounted.view as T;
}

beforeEach(() => {
  vi.useFakeTimers();
  vi.resetAllMocks();
  vi.stubGlobal("window", new EventTarget());
  mocks.route = reactive({ params: { rolloutId: "rollout-a" } });
  mocks.getWorkspaceId.mockResolvedValue("workspace-a");
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
  it.each(["resolve", "reject"] as const)(
    "ignores an older read that %ss after the route changed",
    async (outcome) => {
      const slow = Promise.withResolvers<AgentRollout>();
      mocks.getAgentRollout.mockImplementation((id: string) =>
        id === "rollout-a" ? slow.promise : rollout("rollout-b", "running"),
      );
      const view = await mount<DetailView>(RolloutDetailView);
      mocks.route.params.rolloutId = "rollout-b";
      await flush();
      expect(view.rollout?.id).toBe("rollout-b");
      if (outcome === "resolve") slow.resolve(rollout("rollout-a", "paused"));
      else slow.reject(responseError(404, "{}"));
      await flush();
      expect(view.rollout?.id).toBe("rollout-b");
      expect(view.loading).toBe(false);
      expect(view.notFound).toBe(false);
      expect(view.unavailable).toBe(false);
    },
  );

  it("drops a read from an earlier route before returning to it", async () => {
    const first = Promise.withResolvers<AgentRollout>();
    mocks.getAgentRollout.mockReturnValueOnce(first.promise);
    mocks.getAgentRollout.mockReturnValue(new Promise(() => {}));
    const view = await mount<DetailView>(RolloutDetailView);
    mocks.route.params.rolloutId = "rollout-b";
    await nextTick();
    mocks.route.params.rolloutId = "rollout-a";
    await nextTick();
    first.resolve(rollout("rollout-a", "paused"));
    await flush();
    expect(view.rollout).toBeUndefined();
    expect(view.loading).toBe(true);
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
    mocks.getAgentRollout.mockRejectedValue(responseError(status, "{}"));
    const view = await mount<DetailView>(RolloutDetailView);
    expect(view[field]).toBe(true);
    expect(view.notFound).toBe(status === 404);
  });

  it("does not write a late resume into another rollout", async () => {
    const resume = Promise.withResolvers<AgentRollout>();
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
    const staleRead = Promise.withResolvers<AgentRollout>();
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

  it("blocks resending a lost resume until a fresh read confirms the pause", async () => {
    const followUp = Promise.withResolvers<AgentRollout>();
    mocks.getAgentRollout.mockResolvedValueOnce(rollout("rollout-a", "paused"));
    mocks.getAgentRollout.mockReturnValueOnce(followUp.promise);
    mocks.resumeAgentRollout.mockRejectedValue(new TypeError("network"));
    const view = await mount<DetailView>(RolloutDetailView);
    await view.resume();
    expect(view.resumeError).toBe("rolloutResumeUnconfirmed");
    expect(view.resumeBlocked).toBe(true);
    await view.resume();
    await flush();
    expect(mocks.resumeAgentRollout).toHaveBeenCalledTimes(1);
    expect(mocks.getAgentRollout).toHaveBeenCalledTimes(2);
    followUp.resolve(rollout("rollout-a", "paused"));
    await flush();
    expect(view.resumeBlocked).toBe(false);
    expect(view.rollout?.state).toBe("paused");
  });

  it("stops reading and writing after unmount", async () => {
    const read = Promise.withResolvers<AgentRollout>();
    mocks.getAgentRollout.mockReturnValue(read.promise);
    const view = await mount<DetailView>(RolloutDetailView);
    unmount?.();
    read.reject(new TypeError("network"));
    await flush();
    expect(view.unavailable).toBe(false);
    expect(vi.getTimerCount()).toBe(0);
  });

  it("stops foreground polling on terminal rollouts and resumes on active route change", async () => {
    mocks.getAgentRollout.mockImplementation((id: string) =>
      Promise.resolve(
        rollout(id, id === "rollout-a" ? "succeeded" : "running"),
      ),
    );
    const view = await mount<DetailView>(RolloutDetailView);
    expect(view.rollout?.state).toBe("succeeded");
    expect(vi.getTimerCount()).toBe(0);
    await vi.advanceTimersByTimeAsync(10_000);
    expect(mocks.getAgentRollout).toHaveBeenCalledTimes(1);

    mocks.route.params.rolloutId = "rollout-b";
    await nextTick();
    expect(view.rollout).toBeUndefined();
    await flush();
    expect(view.rollout?.id).toBe("rollout-b");
    expect(view.rollout?.state).toBe("running");
    expect(mocks.getAgentRollout).toHaveBeenLastCalledWith(
      "rollout-b",
      expect.any(AbortSignal),
    );
    expect(vi.getTimerCount()).toBe(1);
    await vi.advanceTimersByTimeAsync(2_000);
    expect(mocks.getAgentRollout).toHaveBeenCalledTimes(3);
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
      page: { has_more: false },
    });
  });

  it("refreshes both lists and drops an older rollout page", async () => {
    const slow = Promise.withResolvers<{ rollouts: AgentRollout[] }>();
    mocks.listAgentRollouts.mockResolvedValueOnce({
      rollouts: [rollout("rollout-a", "running")],
    });
    mocks.listAgentRollouts.mockReturnValueOnce(slow.promise);
    mocks.listAgentRollouts.mockResolvedValue({
      rollouts: [rollout("rollout-b", "running")],
    });
    const view = await mount<OperationsState>(OperationsView);
    expect(view.rollouts.map((item) => item.id)).toEqual(["rollout-a"]);
    view.refresh();
    await flush();
    expect(mocks.listAgentRollouts).toHaveBeenCalledTimes(2);
    expect(mocks.listOperations).toHaveBeenCalledTimes(2);
    view.refresh();
    await flush();
    slow.resolve({ rollouts: [rollout("rollout-a", "paused")] });
    await flush();
    expect(view.rollouts.map((item) => item.id)).toEqual(["rollout-b"]);
  });

  it("ignores an operation detail failure after unmount", async () => {
    const detail = Promise.withResolvers<unknown>();
    mocks.listAgentRollouts.mockResolvedValue({ rollouts: [] });
    mocks.getOperation.mockReturnValue(detail.promise);
    const view = await mount<OperationsState>(OperationsView);
    void view.inspectOperation("operation-a");
    await flush();
    unmount?.();
    detail.reject(new TypeError("network"));
    await flush();
    expect(view.detailError).toBe("");
  });
});
