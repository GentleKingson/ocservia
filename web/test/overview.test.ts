import type {
  NodeObservedState,
  Operation,
  OperationPage,
  PlatformEvent,
  PlatformEventPage,
} from "@ocservia/api-client";
import { createPinia, setActivePinia } from "pinia";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { getWorkspace, workspaceContext } from "../src/api/workspace";
import { listEvents, platformEventsEvent } from "../src/api/events";
import { listNodes } from "../src/api/nodes";
import { listOperations, operationSummary } from "../src/api/operations";
import { useFleetStore } from "../src/shared/fleet";
import { useOverviewStore } from "../src/shared/overview";
import {
  sourceState,
  sourceValue,
} from "../src/features/overview/source-state";

vi.mock("../src/api/workspace", async (original) => ({
  ...(await original<typeof import("../src/api/workspace")>()),
  getWorkspace: vi.fn().mockResolvedValue({ id: "workspace" }),
  workspaceContext: vi.fn().mockReturnValue({ id: "workspace", generation: 1 }),
}));
vi.mock("../src/api/events", () => ({
  listEvents: vi.fn(),
  platformEventsEvent: "ocservia:platform-events",
}));
vi.mock("../src/api/nodes", () => ({
  listNodes: vi.fn(),
}));
vi.mock("../src/api/operations", () => ({
  listOperations: vi.fn(),
  operationSummary: vi.fn(),
}));

const operation = (id: string, state: Operation["state"]): Operation => ({
  id,
  state,
  version: 1,
  createdAt: new Date(0).toISOString(),
  updatedAt: new Date(0).toISOString(),
});
const platformEvent = (id: string, suffix: number): PlatformEvent => ({
  id,
  nodeId: "019fc0a4-6d92-765c-a8a1-4af556614cc3",
  type: "heartbeat",
  traceparent: "00-trace-span-01",
  occurredAt: new Date(suffix).toISOString(),
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
    vi.resetAllMocks();
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
    vi.mocked(operationSummary).mockResolvedValue({ active: 0, unknown: 0 });
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

  it("serves workspace-wide totals from the summary and counts failed operations in the recent window", async () => {
    // Summary totals can exceed the newest page, while failed count stays
    // bounded to the 20-operation recent window.
    const items = Array.from({ length: 24 }, (_, index) =>
      operation(
        `op-${String(index).padStart(2, "0")}`,
        index < 3 || index >= 20 ? "failed" : "succeeded",
      ),
    );
    vi.mocked(listOperations).mockResolvedValue(operationPage(items, true));
    vi.mocked(operationSummary).mockResolvedValue({
      active: 300,
      unknown: 100,
    });
    const overview = useOverviewStore();
    overview.start();
    await vi.advanceTimersByTimeAsync(0);

    expect(overview.activeOperations).toBe(300);
    expect(overview.unknownOperations).toBe(100);
    expect(overview.recentFailedOperations).toBe(3);
    overview.stop();
  });

  it("bounds and orders recent events and operations without walking history", async () => {
    const newestFirst = Array.from({ length: 20 }, (_, index) =>
      platformEvent(
        `019fc0a4-6d92-765c-a8a1-4af556614e${String(19 - index).padStart(2, "0")}`,
        19 - index,
      ),
    );
    vi.mocked(listEvents).mockResolvedValue(eventPage(newestFirst, true));
    vi.mocked(listOperations).mockResolvedValue(
      operationPage(
        Array.from({ length: 20 }, (_, index) =>
          operation(`op-${String(index).padStart(2, "0")}`, "succeeded"),
        ),
        true,
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

  it("reads only the newest operations page on every refresh", async () => {
    vi.mocked(listOperations).mockResolvedValue(
      operationPage([operation("op-new", "running")], true),
    );
    vi.mocked(operationSummary).mockResolvedValue({ active: 1, unknown: 0 });
    const overview = useOverviewStore();
    overview.start();
    await vi.advanceTimersByTimeAsync(0);
    expect(listOperations).toHaveBeenCalledTimes(1);
    expect(operationSummary).toHaveBeenCalledTimes(1);

    overview.refresh();
    await vi.advanceTimersByTimeAsync(0);

    expect(listOperations).toHaveBeenCalledTimes(2);
    expect(listOperations).toHaveBeenLastCalledWith(
      undefined,
      expect.any(AbortSignal),
    );
    expect(operationSummary).toHaveBeenCalledTimes(2);
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
    vi.mocked(operationSummary).mockResolvedValue({ active: 1, unknown: 0 });
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

  it("keeps stale operations and their load time after a failed refresh", async () => {
    vi.mocked(listOperations).mockResolvedValue(
      operationPage([operation("op-1", "unknown")]),
    );
    vi.mocked(operationSummary).mockResolvedValue({ active: 0, unknown: 1 });
    const overview = useOverviewStore();
    overview.start();
    await vi.advanceTimersByTimeAsync(0);
    const loadedAt = overview.operationsAt;
    expect(loadedAt).toBeInstanceOf(Date);
    expect(
      sourceState(overview.operationsLoaded, overview.operationsUnavailable),
    ).toBe("ready");

    vi.mocked(listOperations).mockRejectedValueOnce(new Error("offline"));
    await vi.advanceTimersByTimeAsync(1_000);
    overview.refresh();
    await vi.advanceTimersByTimeAsync(0);

    const state = sourceState(
      overview.operationsLoaded,
      overview.operationsUnavailable,
    );
    expect(state).toBe("stale");
    expect(sourceValue(state, overview.unknownOperations)).toBe("1");
    expect(overview.recentOperations).toHaveLength(1);
    expect(overview.operationsAt).toBe(loadedAt);
    overview.stop();
    expect(overview.operationsAt).toBeUndefined();
  });

  it("records the node snapshot time only on a complete rebuild", async () => {
    vi.mocked(listNodes).mockResolvedValue({
      items: [node("019fc0a4-6d92-765c-a8a1-4af556614cc3", "offline")],
      page: { hasMore: false },
    });
    const fleet = useFleetStore();
    await fleet.rebuild();
    const snapshotAt = fleet.snapshotAt;
    expect(snapshotAt).toBeInstanceOf(Date);

    vi.mocked(listNodes).mockRejectedValueOnce(new Error("offline"));
    await fleet.rebuild();
    expect(sourceState(fleet.initialized, fleet.unavailable)).toBe("stale");
    expect(fleet.snapshotAt).toBe(snapshotAt);
    expect(fleet.nodes[0]?.freshness).toBe("stale");
    fleet.$dispose();
  });

  it("separates loading, unavailable, stale and ready sources", () => {
    expect(sourceState(false, false)).toBe("loading");
    expect(sourceState(false, true)).toBe("unavailable");
    expect(sourceState(true, true)).toBe("stale");
    expect(sourceState(true, false)).toBe("ready");
    expect(sourceValue("loading", 3)).toBe("…");
    expect(sourceValue("unavailable", 3)).toBe("–");
    expect(sourceValue("stale", 0)).toBe("0");
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

  it("follows stream health for 60-second supplementation and 15-second recovery", async () => {
    const fleet = useFleetStore();
    fleet.streamConnected = true;
    const overview = useOverviewStore();
    overview.start();
    await vi.advanceTimersByTimeAsync(59_999);
    expect(listOperations).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(listOperations).toHaveBeenCalledTimes(2);
    fleet.streamConnected = false;
    await vi.advanceTimersByTimeAsync(0);
    await vi.advanceTimersByTimeAsync(15_000);
    expect(listOperations).toHaveBeenCalledTimes(3);
    overview.stop();
  });

  it("stops refreshing and clears state when the overview unmounts", async () => {
    const overview = useOverviewStore();
    overview.start();
    await vi.advanceTimersByTimeAsync(0);
    overview.stop();

    window.dispatchEvent(new Event(platformEventsEvent));
    await vi.advanceTimersByTimeAsync(1_000);

    expect(listOperations).toHaveBeenCalledTimes(1);
    expect(listEvents).toHaveBeenCalledTimes(1);
    expect(overview.events).toEqual([]);
    expect(overview.eventsLoaded).toBe(false);
  });
});
