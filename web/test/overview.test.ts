import { readFile } from "node:fs/promises";
import { resolve } from "node:path";

import type {
  NodeObservedState,
  Operation,
  OperationPage,
  PlatformEvent,
  PlatformEventPage,
} from "@ocservia/api-client";
import { createPinia, setActivePinia } from "pinia";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import {
  getWorkspace,
  listEvents,
  listNodes,
  listOperations,
  platformEventsEvent,
  workspaceContext,
  workspaceChangedEvent,
} from "../src/api/client";
import { useFleetStore } from "../src/shared/fleet";
import { useOverviewStore } from "../src/shared/overview";

vi.mock("../src/api/client", () => ({
  getWorkspace: vi.fn().mockResolvedValue({ id: "workspace" }),
  listEvents: vi.fn(),
  listNodes: vi.fn(),
  listOperations: vi.fn(),
  platformEventsEvent: "ocservia:platform-events",
  workspaceContext: vi.fn().mockReturnValue({ id: "workspace", generation: 1 }),
  workspaceChangedEvent: "ocservia:workspace-changed",
}));

const operation = (id: string, state: Operation["state"]): Operation => ({
  id,
  state,
  version: 1,
  createdAt: new Date(0),
  updatedAt: new Date(0),
});
const platformEvent = (id: string, suffix: number): PlatformEvent => ({
  id,
  nodeId: "019fc0a4-6d92-765c-a8a1-4af556614cc3",
  type: "heartbeat",
  traceparent: "00-trace-span-01",
  occurredAt: new Date(suffix),
});
const operationPage = (items: Operation[], hasMore = false): OperationPage => {
  const last = items.at(-1);
  return {
    items,
    page:
      hasMore && last
        ? { hasMore: true, nextCursor: last.id }
        : { hasMore: false },
  };
};
const eventPage = (
  items: PlatformEvent[],
  hasMore = false,
): PlatformEventPage => {
  const last = items.at(-1);
  return {
    items,
    page:
      hasMore && last
        ? { hasMore: true, nextCursor: last.id }
        : { hasMore: false },
  };
};
const node = (
  id: string,
  state: NodeObservedState["connectionState"],
  path?: "direct" | "relay",
  sessionCount = 0,
): NodeObservedState => ({
  id,
  name: `node-${id}`,
  version: 1,
  trustStatus: "active",
  connectionState: state,
  freshness: state === "online" ? "fresh" : "stale",
  dropped: { security: 0, health: 0, aggregate: 0, raw: 0 },
  sessionCount,
  ...(path
    ? { path: { mode: path, rttMs: 12 } satisfies NodeObservedState["path"] }
    : {}),
});

describe("operational overview", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    vi.stubGlobal("window", new EventTarget());
    setActivePinia(createPinia());
    vi.mocked(getWorkspace).mockReset();
    vi.mocked(listEvents).mockReset();
    vi.mocked(listNodes).mockReset();
    vi.mocked(listOperations).mockReset();
    vi.mocked(getWorkspace).mockResolvedValue({
      id: "workspace",
      name: "Workspace",
      slug: "workspace",
      version: 1,
    });
    vi.mocked(workspaceContext).mockReturnValue({
      id: "workspace",
      generation: 1,
    });
    vi.mocked(listOperations).mockResolvedValue(operationPage([]));
    vi.mocked(listEvents).mockResolvedValue(eventPage([]));
    vi.mocked(listNodes).mockResolvedValue({
      items: [],
      page: { hasMore: false },
    });
  });

  afterEach(() => {
    vi.mocked(workspaceContext).mockReturnValue({
      id: "workspace",
      generation: 1,
    });
    vi.unstubAllGlobals();
    vi.useRealTimers();
  });

  it("aggregates online, offline, relay, direct, and session counts", async () => {
    vi.mocked(listNodes).mockResolvedValue({
      items: [
        node("019fc0a4-6d92-765c-a8a1-4af556614cc1", "online", "direct", 3),
        node("019fc0a4-6d92-765c-a8a1-4af556614cc2", "online", "relay", 2),
        node("019fc0a4-6d92-765c-a8a1-4af556614cc3", "offline", undefined, 1),
      ],
      page: { hasMore: false },
    });
    const fleet = useFleetStore();
    await fleet.rebuild();

    expect(fleet.nodes).toHaveLength(3);
    expect(fleet.online).toBe(2);
    expect(fleet.offline).toBe(1);
    expect(fleet.direct).toBe(1);
    expect(fleet.relay).toBe(1);
    expect(fleet.sessionCount).toBe(6);
    fleet.$dispose();
  });

  it("classifies operation counts with polling terminal semantics", async () => {
    const states: Operation["state"][] = [
      "queued",
      "running",
      "offline_pending",
      "unknown",
      "succeeded",
      "failed",
      "failed",
      "failed",
      "expired",
      "rolled_back",
      "drifted",
      "superseded",
    ];
    vi.mocked(listOperations).mockResolvedValue(
      operationPage(
        states.map((state, index) => operation(`op-${String(index)}`, state)),
      ),
    );
    const overview = useOverviewStore();
    overview.start();
    await vi.advanceTimersByTimeAsync(0);

    expect(overview.activeOperations).toBe(3);
    expect(overview.unknownOperations).toBe(1);
    expect(overview.recentFailedOperations).toBe(3);
    overview.stop();
  });

  it("counts failed operations only inside the recent window", async () => {
    vi.mocked(listOperations).mockResolvedValue(
      operationPage(
        Array.from({ length: 24 }, (_, index) =>
          operation(
            `op-${String(index).padStart(2, "0")}`,
            index < 20 ? "succeeded" : "failed",
          ),
        ),
      ),
    );
    const overview = useOverviewStore();
    overview.start();
    await vi.advanceTimersByTimeAsync(0);

    expect(overview.recentFailedOperations).toBe(0);
    overview.stop();
  });

  it("bounds and orders recent events and operations", async () => {
    const newestFirst = Array.from({ length: 20 }, (_, index) =>
      platformEvent(
        `019fc0a4-6d92-765c-a8a1-4af556614e${String(19 - index).padStart(2, "0")}`,
        19 - index,
      ),
    );
    vi.mocked(listEvents).mockResolvedValue(eventPage(newestFirst));
    vi.mocked(listOperations).mockResolvedValue(
      operationPage(
        Array.from({ length: 20 }, (_, index) =>
          operation(`op-${String(index).padStart(2, "0")}`, "succeeded"),
        ),
      ),
    );
    const overview = useOverviewStore();
    overview.start();
    await vi.advanceTimersByTimeAsync(0);

    expect(listEvents).toHaveBeenCalledTimes(1);
    expect(listEvents).toHaveBeenCalledWith(
      undefined,
      expect.any(AbortSignal),
      "desc",
    );
    expect(overview.events).toHaveLength(20);
    expect(overview.events[0]?.id).toContain("e00");
    expect(overview.events.at(-1)?.id).toContain("e19");
    expect(overview.recentEvents).toHaveLength(12);
    expect(overview.recentEvents[0]?.id).toBe(overview.events.at(-1)?.id);
    expect(overview.recentOperations).toHaveLength(12);
    expect(overview.recentOperations[0]?.id).toBe("op-00");
    overview.stop();
  });

  it("does not walk history when the newest event page has more", async () => {
    vi.mocked(listEvents).mockResolvedValue(
      eventPage(
        [platformEvent("event-newest", 2), platformEvent("event-oldest", 1)],
        true,
      ),
    );
    const overview = useOverviewStore();
    overview.start();
    await vi.advanceTimersByTimeAsync(0);

    expect(listEvents).toHaveBeenCalledTimes(1);
    expect(overview.events.map((event) => event.id)).toEqual([
      "event-oldest",
      "event-newest",
    ]);
    overview.stop();
  });

  it("reads only the newest operations page on every refresh", async () => {
    vi.mocked(listOperations).mockResolvedValue(
      operationPage([operation("op-new", "running")], true),
    );
    const overview = useOverviewStore();
    overview.start();
    await vi.advanceTimersByTimeAsync(0);
    expect(listOperations).toHaveBeenCalledTimes(1);

    overview.refresh();
    await vi.advanceTimersByTimeAsync(0);

    expect(listOperations).toHaveBeenCalledTimes(2);
    expect(listOperations).toHaveBeenLastCalledWith(
      undefined,
      expect.any(AbortSignal),
    );
    expect(overview.activeOperations).toBe(1);
    overview.stop();
  });

  it("appends only new events on an incremental refresh", async () => {
    vi.mocked(listEvents)
      .mockResolvedValueOnce(
        eventPage([platformEvent("event-2", 2), platformEvent("event-1", 1)]),
      )
      .mockResolvedValueOnce(eventPage([platformEvent("event-3", 3)]));
    const overview = useOverviewStore();
    overview.start();
    await vi.advanceTimersByTimeAsync(0);
    expect(overview.events.map((event) => event.id)).toEqual([
      "event-1",
      "event-2",
    ]);

    overview.refresh();
    await vi.advanceTimersByTimeAsync(0);
    expect(listEvents).toHaveBeenLastCalledWith(
      "event-2",
      expect.any(AbortSignal),
    );
    expect(overview.events.map((event) => event.id)).toEqual([
      "event-1",
      "event-2",
      "event-3",
    ]);
    overview.stop();
  });

  it("keeps one source available when the other request fails", async () => {
    vi.mocked(listOperations).mockRejectedValue(new Error("unavailable"));
    vi.mocked(listEvents).mockResolvedValue(
      eventPage([platformEvent("event-1", 1)]),
    );
    const overview = useOverviewStore();
    overview.start();
    await vi.advanceTimersByTimeAsync(0);

    expect(overview.operationsUnavailable).toBe(true);
    expect(overview.operationsLoaded).toBe(false);
    expect(overview.eventsUnavailable).toBe(false);
    expect(overview.eventsLoaded).toBe(true);
    expect(overview.events).toHaveLength(1);
    overview.stop();
  });

  it("keeps previously loaded operations when a later events request fails", async () => {
    vi.mocked(listOperations).mockResolvedValue(
      operationPage([operation("op-1", "running")]),
    );
    vi.mocked(listEvents)
      .mockResolvedValueOnce(eventPage([]))
      .mockRejectedValueOnce(new Error("unavailable"));
    const overview = useOverviewStore();
    overview.start();
    await vi.advanceTimersByTimeAsync(0);
    expect(overview.activeOperations).toBe(1);

    overview.refresh();
    await vi.advanceTimersByTimeAsync(0);

    expect(overview.eventsUnavailable).toBe(true);
    expect(overview.operationsUnavailable).toBe(false);
    expect(overview.activeOperations).toBe(1);
    overview.stop();
  });

  it("treats an empty workspace as loaded, not unavailable", async () => {
    const overview = useOverviewStore();
    overview.start();
    await vi.advanceTimersByTimeAsync(0);

    expect(overview.operationsLoaded).toBe(true);
    expect(overview.eventsLoaded).toBe(true);
    expect(overview.operationsUnavailable).toBe(false);
    expect(overview.eventsUnavailable).toBe(false);
    expect(overview.recentOperations).toEqual([]);
    expect(overview.recentEvents).toEqual([]);
    overview.stop();
  });

  it("clears old workspace data and rejects late responses after a switch", async () => {
    let releaseWorkspaceA!: () => void;
    const gate = new Promise<void>((resolve) => {
      releaseWorkspaceA = resolve;
    });
    vi.mocked(listOperations).mockImplementationOnce(() =>
      gate.then(() => {
        throw new Error("late response");
      }),
    );
    const overview = useOverviewStore();
    overview.start();
    await vi.advanceTimersByTimeAsync(0);

    vi.mocked(workspaceContext).mockReturnValue({
      id: "other-workspace",
      generation: 2,
    });
    window.dispatchEvent(new Event(workspaceChangedEvent));
    expect(overview.operations).toEqual([]);
    expect(overview.events).toEqual([]);
    expect(overview.operationsLoaded).toBe(false);

    releaseWorkspaceA();
    await vi.advanceTimersByTimeAsync(0);
    expect(overview.operationsUnavailable).toBe(false);
    expect(listOperations).toHaveBeenCalledTimes(2);
    overview.stop();
  });

  it("refreshes operations and events when platform events arrive", async () => {
    const overview = useOverviewStore();
    overview.start();
    await vi.advanceTimersByTimeAsync(0);
    expect(listOperations).toHaveBeenCalledTimes(1);
    expect(listEvents).toHaveBeenCalledTimes(1);

    window.dispatchEvent(new Event(platformEventsEvent));
    expect(listOperations).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(500);
    expect(listOperations).toHaveBeenCalledTimes(2);
    expect(listEvents).toHaveBeenCalledTimes(2);
    overview.stop();
  });

  it("still refreshes while platform events arrive continuously", async () => {
    const overview = useOverviewStore();
    overview.start();
    await vi.advanceTimersByTimeAsync(0);
    expect(listOperations).toHaveBeenCalledTimes(1);

    // Events every 150ms forever postpone a trailing debounce; the fixed
    // refresh window must still fire mid-burst.
    for (let elapsed = 0; elapsed < 1_400; elapsed += 150) {
      window.dispatchEvent(new Event(platformEventsEvent));
      await vi.advanceTimersByTimeAsync(150);
    }

    expect(vi.mocked(listOperations).mock.calls.length).toBeGreaterThanOrEqual(
      3,
    );
    expect(vi.mocked(listEvents).mock.calls.length).toBeGreaterThanOrEqual(3);
    overview.stop();
  });

  it("stops refreshing and clears state when the overview unmounts", async () => {
    const overview = useOverviewStore();
    overview.start();
    await vi.advanceTimersByTimeAsync(0);
    overview.stop();

    window.dispatchEvent(new Event(platformEventsEvent));
    await vi.advanceTimersByTimeAsync(1_000);
    window.dispatchEvent(new Event(workspaceChangedEvent));
    await vi.advanceTimersByTimeAsync(1_000);

    expect(listOperations).toHaveBeenCalledTimes(1);
    expect(listEvents).toHaveBeenCalledTimes(1);
    expect(overview.events).toEqual([]);
    expect(overview.eventsLoaded).toBe(false);
  });
});

describe("production overview isolation", () => {
  it("does not use development simulator state in the production view", async () => {
    const source = await readFile(
      resolve(import.meta.dirname, "../src/views/OverviewView.vue"),
      "utf8",
    );
    expect(source).not.toContain("useLocalSliceStore");
    expect(source).not.toContain("SimulationScenario");
    expect(source).not.toContain("createLocalSimulation");
    expect(source).not.toContain("run-probe");
  });

  it("keeps the simulator available on the gated development page", async () => {
    const source = await readFile(
      resolve(import.meta.dirname, "../src/views/DevelopmentView.vue"),
      "utf8",
    );
    expect(source).toContain("useLocalSliceStore");
    expect(source).toContain('data-testid="run-probe"');
  });
});
