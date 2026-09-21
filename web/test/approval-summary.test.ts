import { ApprovalFromJSON, ApprovalToJSON } from "@ocservia/api-client";
import { describe, expect, it } from "vitest";

describe("immutable approval content", () => {
  it("preserves action-specific object content when reading and serializing", () => {
    const summary = {
      node_id: "01900000-0000-7000-8000-000000000001",
      target_version: "0.7.0",
      expected_revision: 7,
      package_sha256: "a".repeat(64),
      nested_binding: { enabled: false, count: 0, optional: null },
      future_field: ["one", "two"],
    };
    const approval = ApprovalFromJSON({ request_summary: summary });
    expect(approval.requestSummary).toEqual(summary);
    expect(ApprovalToJSON(approval)).toMatchObject({
      request_summary: summary,
    });
  });

  it("retains the typed batch summary branch", () => {
    const summary = [
      {
        node_id: "01900000-0000-7000-8000-000000000001",
        username: "alice",
        action: "disable",
        expected_version: 7,
      },
    ];
    const approval = ApprovalFromJSON({ request_summary: summary });
    expect(approval.requestSummary).toEqual([
      {
        nodeId: "01900000-0000-7000-8000-000000000001",
        username: "alice",
        action: "disable",
        expectedVersion: 7,
      },
    ]);
    expect(ApprovalToJSON(approval)).toMatchObject({
      request_summary: summary,
    });
  });

  it("does not invent absent review content", () => {
    expect(ApprovalFromJSON({}).requestSummary).toBeUndefined();
    expect(
      ApprovalFromJSON({ request_summary: null }).requestSummary,
    ).toBeUndefined();
  });

  it("keeps certificate model-union conversion unchanged", () => {
    const summary = {
      certificate_id: "01900000-0000-7000-8000-000000000002",
      node_id: "01900000-0000-7000-8000-000000000001",
      common_name: "vpn.test",
      dns_names: ["vpn.test"],
      csr_sha256: "b".repeat(64),
    };
    const approval = ApprovalFromJSON({ certificate_summary: summary });
    expect(approval.certificateSummary).toEqual({
      certificateId: summary.certificate_id,
      nodeId: summary.node_id,
      commonName: "vpn.test",
      dnsNames: ["vpn.test"],
      csrSha256: summary.csr_sha256,
    });
    expect(ApprovalToJSON(approval)).toMatchObject({
      certificate_summary: summary,
    });
  });
});
