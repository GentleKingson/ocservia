import createClient from "openapi-fetch";
import type { paths } from "./generated/schema";
import { fromWire } from "./models";
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

export class ResponseError extends Error {
  constructor(
    readonly response: Response,
    message = "Response returned an error code",
  ) {
    super(message);
  }
}

export const client = createClient<paths>({
  baseUrl: "/api/v1",
  credentials: "same-origin",
  fetch: (request) => fetch(request),
});

client.use({
  onRequest({ request, schemaPath }) {
    if (devAuthToken && schemaPath !== "/auth/login")
      request.headers.set("Authorization", `Bearer ${devAuthToken}`);
    return request;
  },
  onResponse({ response, schemaPath }) {
    if (schemaPath === "/auth/login") return;
    if (response.status === 401) redirectToLogin();
    // A failed audit verification is a structured result.
    if (
      !response.ok &&
      !(schemaPath === "/audit:verify" && response.status === 409)
    )
      throw new ResponseError(response);
  },
});

export async function read<T>(
  request: Promise<{ data?: T; response: Response }>,
) {
  const { data, response } = await request;
  if (data === undefined) throw new ResponseError(response);
  return fromWire(data);
}
