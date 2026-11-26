import createClient from "openapi-fetch";
import type { paths } from "./generated/schema";
import { redirectToLogin } from "../shared/session";

export const devAuthToken = import.meta.env.DEV
  ? import.meta.env.VITE_DEV_AUTH_TOKEN
  : undefined;

export function formatUuid(bytes: Uint8Array): string {
  const hex = Array.from(bytes, (value) => value.toString(16).padStart(2, "0"));
  return `${hex.slice(0, 4).join("")}-${hex.slice(4, 6).join("")}-${hex.slice(6, 8).join("")}-${hex.slice(8, 10).join("")}-${hex.slice(10).join("")}`;
}

export function newIdempotencyKey(): string {
  if (typeof globalThis.crypto.randomUUID === "function")
    return globalThis.crypto.randomUUID();
  const bytes = globalThis.crypto.getRandomValues(new Uint8Array(16));
  bytes[6] = ((bytes[6] ?? 0) & 0x0f) | 0x40;
  bytes[8] = ((bytes[8] ?? 0) & 0x3f) | 0x80;
  return formatUuid(bytes);
}

export class ResponseError extends Error {
  constructor(
    readonly response: Response,
    message = "",
    readonly type = "",
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
  async onResponse({ response, schemaPath }) {
    if (schemaPath === "/auth/login") return;
    const passwordChange = schemaPath === "/auth/change-password";
    if (response.status === 401 && !passwordChange) redirectToLogin();
    // A failed audit verification is a structured result.
    if (
      !response.ok &&
      !(schemaPath === "/audit:verify" && response.status === 409)
    ) {
      let detail = "";
      let type = "";
      if (
        response.headers
          .get("content-type")
          ?.includes("application/problem+json")
      ) {
        try {
          const problem: unknown = await response.clone().json();
          if (problem && typeof problem === "object") {
            if ("detail" in problem && typeof problem.detail === "string")
              detail = problem.detail;
            if ("type" in problem && typeof problem.type === "string")
              type = problem.type;
          }
        } catch {
          // Keep the HTTP status available for caller-specific handling.
        }
      }
      if (
        passwordChange &&
        response.status === 401 &&
        type !== "https://ocservia.dev/problems/password-change"
      )
        redirectToLogin();
      throw new ResponseError(response, detail, type);
    }
  },
});

export async function read<T>(
  request: Promise<{ data?: T; response: Response }>,
): Promise<T> {
  const { data, response } = await request;
  if (data === undefined) throw new ResponseError(response);
  return data;
}

export function fencedHeaders(version: number) {
  return {
    "Idempotency-Key": newIdempotencyKey(),
    "If-Match": `"revision-${String(version)}"`,
  };
}

export function fencedRequest(
  version: number,
  reason: string,
  ttlSeconds: number,
) {
  return {
    header: fencedHeaders(version),
    body: { reason, expected_version: version, ttl_seconds: ttlSeconds },
  };
}
