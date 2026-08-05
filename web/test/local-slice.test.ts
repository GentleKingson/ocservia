import type {
  Operation,
  OperationPage,
  PlatformEvent,
  PlatformEventPage,
} from "@ocservia/api-client";
import { createPinia, setActivePinia } from "pinia";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import {
  createLocalSimulation,
  getOperation,
  listEvents,
  listOperations,
} from "../src/api/client";
import { useLocalSliceStore } from "../src/shared/localSlice";

vi.mock("../src/api/client", () => ({
  createLocalSimulation: vi.fn(),
  getWorkspace: vi.fn().mockResolvedValue({ id: "workspace" }),
  getOperation: vi.fn(),
  listEvents: vi.fn(),
  listOperations: vi.fn(),
  workspaceContext: vi.fn().mockReturnValue({ id: "workspace", generation: 1 }),
}));

const operation = (
  state: Operation["state"],
  id = "019fc0a4-6d92-765c-a8a1-4af556614cc3",
): Operation => ({
  id,
  state,
  version: 1,
  createdAt: new Date(0),
  updatedAt: new Date(0),
});

const operationPage = (...items: Operation[]): OperationPage => ({
  items,
  page: { hasMore: false },
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

async function flushPromises(): Promise<void> {
  for (let index = 0; index < 6; index += 1) await Promise.resolve();
}

const event = (id: string, nodeId: string): PlatformEvent => ({
  id,
  nodeId,
  type: "connected",
  traceparent: "00-00000000000000000000000000000000-0000000000000000-01",
  occurredAt: new Date(0),
});

describe("local slice operation reconciliation", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    vi.mocked(createLocalSimulation).mockReset();
    vi.mocked(getOperation).mockReset();
    vi.mocked(listEvents).mockReset();
    vi.mocked(listEvents).mockResolvedValue({
      items: [],
      page: { hasMore: false },
    } satisfies PlatformEventPage);
    vi.mocked(getOperation).mockReset();
    vi.mocked(listOperations).mockReset();
    setActivePinia(createPinia());
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it("treats unknown operations as terminal", async () => {
    vi.mocked(listOperations).mockResolvedValue(
      operationPage(operation("unknown")),
    );
    const store = useLocalSliceStore();

    await store.rebuild();
    await vi.advanceTimersByTimeAsync(2000);

    expect(store.pendingOperations).toBe(0);
    expect(listOperations).toHaveBeenCalledTimes(1);
    store.$dispose();
  });

  it("refreshes operations discovered during rebuild until they complete", async () => {
    vi.mocked(listOperations)
      .mockResolvedValueOnce(operationPage(operation("running")))
      .mockResolvedValueOnce(operationPage(operation("succeeded")));
    const store = useLocalSliceStore();

    await store.rebuild();
    expect(store.pendingOperations).toBe(1);

    await vi.advanceTimersByTimeAsync(1000);

    expect(store.pendingOperations).toBe(0);
    expect(listOperations).toHaveBeenCalledTimes(2);
    store.$dispose();
  });

  it("discovers operations created by another client after an empty rebuild", async () => {
    vi.mocked(listOperations)
      .mockResolvedValueOnce(operationPage())
      .mockResolvedValueOnce(operationPage(operation("running")))
      .mockResolvedValueOnce(operationPage(operation("succeeded")));
    const store = useLocalSliceStore();

    await store.rebuild();
    expect(store.pendingOperations).toBe(0);

    await vi.advanceTimersByTimeAsync(5000);
    expect(store.pendingOperations).toBe(1);

    await vi.advanceTimersByTimeAsync(1000);
    expect(store.pendingOperations).toBe(0);
    expect(listOperations).toHaveBeenCalledTimes(3);
    store.$dispose();
  });

  it("does not let an older same-workspace rebuild overwrite newer events", async () => {
    const first = deferred<PlatformEventPage>();
    const second = deferred<PlatformEventPage>();
    vi.mocked(listEvents)
      .mockImplementationOnce(() => first.promise)
      .mockImplementationOnce(() => second.promise);
    vi.mocked(listOperations).mockResolvedValue(operationPage());
    const store = useLocalSliceStore();

    const staleRebuild = store.rebuild();
    await Promise.resolve();
    await Promise.resolve();
    const currentRebuild = store.rebuild();
    await Promise.resolve();
    await Promise.resolve();

    second.resolve({
      items: [event("event-new", "node-new")],
      page: { hasMore: false },
    });
    await currentRebuild;
    first.resolve({
      items: [event("event-old", "node-old")],
      page: { hasMore: false },
    });
    await staleRebuild;

    expect(store.events.map((current) => current.id)).toEqual(["event-new"]);
    expect(store.activeNodes).toBe(1);
    store.$dispose();
  });

  it("does not let an older pending refresh replace a newer operation set", async () => {
    const stale = deferred<OperationPage>();
    const current = deferred<OperationPage>();
    vi.mocked(listOperations)
      .mockResolvedValueOnce(
        operationPage(operation("running", "seed-operation")),
      )
      .mockImplementationOnce(() => stale.promise)
      .mockImplementationOnce(() => current.promise);
    const store = useLocalSliceStore();

    await store.rebuild();
    expect(store.pendingOperations).toBe(1);

    await vi.advanceTimersByTimeAsync(1000);
    expect(listOperations).toHaveBeenCalledTimes(2);

    const currentRebuild = store.rebuild();
    await flushPromises();
    expect(listOperations).toHaveBeenCalledTimes(3);

    current.resolve(operationPage(operation("running", "current-operation")));
    await currentRebuild;
    stale.resolve(
      operationPage(
        operation("running", "old-operation"),
        operation("running", "old-operation-2"),
      ),
    );
    await Promise.resolve();
    await Promise.resolve();

    expect(store.pendingOperations).toBe(1);
    store.$dispose();
  });

  it("keeps the newest operation poll when an older poll returns last", async () => {
    const oldPoll = deferred<Operation>();
    const newPoll = deferred<Operation>();
    vi.mocked(createLocalSimulation)
      .mockResolvedValueOnce(operation("queued", "old-operation"))
      .mockResolvedValueOnce(operation("queued", "new-operation"));
    vi.mocked(getOperation)
      .mockImplementationOnce(() => oldPoll.promise)
      .mockImplementationOnce(() => newPoll.promise);
    const store = useLocalSliceStore();

    await store.run({ heartbeatCount: 1 });
    await vi.advanceTimersByTimeAsync(250);
    const replacement = store.run({ heartbeatCount: 2 });
    await replacement;
    await vi.advanceTimersByTimeAsync(250);

    newPoll.resolve(operation("succeeded", "new-operation"));
    await Promise.resolve();
    oldPoll.resolve(operation("succeeded", "old-operation"));
    await Promise.resolve();
    await Promise.resolve();

    expect(store.operation?.id).toBe("new-operation");
    expect(store.running).toBe(false);
    store.$dispose();
  });
});
