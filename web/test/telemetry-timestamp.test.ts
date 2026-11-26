import { expect, it } from "vitest";
import { formatTimestamp } from "../src/shared/timestamp";

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
