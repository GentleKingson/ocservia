import { expect, test } from "@playwright/test";

const workspaceId = "019fc0a4-6d92-765c-a8a1-4af556614dd1";
const nodeId = "019fc0a4-6d92-765c-a8a1-4af556614dd2";
const planId = "019fc0a4-6d92-765c-a8a1-4af556614dd3";
const node = {
  id: nodeId,
  name: "Config node",
  version: 1,
  trust_status: "active",
  connection_state: "online",
  freshness: "fresh",
  dropped: { security: 0, health: 0, aggregate: 0, raw: 0 },
  session_count: 0,
};

test("submits a typed configuration plan and renders a safe diff", async ({
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
            name: "Config workspace",
            slug: "config",
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
  await page.route(`**/api/v1/nodes/${nodeId}/sessions?**`, (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/json",
      body: '{"items":[],"page":{"has_more":false}}',
    }),
  );
  await page.route(`**/api/v1/nodes/${nodeId}/ip-bans`, (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/json",
      body: '{"items":[]}',
    }),
  );
  await page.route(`**/api/v1/nodes/${nodeId}/user-group-state`, (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/json",
      body: '{"items":[]}',
    }),
  );

  let submitted: Record<string, unknown> | undefined;
  const plan = {
    id: planId,
    workspace_id: workspaceId,
    node_id: nodeId,
    operation_id: planId,
    template_name: "node-baseline",
    expected_revision: 0,
    candidate_hash: "a".repeat(64),
    state: "succeeded",
    validation: "valid",
    diff_redacted:
      "- <current configuration redacted>\n+ server-key = <secret-ref:node>\n+ tcp-port = 443\n",
    warnings: [],
    current_unchanged: true,
    staging_cleaned: true,
    expires_at: "2026-08-08T01:00:00Z",
    created_at: "2026-08-08T00:45:00Z",
  };
  await page.route(`**/api/v1/nodes/${nodeId}/config-plans`, async (route) => {
    submitted = (await route.request().postDataJSON()) as Record<
      string,
      unknown
    >;
    await route.fulfill({
      status: 202,
      contentType: "application/json",
      body: JSON.stringify(plan),
    });
  });

  await page.goto("/nodes");
  await page.getByText("Config node").click();
  await page.getByTitle("Configuration plan").click();
  await page.getByLabel("TCP port").fill("8443");
  await page.getByLabel("Maximum clients").fill("256");
  await page.getByLabel("Reason").fill("review edge configuration");
  await page.getByRole("button", { name: "Plan", exact: true }).last().click();

  await expect(page.getByText("valid", { exact: true })).toBeVisible();
  await expect(page.locator("pre")).toContainText("tcp-port = 443");
  await expect(page.locator("pre")).not.toContainText("tls/server-private-key");
  expect(submitted).toMatchObject({
    expected_revision: 0,
    ttl_seconds: 900,
    reason: "review edge configuration",
    template: {
      directives: expect.arrayContaining([
        {
          name: "server-key",
          secret_ref: {
            provider: "node",
            key: "tls/server-private-key",
          },
        },
      ]),
    },
  });
  expect(JSON.stringify(submitted)).not.toContain("target_path");
});
