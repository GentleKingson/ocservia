// One timer per owner, suspended while the page is hidden or unfocused.
export function isPageActive(): boolean {
  return (
    typeof document === "undefined" ||
    (document.visibilityState !== "hidden" && document.hasFocus())
  );
}

export function createForegroundRefresh(
  refresh: () => Promise<unknown>,
  interval: number | (() => number) = 15_000,
  pause: () => void = () => {},
) {
  let started = false;
  let active = false;
  let running = false;
  let pendingDelay: number | undefined;
  let eventScheduled = false;
  let dueAt = 0;
  let timer: ReturnType<typeof setTimeout> | undefined;

  const pollInterval = () =>
    typeof interval === "number" ? interval : interval();

  function schedule(delay = pollInterval()): void {
    clearTimeout(timer);
    dueAt = Date.now() + delay;
    if (started && active) timer = setTimeout(() => void run(), delay);
  }

  // Event and periodic refresh share one deadline. Events never postpone an
  // already scheduled read; events during a read coalesce into one follow-up.
  function request(delay = 0): void {
    if (!started || !active) return;
    if (running) {
      pendingDelay = Math.min(pendingDelay ?? delay, delay);
      return;
    }
    eventScheduled = true;
    if (timer === undefined || Date.now() + delay < dueAt) schedule(delay);
  }

  function reschedule(): void {
    if (!running && !eventScheduled) schedule();
  }

  async function run(): Promise<void> {
    clearTimeout(timer);
    timer = undefined;
    eventScheduled = false;
    if (!started || !active) return;
    if (running) {
      pendingDelay = 0;
      return;
    }
    running = true;
    try {
      await refresh();
    } catch {
      // The data owner reports failures; the next interval still retries.
    } finally {
      running = false;
      const delay = pendingDelay;
      pendingDelay = undefined;
      eventScheduled = delay !== undefined;
      schedule(delay ?? pollInterval());
    }
  }

  function update(): void {
    const next = isPageActive();
    if (next === active) return;
    active = next;
    clearTimeout(timer);
    timer = undefined;
    pendingDelay = undefined;
    eventScheduled = false;
    if (active) void run();
    else pause();
  }

  function listen(remove: boolean): void {
    if (typeof window === "undefined") return;
    for (const event of ["focus", "blur", "pageshow"]) {
      if (remove) window.removeEventListener(event, update);
      else window.addEventListener(event, update);
    }
    if (typeof document !== "undefined") {
      if (remove) document.removeEventListener("visibilitychange", update);
      else document.addEventListener("visibilitychange", update);
    }
  }

  function start(immediate = true): void {
    if (started) return;
    started = true;
    active = isPageActive();
    listen(false);
    if (immediate) void run();
    else schedule();
  }

  function stop(): void {
    started = false;
    active = false;
    pendingDelay = undefined;
    eventScheduled = false;
    clearTimeout(timer);
    timer = undefined;
    listen(true);
    pause();
  }

  return { start, stop, request, reschedule };
}
