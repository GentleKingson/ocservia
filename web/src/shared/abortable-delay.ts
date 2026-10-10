// Resolves after the delay, or rejects with an AbortError as soon as the
// signal aborts; either way the timer and the abort listener are released.
export function abortableDelay(
  delay: number,
  signal: AbortSignal,
  message: string,
): Promise<void> {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(finish, delay);
    function finish(): void {
      signal.removeEventListener("abort", abort);
      resolve();
    }
    function abort(): void {
      clearTimeout(timer);
      signal.removeEventListener("abort", abort);
      reject(new DOMException(message, "AbortError"));
    }
    if (signal.aborted) abort();
    else signal.addEventListener("abort", abort, { once: true });
  });
}
