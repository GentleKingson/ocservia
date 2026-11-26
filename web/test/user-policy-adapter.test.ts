import type { UserPolicy } from "@/api/models";
import { describe, expect, it, vi } from "vitest";
import { responseError } from "./helpers";

vi.mock("../src/api/users", () => ({
  getUserPolicy: vi.fn(),
  setUserPolicy: vi.fn(),
}));

import { getUserPolicy } from "../src/api/users";

import {
  expiryInputType,
  formToRequest,
  loadUserPolicy,
  policyToForm,
} from "../src/features/user-policy";

function makePolicy(overrides: Partial<UserPolicy> = {}): UserPolicy {
  return {
    node_id: "019fdc5b-b939-72a1-ae67-8efd197e5688",
    username: "alice",
    quota_period: "none",
    quota_direction: "rxtx",
    quota_bytes: 0,
    version: 1,
    period_start: "1970-01-01T00:00:00Z",
    observed_rx_bytes: 0,
    observed_tx_bytes: 0,
    exceeded: false,
    expired: false,
    convergence: "converged",
    ...overrides,
  };
}

describe("user policy feature", () => {
  it("loads the default form for a 404 ResponseError", async () => {
    vi.mocked(getUserPolicy).mockRejectedValueOnce(responseError(404));
    await expect(loadUserPolicy("node", "alice")).resolves.toEqual(
      policyToForm(),
    );
  });

  it.each([
    ["generated non-404", responseError(403)],
    ["network", new TypeError("network unavailable")],
    ["abort", new DOMException("cancelled", "AbortError")],
  ])("preserves the original %s rejection", async (_kind, error) => {
    vi.mocked(getUserPolicy).mockRejectedValueOnce(error);
    await expect(loadUserPolicy("node", "alice")).rejects.toBe(error);
  });

  it("maps display units to integer bytes and UTC expiry", () => {
    const request = formToRequest(
      {
        period: "monthly",
        direction: "rxtx",
        quotaValue: 2.5,
        quotaUnit: "GiB",
        expiresAtUtcInput: "2026-09-01T00:00",
        version: 3,
      },
      "ticket",
    );
    expect(request.quota_bytes).toBe(2_684_354_560);
    expect(request.expires_at).toBe("2026-09-01T00:00:00Z");
    expect(request.expected_version).toBe(3);
  });

  it("forces no-quota policies to zero bytes", () => {
    const request = formToRequest(
      {
        period: "none",
        direction: "rx",
        quotaValue: 99,
        quotaUnit: "GiB",
        expiresAtUtcInput: "",
        version: 0,
      },
      "ticket",
    );
    expect(request.quota_bytes).toBe(0);
    expect(request.expires_at).toBeUndefined();
  });

  it("chooses a lossless display unit", () => {
    const form = policyToForm(
      makePolicy({
        quota_period: "lifetime",
        quota_direction: "tx",
        quota_bytes: 3 * 1024 * 1024,
      }),
    );
    expect(form.quotaUnit).toBe("MiB");
    expect(form.quotaValue).toBe(3);
  });

  it("renders expiry as an exact UTC wall-clock value", () => {
    const form = policyToForm(
      makePolicy({
        expires_at: "2026-12-31T23:59:00Z",
        period_start: "2026-12-01T00:00:00Z",
      }),
    );
    expect(form.expiresAtUtcInput).toBe("2026-12-31T23:59");
  });

  it("never clears or truncates an expiry that the mutation API cannot accept", () => {
    for (const expiry of [
      "infinity",
      "-infinity",
      "+294276-12-31T23:59:59.999999Z",
      "2026-12-31T23:59:59.123456Z",
    ]) {
      const form = policyToForm(makePolicy({ expires_at: expiry }));
      expect(form.expiresAtUtcInput).toBe(expiry.replace(/Z$/, ""));
      expect(expiryInputType(form.expiresAtUtcInput)).toBe("text");
      expect(() => formToRequest(form, "ticket")).toThrow("whole seconds");
      form.expiresAtUtcInput = "2026-12-31T23:59:59";
      expect(formToRequest(form, "ticket").expires_at).toBe(
        "2026-12-31T23:59:59Z",
      );
      form.expiresAtUtcInput = "";
      expect(formToRequest(form, "ticket").expires_at).toBeUndefined();
    }
  });
});
