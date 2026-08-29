import { expect, test } from "@playwright/test";

const workspaceId = "019fc0a4-6d92-765c-a8a1-4af556614cc1";
const nodeId = "019fc0a4-6d92-765c-a8a1-4af556614cc2";
const node = {
  id: nodeId,
  name: "Policy node",
  version: 1,
  trust_status: "active",
  connection_state: "online",
  freshness: "fresh",
  dropped: { security: 0, health: 0, aggregate: 0, raw: 0 },
  session_count: 0,
};

test("sets an upstream quota form through the node-scoped adapter", async ({
  page,
}) => {
  await page.route("**/api/v1/readyz", (route) =>
    route.fulfill({ status: 200, contentType: "application/json", body: "{}" }),
  );
  await page.route("**/api/v1/workspaces", (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        items: [
          {
            id: workspaceId,
            name: "Policy workspace",
            slug: "policy",
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
      body: JSON.stringify({ items: [], page: { has_more: false } }),
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
      body: JSON.stringify({
        items: [
          {
            kind: "user",
            name: "alice",
            desired_enabled: true,
            desired_version: 1,
            desired_revision: 1,
            convergence: "converged",
            recovery_required: false,
          },
        ],
      }),
    }),
  );

  let submitted: Record<string, unknown> | undefined;
  await page.route(
    `**/api/v1/nodes/${nodeId}/users/alice/policy`,
    async (route) => {
      if (route.request().method() === "GET") {
        await route.fulfill({
          status: 404,
          contentType: "application/problem+json",
          body: "{}",
        });
        return;
      }
      submitted = (await route.request().postDataJSON()) as Record<
        string,
        unknown
      >;
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          node_id: nodeId,
          username: "alice",
          quota_period: "monthly",
          quota_direction: "rxtx",
          quota_bytes: 2_147_483_648,
          expires_at: "2026-12-31T23:59:00Z",
          version: 1,
          period_start: "2026-08-01T00:00:00Z",
          observed_rx_bytes: 0,
          observed_tx_bytes: 0,
          exceeded: false,
          expired: false,
          convergence: "converged",
        }),
      });
    },
  );

  await page.goto("/nodes");
  await page.getByText("Policy node").click();
  const policyLoaded = page.waitForResponse((response) =>
    response.url().endsWith(`/api/v1/nodes/${nodeId}/users/alice/policy`),
  );
  await page.getByTitle("Quota and expiry").click();
  await policyLoaded;
  await page.getByLabel("Quota period").selectOption("monthly");
  await page.getByLabel("Quota size").fill("2");
  await page.getByLabel("Quota unit").selectOption("GiB");
  await page.getByLabel("Expires at (UTC)").fill("2026-12-31T23:59");
  await page.getByLabel("Reason").fill("capacity policy");
  await page.getByRole("button", { name: "Confirm" }).click();

  await expect(
    page.getByRole("heading", { name: "Quota and expiry" }),
  ).toHaveCount(0);
  expect(submitted).toMatchObject({
    quota_period: "monthly",
    quota_direction: "rxtx",
    quota_bytes: 2_147_483_648,
    expires_at: "2026-12-31T23:59:00.000Z",
    expected_version: 0,
    reason: "capacity policy",
  });
});
