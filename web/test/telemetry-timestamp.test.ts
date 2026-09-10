import { describe, expect, it } from "vitest";

import {
  AgentRolloutFromJSON,
  ApprovalFromJSON,
  ArtifactGrantFromJSON,
  CertificateFromJSON,
  ConfigPlanFromJSON,
  ConfigPlanApprovalSummaryFromJSON,
  NodeObservedStateFromJSON,
  NodeSessionFromJSON,
  OperationFromJSON,
  PlatformEventFromJSON,
  PlatformEventToJSON,
  SecretProviderRefFromJSON,
  TelemetryPointFromJSON,
  TelemetryPointToJSON,
  UserGroupResourceStateFromJSON,
  UserGroupResourceStateToJSON,
  UserPolicyFromJSON,
  UserPolicyToJSON,
  UserBatchFromJSON,
  UserBatchToJSON,
} from "@ocservia/api-client";
import { formatTimestamp } from "../src/shared/timestamp";

describe("telemetry timestamps", () => {
  it("retains policy and batch clocks without finite Date coercion", () => {
    const policy = UserPolicyFromJSON({
      expires_at: "infinity",
      period_start: "1970-01-01T00:00:00Z",
      observed_at: "+294276-12-31T23:59:59.999999Z",
    });
    expect(UserPolicyToJSON(policy)).toMatchObject({
      expires_at: "infinity",
      period_start: "1970-01-01T00:00:00Z",
      observed_at: "+294276-12-31T23:59:59.999999Z",
    });
    const batch = UserBatchFromJSON({
      items: [],
      created_at: "-infinity",
      updated_at: "infinity",
    });
    expect(UserBatchToJSON(batch)).toMatchObject({
      created_at: "-infinity",
      updated_at: "infinity",
    });
  });
  it("retains resource array dimensions, NULL members and observation time", () => {
    const wire = {
      kind: "group",
      name: "staff",
      convergence: "converged",
      recovery_required: false,
      desired_members: ["alpha", null, "", "omega"],
      observed_members: ["alpha", null, "", "omega"],
      desired_member_dimensions: [
        { length: 2, lower_bound: -1 },
        { length: 2, lower_bound: 5 },
      ],
      observed_member_dimensions: [
        { length: 2, lower_bound: -1 },
        { length: 2, lower_bound: 5 },
      ],
      observed_at: "infinity",
    };
    const resource = UserGroupResourceStateFromJSON(wire);
    const members: Array<string | null> | undefined = resource.desiredMembers;
    expect(members).toEqual(wire.desired_members);
    expect(resource.observedAt).toBe("infinity");
    expect(UserGroupResourceStateToJSON(resource)).toEqual(wire);
  });
  it("retains approval expiry and creation clocks", () => {
    const approval = ApprovalFromJSON({
      expires_at: "infinity",
      created_at: "-infinity",
    });
    expect(approval.expiresAt).toBe("infinity");
    expect(approval.createdAt).toBe("-infinity");
  });
  it("retains rollout clocks and complete stored exclusions", () => {
    const excluded = [null, ["nested"], { node_id: "node", extra: true }];
    const rollout = AgentRolloutFromJSON({
      excluded,
      created_at: "-infinity",
      updated_at: "infinity",
    });
    expect(rollout.excluded).toEqual(excluded);
    expect(rollout.createdAt).toBe("-infinity");
    expect(rollout.updatedAt).toBe("infinity");
  });
  it("retains configuration clocks and complete stored warning arrays", () => {
    const warnings = ["safe", null, [1.25], { value: true }];
    const plan = ConfigPlanFromJSON({
      warnings,
      created_at: "-infinity",
      expires_at: "infinity",
    });
    expect(plan.warnings).toEqual(warnings);
    expect(plan.createdAt).toBe("-infinity");
    expect(plan.expiresAt).toBe("infinity");
    expect(
      ConfigPlanApprovalSummaryFromJSON({ expires_at: "infinity" }).expiresAt,
    ).toBe("infinity");
  });
  it("retains certificate timestamps and the complete DNS JSON array", () => {
    const dnsNames = ["example.test", null, ["nested"], { extra: true }];
    const certificate = CertificateFromJSON({
      dns_names: dnsNames,
      not_before: "-infinity",
      not_after: "infinity",
      created_at: "+294276-12-31T23:59:59.999999Z",
      updated_at: "2026-09-10T04:34:56.123456Z",
    });
    expect(certificate.dnsNames).toEqual(dnsNames);
    expect(certificate.notBefore).toBe("-infinity");
    expect(certificate.notAfter).toBe("infinity");
    expect(certificate.createdAt).toBe("+294276-12-31T23:59:59.999999Z");
    expect(certificate.updatedAt).toBe("2026-09-10T04:34:56.123456Z");
    expect(ArtifactGrantFromJSON({ expires_at: "infinity" }).expiresAt).toBe(
      "infinity",
    );
    expect(
      SecretProviderRefFromJSON({ rotated_at: "-infinity" }).rotatedAt,
    ).toBe("-infinity");
    expect(OperationFromJSON({ expires_at: "infinity" }).expiresAt).toBe(
      "infinity",
    );
  });
  it("retains extended node/session values and displays infinity literally", () => {
    const node = NodeObservedStateFromJSON({
      observed_at: "infinity",
      last_heartbeat_at: "-infinity",
    });
    expect(node.observedAt).toBe("infinity");
    expect(node.lastHeartbeatAt).toBe("-infinity");
    expect(
      NodeSessionFromJSON({ connected_at: "+294276-12-31T23:59:59.999999Z" })
        .connectedAt,
    ).toBe("+294276-12-31T23:59:59.999999Z");
    expect(formatTimestamp("infinity")).toBe("infinity");
    expect(formatTimestamp("-infinity")).toBe("-infinity");
    expect(formatTimestamp("-004713-11-24T00:00:00Z")).toBe(
      "-004713-11-24T00:00:00Z",
    );
  });
  it.each([
    "infinity",
    "-infinity",
    "2026-09-10T04:34:56.123456Z",
    "+294276-12-31T23:59:59.999999Z",
    "-004713-11-24T00:00:00Z",
  ])("preserves %s without Date coercion or precision loss", (at) => {
    const wire = {
      at,
      metric: "cpu_usage_ratio",
      count: 1,
      minimum: 7,
      maximum: 7,
      average: 7,
    };
    const point = TelemetryPointFromJSON(wire);
    expect(point.at).toBe(at);
    expect(TelemetryPointToJSON(point)).toEqual(wire);
    const eventWire = {
      id: "event",
      node_id: "node",
      type: "heartbeat",
      traceparent: "trace",
      occurred_at: at,
    };
    const event = PlatformEventFromJSON(eventWire);
    expect(event.occurredAt).toBe(at);
    expect(PlatformEventToJSON(event)).toEqual(eventWire);
  });
});
