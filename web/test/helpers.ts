import { nextTick } from "vue";
import { vi } from "vitest";
import { ResponseError } from "@/api/transport";

export function stubSessionStorage(): Map<string, string> {
  const storage = new Map<string, string>();
  vi.stubGlobal("sessionStorage", {
    getItem: (key: string) => storage.get(key) ?? null,
    setItem: (key: string, value: string) => storage.set(key, value),
    removeItem: (key: string) => storage.delete(key),
  });
  return storage;
}

export async function flushTicks(ticks = 8): Promise<void> {
  for (let i = 0; i < ticks; i++) await nextTick();
}

export async function flushTimers(ticks = 8): Promise<void> {
  await flushTicks(ticks);
  await vi.advanceTimersByTimeAsync(0);
  await flushTicks(ticks);
}

export function responseError(
  status: number,
  body: BodyInit | null = null,
): ResponseError {
  return new ResponseError(new Response(body, { status }));
}
