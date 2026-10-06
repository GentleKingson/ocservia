import { createPinia, setActivePinia } from "pinia";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { eventStreamPath, listEvents } from "../src/api/events";
import { listNodes } from "../src/api/nodes";
import { useFleetStore } from "../src/shared/fleet";

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
  constructor(readonly url: string) {
    super();
    FakeEventSource.instances.push(this);
  }
  close(): void {}
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
});
