import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { createForegroundRefresh } from "../src/shared/foreground-refresh";

describe("foreground refresh", () => {
  let focused: boolean;
  let hidden: boolean;
  let stop: (() => void) | undefined;

  beforeEach(() => {
    vi.useFakeTimers();
    focused = true;
    hidden = false;
    vi.stubGlobal("window", new EventTarget());
    vi.stubGlobal(
      "document",
      Object.assign(new EventTarget(), {
        hasFocus: () => focused,
      }),
    );
    Object.defineProperty(document, "visibilityState", {
      get: () => (hidden ? "hidden" : "visible"),
    });
  });

  afterEach(() => {
    stop?.();
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  it("keeps polling when blurred, pauses when hidden and resumes once, then every 15 seconds", async () => {
    const refresh = vi.fn().mockResolvedValue(undefined);
    const pause = vi.fn();
    const polling = createForegroundRefresh(refresh, 15_000, pause);
    stop = polling.stop;
    polling.start();
    await vi.advanceTimersByTimeAsync(15_000);
    expect(refresh).toHaveBeenCalledTimes(2);
    focused = false;
    window.dispatchEvent(new Event("blur"));
    await vi.advanceTimersByTimeAsync(15_000);
    expect(refresh).toHaveBeenCalledTimes(3);
    expect(pause).not.toHaveBeenCalled();
    hidden = true;
    document.dispatchEvent(new Event("visibilitychange"));
    await vi.advanceTimersByTimeAsync(60_000);
    expect(refresh).toHaveBeenCalledTimes(3);
    expect(pause).toHaveBeenCalledTimes(1);
    focused = true;
    window.dispatchEvent(new Event("focus"));
    expect(refresh).toHaveBeenCalledTimes(3);
    hidden = false;
    document.dispatchEvent(new Event("visibilitychange"));
    window.dispatchEvent(new Event("focus"));
    window.dispatchEvent(new Event("pageshow"));
    await vi.advanceTimersByTimeAsync(0);
    expect(refresh).toHaveBeenCalledTimes(4);
    await vi.advanceTimersByTimeAsync(15_000);
    expect(refresh).toHaveBeenCalledTimes(5);
    polling.stop();
    window.dispatchEvent(new Event("focus"));
    await vi.advanceTimersByTimeAsync(60_000);
    expect(refresh).toHaveBeenCalledTimes(5);
  });

  it("does not overlap slow reads and retries after failures", async () => {
    let finish!: () => void;
    const refresh = vi
      .fn()
      .mockImplementationOnce(
        () =>
          new Promise<void>((resolve) => {
            finish = resolve;
          }),
      )
      .mockRejectedValueOnce(new Error("offline"))
      .mockResolvedValue(undefined);
    const polling = createForegroundRefresh(refresh);
    stop = polling.stop;
    polling.start();
    await vi.advanceTimersByTimeAsync(60_000);
    hidden = true;
    document.dispatchEvent(new Event("visibilitychange"));
    hidden = false;
    document.dispatchEvent(new Event("visibilitychange"));
    expect(refresh).toHaveBeenCalledTimes(1);
    finish();
    await vi.advanceTimersByTimeAsync(0);
    expect(refresh).toHaveBeenCalledTimes(2);
    await vi.advanceTimersByTimeAsync(15_000);
    expect(refresh).toHaveBeenCalledTimes(3);
  });

  it("defers an initially background page until it becomes visible", async () => {
    hidden = true;
    const refresh = vi.fn().mockResolvedValue(undefined);
    const polling = createForegroundRefresh(refresh);
    stop = polling.stop;
    polling.start();
    polling.start();
    await vi.advanceTimersByTimeAsync(60_000);
    expect(refresh).not.toHaveBeenCalled();
    hidden = false;
    document.dispatchEvent(new Event("visibilitychange"));
    await vi.advanceTimersByTimeAsync(0);
    expect(refresh).toHaveBeenCalledTimes(1);
  });

  it("uses adaptive intervals and replaces a pending poll with one event refresh", async () => {
    let interval = 60_000;
    const refresh = vi.fn().mockResolvedValue(undefined);
    const polling = createForegroundRefresh(refresh, () => interval);
    stop = polling.stop;
    polling.start();
    await vi.advanceTimersByTimeAsync(59_900);
    expect(refresh).toHaveBeenCalledTimes(1);
    polling.request(150);
    polling.request(150);
    await vi.advanceTimersByTimeAsync(100);
    expect(refresh).toHaveBeenCalledTimes(2);
    await vi.advanceTimersByTimeAsync(150);
    expect(refresh).toHaveBeenCalledTimes(2);
    interval = 15_000;
    polling.reschedule();
    await vi.advanceTimersByTimeAsync(14_999);
    expect(refresh).toHaveBeenCalledTimes(2);
    await vi.advanceTimersByTimeAsync(1);
    expect(refresh).toHaveBeenCalledTimes(3);
    interval = 60_000;
    polling.reschedule();
    await vi.advanceTimersByTimeAsync(59_999);
    expect(refresh).toHaveBeenCalledTimes(3);
    await vi.advanceTimersByTimeAsync(1);
    expect(refresh).toHaveBeenCalledTimes(4);
  });

  it("coalesces events during a slow read without starving or overlapping it", async () => {
    let finish!: () => void;
    const refresh = vi
      .fn()
      .mockImplementationOnce(
        () =>
          new Promise<void>((resolve) => {
            finish = resolve;
          }),
      )
      .mockResolvedValue(undefined);
    const polling = createForegroundRefresh(refresh, 60_000);
    stop = polling.stop;
    polling.start();
    for (let index = 0; index < 10; index += 1) {
      polling.request(150);
      await vi.advanceTimersByTimeAsync(150);
    }
    expect(refresh).toHaveBeenCalledTimes(1);
    finish();
    await vi.advanceTimersByTimeAsync(150);
    expect(refresh).toHaveBeenCalledTimes(2);
    await vi.advanceTimersByTimeAsync(59_999);
    expect(refresh).toHaveBeenCalledTimes(2);
    await vi.advanceTimersByTimeAsync(1);
    expect(refresh).toHaveBeenCalledTimes(3);
    polling.request(150);
    polling.stop();
    await vi.advanceTimersByTimeAsync(60_000);
    expect(refresh).toHaveBeenCalledTimes(3);
  });
});
