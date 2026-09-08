import { describe, expect, it, vi } from "vitest";

import { retryAfterSeconds, safeLoginReturnPath } from "../src/shared/login";

describe("login return paths", () => {
  it.each([
    undefined,
    "https://evil.example",
    "//evil.example",
    "/a/..//evil.example",
    "/%2e//evil.example",
    "/\\evil.example",
    "/\n/evil.example",
    "relative",
    "/login",
    "/api/v1/auth/login",
    "/nodes/../login",
  ])("rejects %s", (value) => {
    expect(safeLoginReturnPath(value)).toBeUndefined();
  });
  it("preserves an internal path, query and fragment", () => {
    expect(safeLoginReturnPath("/nodes?sort=name#detail")).toBe(
      "/nodes?sort=name#detail",
    );
  });
});

describe("Retry-After", () => {
  it("supports seconds, dates and invalid or missing headers", () => {
    vi.spyOn(Date, "now").mockReturnValue(Date.parse("2026-09-08T00:00:00Z"));
    expect(retryAfterSeconds("30")).toBe(30);
    expect(retryAfterSeconds("Tue, 08 Sep 2026 00:01:00 GMT")).toBe(60);
    expect(retryAfterSeconds(null)).toBeUndefined();
    expect(retryAfterSeconds("invalid")).toBeUndefined();
    vi.restoreAllMocks();
  });
});
