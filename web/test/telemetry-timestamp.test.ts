import { describe, expect, it } from "vitest";
import { fromWire } from "@/api/models";
import { formatTimestamp } from "../src/shared/timestamp";

describe("API timestamps", () => {
  it.each(["1970-01-01T00:00:00Z", "2026-09-10T04:34:56.123456Z"])(
    "preserves %s in domain objects",
    (at) => {
      expect(fromWire({ observed_at: at }).observedAt).toBe(at);
      expect(fromWire({ expires_at: at }).expiresAt).toBe(at);
      expect(fromWire({ occurred_at: at }).occurredAt).toBe(at);
    },
  );

  it("preserves ordered members and opaque JSON without renaming their keys", () => {
    const members = ["alpha", "omega"];
    const warnings = ["safe", null, { extra_value: [1.25] }];
    const actions = { config_apply: { allowed: true, reason: "available" } };
    expect(
      fromWire({
        desired_members: members,
        warnings,
        effective_actions: actions,
      }),
    ).toEqual({
      desiredMembers: members,
      warnings,
      effectiveActions: actions,
    });
  });
});

it("displays UTC across offsets without losing microseconds", () => {
  expect(formatTimestamp("2026-09-30T16:00:00.123456+08:00")).toBe(
    "2026-09-30 08:00:00.123456 UTC",
  );
  expect(formatTimestamp("2026-09-30T08:00:00.123456Z")).toBe(
    "2026-09-30 08:00:00.123456 UTC",
  );
  expect(formatTimestamp("2026-09-30T01:00:00-07:00")).toBe(
    "2026-09-30 08:00:00 UTC",
  );
  expect(formatTimestamp(undefined)).toBe("—");
  expect(formatTimestamp("2026-invalid")).toBe("2026-invalid");
});
