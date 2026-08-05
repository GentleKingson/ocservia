import type {
  NodeObservedState,
  NodeSession,
  Operation,
} from "@ocservia/api-client";
import { createPinia, setActivePinia } from "pinia";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import {
  disconnectSession,
  getNode,
  getOperation,
  listNodeIpBans,
  listNodeSessions,
} from "../src/api/client";
import { useFleetStore } from "../src/shared/fleet";

vi.mock("../src/api/client", () => ({
  disconnectSession: vi.fn(),
  getNode: vi.fn(),
  getOperation: vi.fn(),
  listNodeIpBans: vi.fn(),
  listNodeSessions: vi.fn(),
  listNodes: vi.fn(),
  reloadService: vi.fn(),
  removeIpBan: vi.fn(),
  terminateSession: vi.fn(),
}));

const node: NodeObservedState = {
  id: "019fc0a4-6d92-765c-a8a1-4af556614cc3",
  name: "node",
  version: 4,
  trustStatus: "active",
  connectionState: "online",
  freshness: "fresh",
  bootId: "019fc0a4-6d92-765c-a8a1-4af556614cc4",
  dropped: { security: 0, health: 0, aggregate: 0, raw: 0 },
  sessionCount: 1,
};
const session: NodeSession = {
  id: "42",
  username: "alice",
  clientIp: "192.0.2.10",
  connectedAt: new Date(0),
  bytesIn: 0,
  bytesOut: 0,
};
const operation = (state: Operation["state"]): Operation => ({
  id: "019fc0a4-6d92-765c-a8a1-4af556614cc5",
  state,
  version: 1,
  createdAt: new Date(0),
  updatedAt: new Date(0),
});

describe("controlled fleet operations", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    setActivePinia(createPinia());
    vi.mocked(getNode).mockResolvedValue(node);
    vi.mocked(listNodeIpBans).mockResolvedValue({ items: [] });
    vi.mocked(listNodeSessions)
      .mockResolvedValueOnce({ items: [session], page: { hasMore: false } })
      .mockResolvedValueOnce({ items: [], page: { hasMore: false } });
  });

  afterEach(() => vi.useRealTimers());

  it("keeps observed state until the asynchronous operation succeeds", async () => {
    vi.mocked(disconnectSession).mockResolvedValue(operation("queued"));
    vi.mocked(getOperation).mockResolvedValue(operation("succeeded"));
    const store = useFleetStore();
    await store.select(node.id);

    const completion = store.disconnectSession(session.id, "support case");
    await vi.advanceTimersByTimeAsync(0);
    expect(store.sessions).toEqual([session]);
    expect(store.latestOperation?.state).toBe("queued");

    await vi.advanceTimersByTimeAsync(750);
    await completion;
    expect(store.latestOperation?.state).toBe("succeeded");
    expect(store.sessions).toEqual([]);
  });
});
