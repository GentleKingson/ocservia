import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { waitForNodePoll } from "../src/features/node-workflow";
import { abortableDelay } from "../src/shared/abortable-delay";

describe("abortable delay", () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  it("rejects an already aborted signal without starting a timer", async () => {
    const controller = new AbortController();
    controller.abort();
    const remove = vi.spyOn(controller.signal, "removeEventListener");

    await expect(
      abortableDelay(1_000, controller.signal, "stopped"),
    ).rejects.toMatchObject({ name: "AbortError", message: "stopped" });
    expect(vi.getTimerCount()).toBe(0);
    expect(remove).toHaveBeenCalledWith("abort", expect.any(Function));
  });

  it("clears the timer and listener when aborted while waiting", async () => {
    const controller = new AbortController();
    const remove = vi.spyOn(controller.signal, "removeEventListener");
    const wait = abortableDelay(1_000, controller.signal, "stopped");
    expect(vi.getTimerCount()).toBe(1);

    controller.abort();
    await expect(wait).rejects.toMatchObject({ name: "AbortError" });
    expect(vi.getTimerCount()).toBe(0);
    expect(remove).toHaveBeenCalledWith("abort", expect.any(Function));
  });

  it("releases the listener when the delay completes", async () => {
    const controller = new AbortController();
    const remove = vi.spyOn(controller.signal, "removeEventListener");
    let settled = false;
    const wait = waitForNodePoll(controller.signal).then(() => {
      settled = true;
    });

    await vi.advanceTimersByTimeAsync(499);
    expect(settled).toBe(false);
    await vi.advanceTimersByTimeAsync(1);
    await wait;
    expect(settled).toBe(true);
    expect(remove).toHaveBeenCalledWith("abort", expect.any(Function));
  });
});
