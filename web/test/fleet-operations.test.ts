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
  listNodeUserGroupState,
  listNodes,
  listNodeSessions,
} from "../src/api/client";
import { useFleetStore } from "../src/shared/fleet";

vi.mock("../src/api/client", () => ({
  disconnectSession: vi.fn(),
  getWorkspace: vi.fn().mockResolvedValue({ id: "workspace" }),
  getNode: vi.fn(),
  getOperation: vi.fn(),
  listNodeIpBans: vi.fn(),
  listNodeUserGroupState: vi.fn(),
  listNodeSessions: vi.fn(),
  listNodes: vi.fn(),
  reloadService: vi.fn(),
  removeIpBan: vi.fn(),
  terminateSession: vi.fn(),
  workspaceContext: vi.fn().mockReturnValue({ id: "workspace", generation: 1 }),
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

function deferred<T>(): {
  promise: Promise<T>;
  resolve: (value: T) => void;
} {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((next) => {
    resolve = next;
  });
  return { promise, resolve };
}

const nodeB: NodeObservedState = {
  ...node,
  id: "019fc0a4-6d92-765c-a8a1-4af556614cc6",
  name: "node-b",
};

const nodePage = (item: NodeObservedState) => ({
  items: [item],
  page: { hasMore: false },
});

describe("controlled fleet operations", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    setActivePinia(createPinia());
    vi.mocked(getNode).mockReset();
    vi.mocked(getOperation).mockReset();
    vi.mocked(listNodeIpBans).mockReset();
    vi.mocked(listNodeUserGroupState).mockReset();
    vi.mocked(listNodes).mockReset();
    vi.mocked(listNodeSessions).mockReset();
    vi.mocked(disconnectSession).mockReset();
    vi.mocked(getNode).mockResolvedValue(node);
    vi.mocked(listNodeIpBans).mockResolvedValue({ items: [] });
    vi.mocked(listNodeUserGroupState).mockResolvedValue({ items: [] });
    vi.mocked(listNodeSessions).mockResolvedValue({
      items: [],
      page: { hasMore: false },
    });
  });

  afterEach(() => vi.useRealTimers());

  it("keeps observed state until the asynchronous operation succeeds", async () => {
    vi.mocked(disconnectSession).mockResolvedValue(operation("queued"));
    vi.mocked(getOperation).mockResolvedValue(operation("succeeded"));
    vi.mocked(listNodeSessions)
      .mockResolvedValueOnce({ items: [session], page: { hasMore: false } })
      .mockResolvedValueOnce({ items: [], page: { hasMore: false } });
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

  it("does not let an older rebuild overwrite a newer same-workspace snapshot", async () => {
    const first = deferred<ReturnType<typeof nodePage>>();
    const second = deferred<ReturnType<typeof nodePage>>();
    vi.mocked(listNodes)
      .mockImplementationOnce(() => first.promise)
      .mockImplementationOnce(() => second.promise);
    const store = useFleetStore();

    const staleRebuild = store.rebuild();
    await Promise.resolve();
    await Promise.resolve();
    const currentRebuild = store.rebuild();
    await Promise.resolve();
    await Promise.resolve();

    second.resolve(nodePage(nodeB));
    await currentRebuild;
    first.resolve(nodePage(node));
    await staleRebuild;

    expect(store.nodes).toEqual([nodeB]);
    store.$dispose();
  });

  it("keeps the latest node selection when an earlier detail request returns last", async () => {
    const first = deferred<NodeObservedState>();
    const second = deferred<NodeObservedState>();
    vi.mocked(getNode).mockImplementation((nodeId) =>
      nodeId === "node-a" ? first.promise : second.promise,
    );
    const store = useFleetStore();

    const staleSelection = store.select("node-a");
    await Promise.resolve();
    await Promise.resolve();
    const currentSelection = store.select("node-b");
    await Promise.resolve();
    await Promise.resolve();

    second.resolve(nodeB);
    await currentSelection;
    first.resolve(node);
    await staleSelection;

    expect(store.selected?.id).toBe(nodeB.id);
    expect(store.selected?.name).toBe(nodeB.name);
    store.$dispose();
  });

  it("does not refresh the operated node over a selection that is still loading", async () => {
    const nodeBDetails = deferred<NodeObservedState>();
    const operationResult = deferred<Operation>();
    vi.mocked(getNode).mockImplementation((nodeId) =>
      nodeId === nodeB.id ? nodeBDetails.promise : Promise.resolve(node),
    );
    vi.mocked(disconnectSession).mockResolvedValue(operation("queued"));
    vi.mocked(getOperation).mockReturnValue(operationResult.promise);
    const store = useFleetStore();
    await store.select(node.id);

    const completion = store.disconnectSession(session.id, "support case");
    await vi.advanceTimersByTimeAsync(750);
    const selection = store.select(nodeB.id);
    await Promise.resolve();
    await Promise.resolve();

    operationResult.resolve(operation("succeeded"));
    await completion;
    nodeBDetails.resolve(nodeB);
    await selection;

    expect(store.selected?.id).toBe(nodeB.id);
    expect(store.selected?.name).toBe(nodeB.name);
    store.$dispose();
  });
});
