import { describe, expect, it } from "vitest";

import { formToRequest, policyToForm } from "../src/adapters/user-policy";

describe("upstream user policy adapter", () => {
  it("maps display units to integer bytes and UTC expiry", () => {
    const request = formToRequest(
      {
        period: "monthly",
        direction: "rxtx",
        quotaValue: 2.5,
        quotaUnit: "GiB",
        expiresAtLocal: "2026-09-01T00:00",
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
        expiresAtLocal: "",
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
      periodStart: new Date("1970-01-01T00:00:00Z"),
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
      expiresAt: new Date("2026-12-31T23:59:00Z"),
      version: 1,
      periodStart: new Date("2026-12-01T00:00:00Z"),
      observedRxBytes: 0,
      observedTxBytes: 0,
      exceeded: false,
      expired: false,
      convergence: "converged",
    });
    expect(form.expiresAtLocal).toBe("2026-12-31T23:59");
  });
});
