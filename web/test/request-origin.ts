import { vi } from "vitest";

// Browsers resolve same-origin request paths against the document URL.
export const requestOrigin = "https://console.invalid";
export function stubRequestOrigin() {
  const NativeRequest = Request;
  vi.stubGlobal(
    "Request",
    class extends NativeRequest {
      constructor(input: RequestInfo | URL, init?: RequestInit) {
        super(
          typeof input === "string" ? new URL(input, requestOrigin) : input,
          init,
        );
      }
    },
  );
}
