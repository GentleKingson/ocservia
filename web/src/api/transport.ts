import { Configuration } from "@ocservia/api-client";
import { redirectToLogin } from "../shared/session";

export const devAuthToken = import.meta.env.DEV
  ? import.meta.env.VITE_DEV_AUTH_TOKEN
  : undefined;

export function newIdempotencyKey(): string {
  if (typeof globalThis.crypto.randomUUID === "function")
    return globalThis.crypto.randomUUID();
  const bytes = globalThis.crypto.getRandomValues(new Uint8Array(16));
  bytes[6] = ((bytes[6] ?? 0) & 0x0f) | 0x40;
  bytes[8] = ((bytes[8] ?? 0) & 0x3f) | 0x80;
  const hex = Array.from(bytes, (value) => value.toString(16).padStart(2, "0"));
  return `${hex.slice(0, 4).join("")}-${hex.slice(4, 6).join("")}-${hex.slice(6, 8).join("")}-${hex.slice(8, 10).join("")}-${hex.slice(10).join("")}`;
}

export async function authenticatedFetch(
  input: RequestInfo | URL,
  init?: RequestInit,
): Promise<Response> {
  const response = await fetch(input, init);
  if (response.status === 401) redirectToLogin();
  return response;
}

export const configuration = new Configuration({
  basePath: "/api/v1",
  fetchApi: authenticatedFetch,
  credentials: "same-origin",
  ...(devAuthToken ? { accessToken: devAuthToken } : {}),
});

export function requestInit(signal?: AbortSignal): RequestInit | undefined {
  return signal ? { signal } : undefined;
}
