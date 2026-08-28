import type {
  NodeObservedState,
  NodeSession,
  Operation,
} from "@ocservia/api-client";
import { createPinia, setActivePinia } from "pinia";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import {
  createUser,
  disconnectSession,
  upgradeNodeAgent,
  getNode,
  getOperation,
  listNodeIpBans,
  listNodeUserGroupState,
  listNodes,
  listNodeSessions,
  workspaceContext,
  workspaceChangedEvent,
} from "../src/api/client";
import { useFleetStore } from "../src/shared/fleet";

vi.mock("../src/api/client", () => ({
  applyGroup: vi.fn(),
  createUser: vi.fn(),
  disableUser: vi.fn(),
  disconnectSession: vi.fn(),
  enableUser: vi.fn(),
  getWorkspace: vi.fn().mockResolvedValue({ id: "workspace" }),
  getNode: vi.fn(),
  getOperation: vi.fn(),
  listNodeIpBans: vi.fn(),
  listNodeUserGroupState: vi.fn(),
  listNodeSessions: vi.fn(),
  listNodes: vi.fn(),
  reloadService: vi.fn(),
  removeIpBan: vi.fn(),
  rotateUserPassword: vi.fn(),
  terminateSession: vi.fn(),
  upgradeNodeAgent: vi.fn(),
  workspaceContext: vi.fn().mockReturnValue({ id: "workspace", generation: 1 }),
  workspaceChangedEvent: "ocservia:workspace-changed",
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
    vi.mocked(upgradeNodeAgent).mockReset();
    vi.mocked(createUser).mockReset();
    vi.mocked(workspaceContext).mockReturnValue({
      id: "workspace",
      generation: 1,
    });
    vi.mocked(getNode).mockResolvedValue(node);
    vi.mocked(listNodeIpBans).mockResolvedValue({ items: [] });
    vi.mocked(listNodeUserGroupState).mockResolvedValue({ items: [] });
    vi.mocked(listNodeSessions).mockResolvedValue({
      items: [],
      page: { hasMore: false },
    });
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    vi.useRealTimers();
  });

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

  it("retries a failed create at the current desired version", async () => {
    vi.mocked(createUser).mockResolvedValue(operation("succeeded"));
    const store = useFleetStore();
    await store.select(node.id);

    await store.createUser(
      "alice",
      1,
      "sealed-password",
      "node-key-1",
      "retry failed create",
    );

    expect(createUser).toHaveBeenCalledWith(
      node.id,
      "alice",
      1,
      "sealed-password",
      "node-key-1",
      "retry failed create",
      expect.any(AbortSignal),
    );
    store.$dispose();
  });

  it("continues polling from unknown until the operation succeeds", async () => {
    vi.mocked(disconnectSession).mockResolvedValue(operation("queued"));
    vi.mocked(getOperation)
      .mockResolvedValueOnce(operation("unknown"))
      .mockResolvedValueOnce(operation("succeeded"));
    vi.mocked(listNodeSessions)
      .mockResolvedValueOnce({ items: [session], page: { hasMore: false } })
      .mockResolvedValueOnce({ items: [], page: { hasMore: false } });
    const store = useFleetStore();
    await store.select(node.id);

    const completion = store.disconnectSession(session.id, "support case");
    await vi.advanceTimersByTimeAsync(750);
    expect(store.latestOperation?.state).toBe("unknown");
    await vi.advanceTimersByTimeAsync(1_500);
    await completion;

    expect(store.latestOperation?.state).toBe("succeeded");
    expect(store.sessions).toEqual([]);
    expect(getOperation).toHaveBeenCalledTimes(2);
    store.$dispose();
  });

  it("continues polling from unknown until the operation fails", async () => {
    vi.mocked(disconnectSession).mockResolvedValue(operation("queued"));
    vi.mocked(getOperation)
      .mockResolvedValueOnce(operation("unknown"))
      .mockResolvedValueOnce(operation("failed"));
    const store = useFleetStore();
    await store.select(node.id);

    const completion = store.disconnectSession(session.id, "support case");
    await vi.advanceTimersByTimeAsync(750);
    expect(store.latestOperation?.state).toBe("unknown");
    await vi.advanceTimersByTimeAsync(1_500);
    await completion;

    expect(store.latestOperation?.state).toBe("failed");
    expect(getOperation).toHaveBeenCalledTimes(2);
    store.$dispose();
  });

  it("backs off bounded unknown polling without overlapping requests", async () => {
    vi.mocked(disconnectSession).mockResolvedValue(operation("queued"));
    vi.mocked(getOperation).mockResolvedValue(operation("unknown"));
    const store = useFleetStore();
    await store.select(node.id);

    const completion = store.disconnectSession(session.id, "support case");
    await vi.advanceTimersByTimeAsync(750);
    expect(getOperation).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(1_500);
    expect(getOperation).toHaveBeenCalledTimes(2);
    await vi.advanceTimersByTimeAsync(3_000);
    expect(getOperation).toHaveBeenCalledTimes(3);
    await vi.advanceTimersByTimeAsync(4_999);
    expect(getOperation).toHaveBeenCalledTimes(3);
    await vi.advanceTimersByTimeAsync(1);
    expect(getOperation).toHaveBeenCalledTimes(4);

    store.$dispose();
    await completion;
  });

  it("cancels unknown polling when the selected node changes", async () => {
    let pollingSignal: AbortSignal | undefined;
    vi.mocked(disconnectSession)
      .mockResolvedValueOnce(operation("queued"))
      .mockResolvedValueOnce(operation("succeeded"));
    vi.mocked(getOperation).mockImplementation((_operationId, signal) => {
      pollingSignal = signal;
      return Promise.resolve(operation("unknown"));
    });
    vi.mocked(getNode).mockImplementation((nodeId) =>
      Promise.resolve(nodeId === nodeB.id ? nodeB : node),
    );
    const store = useFleetStore();
    await store.select(node.id);

    const completion = store.disconnectSession(session.id, "support case");
    await vi.advanceTimersByTimeAsync(750);
    expect(store.latestOperation?.state).toBe("unknown");
    await store.select(nodeB.id);
    await completion;
    await store.disconnectSession(session.id, "node b support case");

    expect(pollingSignal?.aborted).toBe(true);
    expect(store.selected?.id).toBe(nodeB.id);
    expect(store.latestOperation?.state).toBe("succeeded");
    expect(disconnectSession).toHaveBeenCalledTimes(2);
    store.$dispose();
  });

  it("lets the operator detach a manual unknown and start another operation", async () => {
    vi.mocked(disconnectSession)
      .mockResolvedValueOnce(operation("queued"))
      .mockResolvedValueOnce(operation("succeeded"));
    vi.mocked(getOperation).mockResolvedValue(operation("unknown"));
    const store = useFleetStore();
    await store.select(node.id);

    const unknownCompletion = store.disconnectSession(
      session.id,
      "manual reconciliation",
    );
    await vi.advanceTimersByTimeAsync(750);
    expect(store.latestOperation?.state).toBe("unknown");
    expect(store.operationTracking).toBe(true);

    store.detachOperation();
    await unknownCompletion;
    expect(store.operationTracking).toBe(false);
    expect(store.latestOperation?.state).toBe("unknown");

    await store.disconnectSession(session.id, "unrelated follow-up");
    expect(disconnectSession).toHaveBeenCalledTimes(2);
    expect(store.latestOperation?.state).toBe("succeeded");
    store.$dispose();
  });

  it("cancels unknown polling when the workspace changes", async () => {
    let pollingSignal: AbortSignal | undefined;
    vi.stubGlobal("window", new EventTarget());
    vi.mocked(disconnectSession).mockResolvedValue(operation("queued"));
    vi.mocked(getOperation).mockImplementation((_operationId, signal) => {
      pollingSignal = signal;
      return new Promise((_resolve, reject) => {
        signal?.addEventListener(
          "abort",
          () => {
            reject(new DOMException("aborted", "AbortError"));
          },
          { once: true },
        );
      });
    });
    const store = useFleetStore();
    await store.select(node.id);

    const completion = store.disconnectSession(session.id, "support case");
    await vi.advanceTimersByTimeAsync(750);
    vi.mocked(workspaceContext).mockReturnValue({
      id: "other-workspace",
      generation: 2,
    });
    window.dispatchEvent(new Event(workspaceChangedEvent));
    await completion;

    expect(pollingSignal?.aborted).toBe(true);
    expect(store.latestOperation).toBeUndefined();
    store.$dispose();
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

  it("does not mark the fleet unavailable when a node selection returns 404", async () => {
    vi.mocked(getNode).mockRejectedValueOnce({ response: { status: 404 } });
    const store = useFleetStore();

    await store.select("missing-node");

    expect(store.selectionError).toBe("notFound");
    expect(store.unavailable).toBe(false);
    expect(store.selected).toBeUndefined();
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

  it("tracks the reconciled agent upgrade through verification to success", async () => {
    vi.mocked(upgradeNodeAgent).mockResolvedValue(operation("accepted"));
    vi.mocked(getOperation)
      .mockResolvedValueOnce(operation("running"))
      .mockResolvedValueOnce(operation("succeeded"));
    const store = useFleetStore();
    await store.select(node.id);

    const completion = store.upgradeAgent(
      "2.0.0",
      "monthly maintained release",
      "019fc0a4-6d92-765c-a8a1-4af556614c77",
    );
    await vi.advanceTimersByTimeAsync(0);
    expect(upgradeNodeAgent).toHaveBeenCalledWith(
      node,
      "2.0.0",
      "monthly maintained release",
      "019fc0a4-6d92-765c-a8a1-4af556614c77",
      expect.any(AbortSignal),
    );
    expect(store.latestOperation?.state).toBe("accepted");
    await vi.advanceTimersByTimeAsync(750);
    expect(store.latestOperation?.state).toBe("running");
    await vi.advanceTimersByTimeAsync(750);
    await completion;
    expect(store.latestOperation?.state).toBe("succeeded");
    expect(getOperation).toHaveBeenCalledTimes(2);
    store.$dispose();
  });

  it("ends upgrade tracking on the conservative unknown outcome", async () => {
    vi.mocked(upgradeNodeAgent).mockResolvedValue(operation("accepted"));
    vi.mocked(getOperation)
      .mockResolvedValueOnce(operation("unknown"))
      .mockResolvedValueOnce(operation("failed"));
    const store = useFleetStore();
    await store.select(node.id);

    const completion = store.upgradeAgent(
      "2.0.0",
      "monthly maintained release",
      "019fc0a4-6d92-765c-a8a1-4af556614c77",
    );
    await vi.advanceTimersByTimeAsync(750);
    await completion;

    expect(store.latestOperation?.state).toBe("unknown");
    // Unknown is terminal for a reconciled upgrade: no retry may be inferred.
    expect(getOperation).toHaveBeenCalledTimes(1);
    expect(store.operationTracking).toBe(false);
    store.$dispose();
  });
});
