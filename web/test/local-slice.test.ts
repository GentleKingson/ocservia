import type {
  Operation,
  OperationPage,
  PlatformEventPage,
} from "@ocservia/api-client";
import { createPinia, setActivePinia } from "pinia";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { getOperation, listEvents, listOperations } from "../src/api/client";
import { useLocalSliceStore } from "../src/shared/localSlice";

vi.mock("../src/api/client", () => ({
  createLocalSimulation: vi.fn(),
  getOperation: vi.fn(),
  listEvents: vi.fn(),
  listOperations: vi.fn(),
}));

const operation = (state: Operation["state"]): Operation => ({
  id: "019fc0a4-6d92-765c-a8a1-4af556614cc3",
  state,
  createdAt: new Date(0),
  updatedAt: new Date(0),
});

const operationPage = (...items: Operation[]): OperationPage => ({
  items,
  page: { hasMore: false },
});

describe("local slice operation reconciliation", () => {
  beforeEach(() => {
    vi.useFakeTimers();
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
});
