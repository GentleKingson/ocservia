import { expect, test } from "@playwright/test";

const workspaceId = "019fde50-1111-7111-8111-111111111111";
const nodeId = "019fde50-2222-7222-8222-222222222222";
const certificateId = "019fde50-3333-7333-8333-333333333333";
const approvalId = "019fde50-4444-7444-8444-444444444444";
const operationId = "019fde50-5555-7555-8555-555555555555";
const artifactId = "019fde50-6666-7666-8666-666666666666";
const node = {
  id: nodeId,
  name: "Certificate node",
  version: 1,
  trust_status: "active",
  connection_state: "online",
  freshness: "fresh",
  dropped: { security: 0, health: 0, aggregate: 0, raw: 0 },
  session_count: 0,
};

test("issues a node-local CSR and downloads a one-time P12", async ({
  page,
}) => {
  await page.route("**/api/v1/readyz", (route) =>
    route.fulfill({ status: 200, contentType: "application/json", body: "{}" }),
  );
  await page.route("**/api/v1/events/stream?**", (route) =>
    route.fulfill({ status: 200, contentType: "text/event-stream", body: "" }),
  );
  await page.route("**/api/v1/workspaces", (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        items: [
          {
            id: workspaceId,
            name: "Certificate workspace",
            slug: "certificate",
            version: 1,
          },
        ],
      }),
    }),
  );
  await page.route("**/api/v1/nodes?**", (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ items: [node], page: { has_more: false } }),
    }),
  );
  await page.route(`**/api/v1/nodes/${nodeId}`, (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(node),
    }),
  );
  for (const suffix of ["sessions?**", "ip-bans", "user-group-state"]) {
    await page.route(`**/api/v1/nodes/${nodeId}/${suffix}`, (route) =>
      route.fulfill({
        status: 200,
        contentType: "application/json",
        body: '{"items":[],"page":{"has_more":false}}',
      }),
    );
  }
  let csrRequest: Record<string, unknown> | undefined;
  await page.route(`**/api/v1/nodes/${nodeId}/certificates`, async (route) => {
    if (route.request().method() === "GET") {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: '{"items":[]}',
      });
      return;
    }
    csrRequest = (await route.request().postDataJSON()) as Record<
      string,
      unknown
    >;
    await route.fulfill({
      status: 202,
      contentType: "application/json",
      body: JSON.stringify({
        id: certificateId,
        workspace_id: workspaceId,
        node_id: nodeId,
        operation_id: operationId,
        common_name: "vpn.example.test",
        dns_names: ["alt.example.test"],
        key_bits: 3072,
        state: "csr_ready",
        public_key_sha256: "a".repeat(64),
        created_at: "2026-08-08T01:00:00Z",
        updated_at: "2026-08-08T01:00:01Z",
      }),
    });
  });
  await page.route(
    `**/api/v1/certificates/${certificateId}:issue`,
    async (route) => {
      expect(await route.request().postDataJSON()).toEqual({
        approval_id: approvalId,
        reason: "issue approved certificate",
      });
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          id: certificateId,
          workspace_id: workspaceId,
          node_id: nodeId,
          operation_id: operationId,
          common_name: "vpn.example.test",
          dns_names: ["alt.example.test"],
          key_bits: 3072,
          state: "issued",
          serial_number: "42",
          not_after: "2026-09-08T01:00:00Z",
          created_at: "2026-08-08T01:00:00Z",
          updated_at: "2026-08-08T01:00:02Z",
        }),
      });
    },
  );
  await page.route(`**/api/v1/certificates/${certificateId}:p12`, (route) =>
    route.fulfill({
      status: 202,
      contentType: "application/json",
      body: JSON.stringify({
        artifact_id: artifactId,
        operation: {
          id: operationId,
          state: "succeeded",
          node_id: nodeId,
          version: 1,
          created_at: "2026-08-08T01:00:03Z",
          updated_at: "2026-08-08T01:00:04Z",
        },
        download_token: "t".repeat(43),
        password: "one-time-password",
        expires_at: "2026-08-08T01:10:03Z",
      }),
    }),
  );
  await page.route(`**/api/v1/operations/${operationId}`, (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        id: operationId,
        state: "succeeded",
        node_id: nodeId,
        version: 1,
        created_at: "2026-08-08T01:00:03Z",
        updated_at: "2026-08-08T01:00:04Z",
      }),
    }),
  );
  let artifactToken = "";
  await page.route(`**/api/v1/artifacts/${artifactId}`, async (route) => {
    artifactToken = route.request().headers()["x-artifact-token"] ?? "";
    await route.fulfill({
      status: 200,
      contentType: "application/x-pkcs12",
      body: "encrypted-p12",
    });
  });

  await page.goto("/nodes");
  await page.getByText("Certificate node").click();
  await page.getByTitle("Certificate lifecycle").click();
  await page.getByLabel("Common name").fill("vpn.example.test");
  await page.getByLabel("DNS names (comma separated)").fill("alt.example.test");
  await page.getByLabel("Reason").fill("generate node-local CSR");
  await page.getByRole("button", { name: "Request CSR" }).click();
  await expect(page.getByText("csr_ready", { exact: true })).toBeVisible();
  expect(csrRequest).toMatchObject({
    common_name: "vpn.example.test",
    dns_names: ["alt.example.test"],
    key_bits: 3072,
  });

  await page.getByLabel("Approval ID").fill(approvalId);
  await page.getByLabel("Reason").fill("issue approved certificate");
  await page.getByRole("button", { name: "Issue certificate" }).click();
  await expect(page.getByText("issued", { exact: true })).toBeVisible();
  await page.getByLabel("Reason").fill("create support export");
  await page.getByRole("button", { name: "Create P12" }).click();
  await expect(page.getByLabel("P12 password")).toHaveValue(
    "one-time-password",
  );
  const download = page.waitForEvent("download");
  await page.getByRole("button", { name: "Download" }).click();
  await download;
  expect(artifactToken).toBe("t".repeat(43));
  await expect(page.getByRole("button", { name: "Download" })).toHaveCount(0);
});
