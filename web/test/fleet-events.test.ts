import { createPinia, setActivePinia } from "pinia";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { eventStreamPath, listEvents } from "../src/api/events";
import {
  getNode,
  listNodeSessions,
  listNodeIpBans,
  listNodeUserGroupState,
  listNodes,
} from "../src/api/nodes";
import { workspaceChangedEvent } from "../src/api/workspace";
import { useFleetStore } from "../src/shared/fleet";

vi.mock("../src/api/platform", () => ({
  probeAuthentication: vi.fn().mockResolvedValue(undefined),
}));

vi.mock("../src/api/events", () => ({
  eventStreamPath: vi.fn(),
  listEvents: vi.fn(),
  platformEventsEvent: "ocservia:platform-events",
}));
vi.mock("../src/api/workspace", () => ({
  getWorkspace: vi.fn().mockResolvedValue({ id: "workspace" }),
  workspaceContext: vi.fn().mockReturnValue({ id: "workspace", generation: 1 }),
  workspaceChangedEvent: "ocservia:workspace-changed",
}));
vi.mock("../src/api/nodes", () => ({
  getNode: vi.fn(),
  listNodeIpBans: vi.fn(),
  listNodeUserGroupState: vi.fn(),
  listNodeSessions: vi.fn(),
  listNodes: vi.fn(),
}));

class FakeEventSource extends EventTarget {
  static instances: FakeEventSource[] = [];
  onerror: (() => void) | null = null;
  onopen: (() => void) | null = null;
  constructor(readonly url: string) {
    super();
    FakeEventSource.instances.push(this);
  }
  close = vi.fn();
}

const latestEvent = "019fc0a4-6d92-765c-a8a1-4af556614cc7";
const streamedEvent = "019fc0a4-6d92-765c-a8a1-4af556614cc8";

describe("fleet event stream", () => {
  beforeEach(() => {
    setActivePinia(createPinia());
    FakeEventSource.instances = [];
    vi.stubGlobal("EventSource", FakeEventSource);
    vi.mocked(listNodes).mockResolvedValue({
      items: [],
      page: { hasMore: false },
    });
    vi.mocked(listEvents).mockResolvedValue({
      items: [{ id: latestEvent }],
      page: { hasMore: true },
    } as never);
    vi.mocked(eventStreamPath).mockImplementation((after) =>
      Promise.resolve(after ? `/stream?after=${after}` : "/stream"),
    );
  });

  afterEach(() => {
    vi.useRealTimers();
    vi.unstubAllGlobals();
    vi.clearAllMocks();
  });

  it("resumes after the snapshot instead of replaying history", async () => {
    const store = useFleetStore();
    await store.rebuild();
    await store.connect();

    expect(listEvents).toHaveBeenCalledWith(
      undefined,
      expect.any(AbortSignal),
      "desc",
    );
    expect(FakeEventSource.instances.at(-1)?.url).toBe(
      `/stream?after=${latestEvent}`,
    );

    // Returning to a fleet view reconnects after the last delivered event.
    FakeEventSource.instances
      .at(-1)
      ?.dispatchEvent(
        new MessageEvent("platform", { lastEventId: streamedEvent }),
      );
    await store.connect();

    expect(FakeEventSource.instances.at(-1)?.url).toBe(
      `/stream?after=${streamedEvent}`,
    );
    expect(listEvents).toHaveBeenCalledTimes(1);
    store.disconnect();
  });

  it("suspends the stream in the background and recovers a stale snapshot on focus", async () => {
    vi.useFakeTimers();
    let focused = true;
    vi.stubGlobal("window", new EventTarget());
    vi.stubGlobal(
      "document",
      Object.assign(new EventTarget(), {
        visibilityState: "visible",
        hasFocus: () => focused,
      }),
    );
    const store = useFleetStore();
    await store.rebuild();
    await store.connect();
    const oldStream = FakeEventSource.instances.at(-1);
    if (!oldStream) throw new Error("Missing event stream");
    vi.mocked(listNodes).mockRejectedValueOnce(new Error("offline"));
    await store.rebuild();
    expect(store.unavailable).toBe(true);
    focused = false;
    window.dispatchEvent(new Event("blur"));
    expect(oldStream.close).toHaveBeenCalled();
    const calls = vi.mocked(listNodes).mock.calls.length;
    oldStream.dispatchEvent(new MessageEvent("platform"));
    await vi.advanceTimersByTimeAsync(60_000);
    expect(listNodes).toHaveBeenCalledTimes(calls);
    focused = true;
    window.dispatchEvent(new Event("focus"));
    document.dispatchEvent(new Event("visibilitychange"));
    await vi.advanceTimersByTimeAsync(0);
    expect(listNodes).toHaveBeenCalledTimes(calls + 1);
    expect(store.unavailable).toBe(false);
    expect(FakeEventSource.instances).toHaveLength(2);
    await vi.advanceTimersByTimeAsync(15_000);
    expect(listNodes).toHaveBeenCalledTimes(calls + 2);
    store.$dispose();
    window.dispatchEvent(new Event("focus"));
    await vi.advanceTimersByTimeAsync(60_000);
    expect(listNodes).toHaveBeenCalledTimes(calls + 2);
  });

  it("does not report a cancelled background read or reconnect after disposal", async () => {
    vi.useFakeTimers();
    let focused = true;
    vi.stubGlobal("window", new EventTarget());
    vi.stubGlobal(
      "document",
      Object.assign(new EventTarget(), {
        visibilityState: "visible",
        hasFocus: () => focused,
      }),
    );
    const store = useFleetStore();
    await store.rebuild();
    await store.connect();
    vi.mocked(listNodes).mockImplementationOnce(
      (_cursor, signal) =>
        new Promise((_resolve, reject) => {
          signal?.addEventListener("abort", () => {
            reject(new DOMException("Aborted", "AbortError"));
          });
        }),
    );
    await vi.advanceTimersByTimeAsync(15_000);
    expect(store.loading).toBe(true);
    focused = false;
    window.dispatchEvent(new Event("blur"));
    store.$dispose();
    focused = true;
    window.dispatchEvent(new Event("focus"));
    await vi.advanceTimersByTimeAsync(60_000);
    expect(store.unavailable).toBe(false);
    expect(FakeEventSource.instances).toHaveLength(1);
  });

  it("polls healthy streams at 60 seconds and reconnects failed streams after 15 seconds", async () => {
    vi.useFakeTimers();
    const store = useFleetStore();
    store.start();
    await vi.advanceTimersByTimeAsync(0);
    const stream = FakeEventSource.instances.at(-1);
    if (!stream) throw new Error("Missing stream");
    stream.onopen?.();
    expect(store.streamConnected).toBe(true);
    await vi.advanceTimersByTimeAsync(59_999);
    expect(listNodes).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(listNodes).toHaveBeenCalledTimes(2);
    stream.onerror?.();
    expect(store.streamConnected).toBe(false);
    expect(stream.close).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(14_999);
    expect(listNodes).toHaveBeenCalledTimes(2);
    await vi.advanceTimersByTimeAsync(1);
    expect(listNodes).toHaveBeenCalledTimes(3);
    expect(FakeEventSource.instances).toHaveLength(2);
    stream.onopen?.(); // A late callback from the closed stream is ignored.
    expect(store.streamConnected).toBe(false);
    FakeEventSource.instances.at(-1)?.onopen?.();
    await vi.advanceTimersByTimeAsync(60_000);
    expect(listNodes).toHaveBeenCalledTimes(4);
    store.$dispose();
  });

  it("stops all page reads and clears detail selection until a view starts again", async () => {
    vi.useFakeTimers();
    vi.stubGlobal("window", new EventTarget());
    vi.mocked(getNode).mockResolvedValue({ id: "node" } as never);
    vi.mocked(listNodeSessions).mockResolvedValue({
      items: [],
      page: { hasMore: false },
    });
    vi.mocked(listNodeIpBans).mockResolvedValue({ items: [] });
    vi.mocked(listNodeUserGroupState).mockResolvedValue({ items: [] });
    const store = useFleetStore();
    store.start();
    await vi.advanceTimersByTimeAsync(0);
    await store.select("node");
    const stream = FakeEventSource.instances.at(-1);
    store.stop();
    expect(store.selected).toBeUndefined();
    stream?.dispatchEvent(new MessageEvent("platform"));
    window.dispatchEvent(new Event(workspaceChangedEvent));
    await vi.advanceTimersByTimeAsync(120_000);
    expect(listNodes).toHaveBeenCalledTimes(1);
    expect(getNode).toHaveBeenCalledTimes(1);
    store.start();
    await vi.advanceTimersByTimeAsync(0);
    expect(listNodes).toHaveBeenCalledTimes(2);
    expect(getNode).toHaveBeenCalledTimes(1);
    store.$dispose();
  });

  it("does not overlap event reads or duplicate a nearby periodic read", async () => {
    vi.useFakeTimers();
    const store = useFleetStore();
    store.start();
    await vi.advanceTimersByTimeAsync(0);
    const stream = FakeEventSource.instances.at(-1);
    stream?.onopen?.();
    await vi.advanceTimersByTimeAsync(59_900);
    let finish!: () => void;
    let signal: AbortSignal | undefined;
    vi.mocked(listNodes).mockImplementationOnce((_cursor, requestSignal) => {
      signal = requestSignal;
      return new Promise((resolve) => {
        finish = () => {
          resolve({ items: [], page: { hasMore: false } });
        };
      });
    });
    stream?.dispatchEvent(new MessageEvent("platform"));
    await vi.advanceTimersByTimeAsync(100);
    for (let index = 0; index < 5; index += 1) {
      stream?.dispatchEvent(new MessageEvent("platform"));
      await vi.advanceTimersByTimeAsync(150);
    }
    expect(listNodes).toHaveBeenCalledTimes(2);
    expect(signal?.aborted).toBe(false);
    finish();
    await vi.advanceTimersByTimeAsync(150);
    expect(listNodes).toHaveBeenCalledTimes(3);
    store.$dispose();
  });
  it("does not reopen a stream when navigation races connection setup", async () => {
    vi.useFakeTimers();
    let finish!: (path: string) => void;
    vi.mocked(eventStreamPath).mockImplementationOnce(
      () =>
        new Promise<string>((resolve) => {
          finish = resolve;
        }),
    );
    const store = useFleetStore();
    store.start();
    await vi.advanceTimersByTimeAsync(0);
    store.stop();
    finish("/stream");
    await vi.advanceTimersByTimeAsync(120_000);
    expect(FakeEventSource.instances).toHaveLength(0);
    expect(listNodes).toHaveBeenCalledTimes(1);
    store.$dispose();
  });
});
