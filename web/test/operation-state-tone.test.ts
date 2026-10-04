import type { Operation } from "@ocservia/api-client";
import { describe, expect, it } from "vitest";

import {
  approvalTone,
  auditResultTone,
  operationTone,
  rolloutNodeTone,
  rolloutTone,
} from "../src/features/operations/state-tone";

const operation = (state: Operation["state"], upgrade = false): Operation => ({
  id: "op",
  state,
  version: 1,
  createdAt: "2026-10-04T00:00:00Z",
  updatedAt: "2026-10-04T00:00:00Z",
  ...(upgrade ? { agentUpgradeState: state as never } : {}),
});

describe("operation state tones", () => {
  it("keeps a plain unknown recoverable but an upgrade unknown terminal", () => {
    expect(operationTone(operation("unknown"))).toBe("warning");
    expect(operationTone(operation("unknown", true))).toBe("danger");
  });

  it("never renders an accepted or running operation as success", () => {
    for (const state of [
      "draft",
      "queued",
      "dispatched",
      "accepted",
      "running",
      "offline_pending",
      "superseded",
    ] as const)
      expect(operationTone(operation(state))).toBe("neutral");
    expect(operationTone(operation("accepted", true))).toBe("neutral");
    expect(operationTone(operation("succeeded"))).toBe("success");
    for (const state of ["failed", "expired", "drifted"] as const)
      expect(operationTone(operation(state))).toBe("danger");
    expect(operationTone(operation("rolled_back"))).toBe("warning");
  });

  it("maps every rollout and rollout node state", () => {
    expect(rolloutTone("succeeded")).toBe("success");
    expect(rolloutTone("failed")).toBe("danger");
    expect(rolloutTone("paused")).toBe("warning");
    for (const state of ["queued", "running", "cancelled"] as const)
      expect(rolloutTone(state)).toBe("neutral");
    expect(rolloutNodeTone("succeeded")).toBe("success");
    for (const state of ["failed", "rolled_back", "unknown"] as const)
      expect(rolloutNodeTone(state)).toBe("danger");
    for (const state of ["pending", "running", "skipped"] as const)
      expect(rolloutNodeTone(state)).toBe("neutral");
  });
});

describe("approval and audit tones", () => {
  it("never shows an approval as finished work or an intent as success", () => {
    expect(approvalTone("pending")).toBe("warning");
    expect(approvalTone("approved")).toBe("success");
    expect(approvalTone("consumed")).toBe("neutral");
    expect(approvalTone("rejected")).toBe("danger");
    expect(approvalTone("expired")).toBe("danger");
    expect(auditResultTone("succeeded")).toBe("success");
    expect(auditResultTone("failed")).toBe("danger");
    expect(auditResultTone("intent")).toBe("neutral");
    expect(auditResultTone("future")).toBe("neutral");
  });
});
