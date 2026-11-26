import type {
  NodeIpBan,
  NodeObservedState,
  NodeSession,
  Operation,
  UserGroupResourceState,
} from "@/api/models";
import { createPinia, setActivePinia } from "pinia";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { responseError } from "./helpers";

import { createUser } from "../src/api/users";
import { disconnectSession, getOperation } from "../src/api/operations";
import { upgradeNodeAgent } from "../src/api/agents";
import { listEvents } from "../src/api/events";
import {
  getNode,
  listNodeIpBans,
  listNodeUserGroupState,
  listNodes,
  listNodeSessions,
} from "../src/api/nodes";
import { useFleetStore } from "../src/shared/fleet";

vi.mock("../src/api/users", () => ({
  applyGroup: vi.fn(),
  createUser: vi.fn(),
  disableUser: vi.fn(),
  enableUser: vi.fn(),
  rotateUserPassword: vi.fn(),
}));
vi.mock("../src/api/operations", () => ({
  disconnectSession: vi.fn(),
  getOperation: vi.fn(),
  reloadService: vi.fn(),
  removeIpBan: vi.fn(),
  terminateSession: vi.fn(),
}));
vi.mock("../src/api/nodes", () => ({
  getNode: vi.fn(),
  listNodeIpBans: vi.fn(),
  listNodeUserGroupState: vi.fn(),
  listNodeSessions: vi.fn(),
  listNodes: vi.fn(),
}));
vi.mock("../src/api/events", () => ({
  eventStreamPath: vi.fn(),
  listEvents: vi.fn(),
  platformEventsEvent: "ocservia:platform-events",
}));
vi.mock("../src/api/agents", () => ({
  upgradeNodeAgent: vi.fn(),
}));

const node: NodeObservedState = {
  id: "019fc0a4-6d92-765c-a8a1-4af556614cc3",
  name: "node",
  version: 4,
  trust_status: "active",
  connection_state: "online",
  freshness: "fresh",
  boot_id: "019fc0a4-6d92-765c-a8a1-4af556614cc4",
  dropped: { security: 0, health: 0, aggregate: 0, raw: 0 },
  session_count: 1,
};
const session: NodeSession = {
  id: "42",
  username: "alice",
  client_ip: "192.0.2.10",
  connected_at: new Date(0).toISOString(),
  bytes_in: 0,
  bytes_out: 0,
};
const operation = (state: Operation["state"], nodeId = node.id): Operation => ({
  id: "019fc0a4-6d92-765c-a8a1-4af556614cc5",
  node_id: nodeId,
  state,
  version: 1,
  created_at: new Date(0).toISOString(),
  updated_at: new Date(0).toISOString(),
});

const nodeB: NodeObservedState = {
  ...node,
  id: "019fc0a4-6d92-765c-a8a1-4af556614cc6",
  name: "node-b",
};

const nodePage = (item: NodeObservedState) => ({
  items: [item],
  page: { has_more: false },
});

describe("controlled fleet operations", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    setActivePinia(createPinia());
    vi.resetAllMocks();

    vi.mocked(listEvents).mockResolvedValue({
      items: [],
      page: { has_more: false },
    });
    vi.mocked(getNode).mockResolvedValue(node);
    vi.mocked(listNodeIpBans).mockResolvedValue({ items: [] });
    vi.mocked(listNodeUserGroupState).mockResolvedValue({ items: [] });
    vi.mocked(listNodeSessions).mockResolvedValue({
      items: [],
      page: { has_more: false },
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
      .mockResolvedValueOnce({ items: [session], page: { has_more: false } })
      .mockResolvedValueOnce({ items: [], page: { has_more: false } });
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

  it("stops operation tracking and clears detail state when leaving the view", async () => {
    vi.mocked(disconnectSession).mockResolvedValue(operation("queued"));
    vi.mocked(getOperation).mockResolvedValue(operation("succeeded"));
    const store = useFleetStore();
    await store.select(node.id);
    const completion = store.disconnectSession(session.id, "support case");
    await vi.advanceTimersByTimeAsync(0);
    store.stop();
    expect(store.operationTracking).toBe(false);
    await vi.advanceTimersByTimeAsync(750);
    await completion;
    expect(store.latestOperation).toBeUndefined();
    expect(getOperation).not.toHaveBeenCalled();
    expect(store.selected).toBeUndefined();
    expect(getNode).toHaveBeenCalledTimes(1);
  });

  it("backs off offline polling, suspends it while hidden, and allows detaching", async () => {
    const page = Object.assign(new EventTarget(), {
      visibilityState: "visible",
    });
    vi.stubGlobal("document", page);
    vi.mocked(disconnectSession).mockResolvedValue(
      operation("offline_pending"),
    );
    vi.mocked(getOperation).mockResolvedValue(operation("offline_pending"));
    const store = useFleetStore();
    await store.select(node.id);

    const completion = store.disconnectSession(session.id, "support case");
    await vi.advanceTimersByTimeAsync(4_999);
    expect(getOperation).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(1);
    expect(getOperation).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(9_999);
    expect(getOperation).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(getOperation).toHaveBeenCalledTimes(2);

    page.visibilityState = "hidden";
    await vi.advanceTimersByTimeAsync(120_000);
    expect(getOperation).toHaveBeenCalledTimes(2);
    page.visibilityState = "visible";
    page.dispatchEvent(new Event("visibilitychange"));
    await vi.advanceTimersByTimeAsync(0);
    expect(getOperation).toHaveBeenCalledTimes(3);

    page.visibilityState = "hidden";
    await vi.advanceTimersByTimeAsync(40_000);
    store.detachOperation();
    await completion;
    page.visibilityState = "visible";
    page.dispatchEvent(new Event("visibilitychange"));
    await vi.advanceTimersByTimeAsync(60_000);
    expect(getOperation).toHaveBeenCalledTimes(3);
    expect(store.operationTracking).toBe(false);
    store.$dispose();
  });

  it("ignores a tracked operation from a different node", async () => {
    vi.mocked(getOperation).mockResolvedValue(
      operation("offline_pending", nodeB.id),
    );
    const store = useFleetStore();
    await store.select(node.id);
    await store.trackOperation("foreign");
    expect(store.latestOperation).toBeUndefined();
    expect(store.operationTracking).toBe(false);
    expect(getOperation).toHaveBeenCalledTimes(1);
    store.$dispose();
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

  it.each(["succeeded", "failed"] as const)(
    "continues polling from unknown until the operation reaches %s",
    async (terminalState) => {
      vi.mocked(disconnectSession).mockResolvedValue(operation("queued"));
      vi.mocked(getOperation)
        .mockResolvedValueOnce(operation("unknown"))
        .mockResolvedValueOnce(operation(terminalState));
      vi.mocked(listNodeSessions)
        .mockResolvedValueOnce({ items: [session], page: { has_more: false } })
        .mockResolvedValueOnce({ items: [], page: { has_more: false } });
      const store = useFleetStore();
      await store.select(node.id);

      const completion = store.disconnectSession(session.id, "support case");
      await vi.advanceTimersByTimeAsync(750);
      expect(store.latestOperation?.state).toBe("unknown");
      await vi.advanceTimersByTimeAsync(1_500);
      await completion;

      expect(store.latestOperation?.state).toBe(terminalState);
      expect(store.sessions).toEqual(
        terminalState === "succeeded" ? [] : [session],
      );
      expect(getOperation).toHaveBeenCalledTimes(2);
      store.$dispose();
    },
  );

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
      .mockResolvedValueOnce(operation("succeeded", nodeB.id));
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
    expect(store.latestOperation).toBeUndefined();
    expect(store.operationTracking).toBe(false);
    await store.disconnectSession(session.id, "node b support case");

    expect(pollingSignal?.aborted).toBe(true);
    expect(store.selected?.id).toBe(nodeB.id);
    expect(store.latestOperation?.state).toBe("succeeded");
    expect(store.latestOperation?.node_id).toBe(nodeB.id);
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

  it("does not let an older rebuild overwrite a newer same-workspace snapshot", async () => {
    const first = Promise.withResolvers<ReturnType<typeof nodePage>>();
    const second = Promise.withResolvers<ReturnType<typeof nodePage>>();
    vi.mocked(listNodes)
      .mockImplementationOnce(() => first.promise)
      .mockImplementationOnce(() => second.promise);
    const store = useFleetStore();

    const staleRebuild = store.rebuild();
    await vi.waitFor(() => {
      expect(listNodes).toHaveBeenCalledTimes(1);
    });
    const currentRebuild = store.rebuild();
    await vi.waitFor(() => {
      expect(listNodes).toHaveBeenCalledTimes(2);
    });

    second.resolve(nodePage(nodeB));
    await currentRebuild;
    first.resolve(nodePage(node));
    await staleRebuild;

    expect(store.nodes).toEqual([nodeB]);
    store.$dispose();
  });

  it("keeps the latest node selection when an earlier detail request returns last", async () => {
    const first = Promise.withResolvers<NodeObservedState>();
    const second = Promise.withResolvers<NodeObservedState>();
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
    vi.mocked(getNode).mockRejectedValueOnce(responseError(404));
    const store = useFleetStore();

    await store.select("missing-node");

    expect(store.selectionError).toBe("notFound");
    expect(store.unavailable).toBe(false);
    expect(store.selected).toBeUndefined();
    store.$dispose();
  });

  it("does not refresh the operated node over a selection that is still loading", async () => {
    const nodeBDetails = Promise.withResolvers<NodeObservedState>();
    const operationResult = Promise.withResolvers<Operation>();
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
    );
    await vi.advanceTimersByTimeAsync(0);
    expect(upgradeNodeAgent).toHaveBeenCalledWith(
      node,
      "2.0.0",
      "monthly maintained release",
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
    );
    await vi.advanceTimersByTimeAsync(750);
    await completion;

    expect(store.latestOperation?.state).toBe("unknown");
    // Unknown is terminal for a reconciled upgrade: no retry may be inferred.
    expect(getOperation).toHaveBeenCalledTimes(1);
    expect(store.operationTracking).toBe(false);
    store.$dispose();
  });

  it("fetches sessions, IP bans, and user/group state concurrently after getNode", async () => {
    const sessionsDeferred = Promise.withResolvers<{
      items: NodeSession[];
      page: { has_more: boolean };
    }>();
    const bansDeferred = Promise.withResolvers<{ items: NodeIpBan[] }>();
    const usersDeferred = Promise.withResolvers<{
      items: UserGroupResourceState[];
    }>();
    vi.mocked(listNodeSessions).mockReturnValueOnce(sessionsDeferred.promise);
    vi.mocked(listNodeIpBans).mockReturnValueOnce(bansDeferred.promise);
    vi.mocked(listNodeUserGroupState).mockReturnValueOnce(
      usersDeferred.promise,
    );

    const store = useFleetStore();
    await store.select(node.id);
    expect(store.selected?.id).toBe(node.id);
    expect(store.sessionsState).toBe("loading");
    expect(listNodeSessions).toHaveBeenCalledTimes(1);
    expect(listNodeIpBans).toHaveBeenCalledTimes(1);
    expect(listNodeUserGroupState).toHaveBeenCalledTimes(1);

    sessionsDeferred.resolve({ items: [session], page: { has_more: false } });
    bansDeferred.resolve({ items: [] });
    usersDeferred.resolve({ items: [] });
    await vi.waitFor(() => {
      expect(store.sessions).toEqual([session]);
    });
    store.$dispose();
  });
});
