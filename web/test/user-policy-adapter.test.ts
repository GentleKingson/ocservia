import { ResponseError } from "@ocservia/api-client";
import { describe, expect, it, vi } from "vitest";

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
} from "../src/adapters/user-policy";

describe("upstream user policy adapter", () => {
  it.each([
    ["generated", new ResponseError(new Response(null, { status: 404 }))],
    ["native", new Response(null, { status: 404 })],
  ])("loads the default form for a %s 404", async (_kind, error) => {
    vi.mocked(getUserPolicy).mockRejectedValueOnce(error);
    await expect(loadUserPolicy("node", "alice")).resolves.toEqual(
      policyToForm(),
    );
  });

  it.each([
    [
      "generated non-404",
      new ResponseError(new Response(null, { status: 403 })),
    ],
    ["native non-404", new Response(null, { status: 500 })],
    ["null", null],
    ["undefined", undefined],
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
    expect(request.quotaBytes).toBe(2_684_354_560);
    expect(request.expiresAt?.toISOString()).toBe("2026-09-01T00:00:00.000Z");
    expect(request.expectedVersion).toBe(3);
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
    expect(request.quotaBytes).toBe(0);
    expect(request.expiresAt).toBeUndefined();
  });

  it("chooses a lossless display unit", () => {
    const form = policyToForm({
      nodeId: "019fdc5b-b939-72a1-ae67-8efd197e5688",
      username: "alice",
      quotaPeriod: "lifetime",
      quotaDirection: "tx",
      quotaBytes: 3 * 1024 * 1024,
      version: 1,
      periodStart: "1970-01-01T00:00:00Z",
      observedRxBytes: 0,
      observedTxBytes: 0,
      exceeded: false,
      expired: false,
      convergence: "converged",
    });
    expect(form.quotaUnit).toBe("MiB");
    expect(form.quotaValue).toBe(3);
  });

  it("renders expiry as an exact UTC wall-clock value", () => {
    const form = policyToForm({
      nodeId: "019fdc5b-b939-72a1-ae67-8efd197e5688",
      username: "alice",
      quotaPeriod: "none",
      quotaDirection: "rxtx",
      quotaBytes: 0,
      expiresAt: "2026-12-31T23:59:00Z",
      version: 1,
      periodStart: "2026-12-01T00:00:00Z",
      observedRxBytes: 0,
      observedTxBytes: 0,
      exceeded: false,
      expired: false,
      convergence: "converged",
    });
    expect(form.expiresAtUtcInput).toBe("2026-12-31T23:59");
  });

  it("never clears or truncates an expiry that the mutation API cannot accept", () => {
    for (const expiry of [
      "infinity",
      "-infinity",
      "+294276-12-31T23:59:59.999999Z",
      "2026-12-31T23:59:59.123456Z",
    ]) {
      const form = policyToForm({
        nodeId: "019fdc5b-b939-72a1-ae67-8efd197e5688",
        username: "alice",
        quotaPeriod: "none",
        quotaDirection: "rxtx",
        quotaBytes: 0,
        expiresAt: expiry,
        version: 1,
        periodStart: "1970-01-01T00:00:00Z",
        observedRxBytes: 0,
        observedTxBytes: 0,
        exceeded: false,
        expired: false,
        convergence: "converged",
      });
      expect(form.expiresAtUtcInput).toBe(expiry.replace(/Z$/, ""));
      expect(expiryInputType(form.expiresAtUtcInput)).toBe("text");
      expect(() => formToRequest(form, "ticket")).toThrow("whole seconds");
      form.expiresAtUtcInput = "2026-12-31T23:59:59";
      expect(formToRequest(form, "ticket").expiresAt?.toISOString()).toBe(
        "2026-12-31T23:59:59.000Z",
      );
      form.expiresAtUtcInput = "";
      expect(formToRequest(form, "ticket").expiresAt).toBeUndefined();
    }
  });
});
