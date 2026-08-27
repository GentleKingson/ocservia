import { expect, test, type Route } from "@playwright/test";

const workspaceId = "019fc0a4-6d92-765c-a8a1-4af556614ee1";
const eligibleNodeId = "019fc0a4-6d92-765c-a8a1-4af556614ee2";
const ineligibleNodeId = "019fc0a4-6d92-765c-a8a1-4af556614ee3";
const operationId = "019fc0a4-6d92-765c-a8a1-4af556614ee4";
const recommended = "0.2.0";
const nodeRow = (
  id: string,
  name: string,
  extra: Record<string, unknown> = {},
) => ({
  id,
  name,
  version: 1,
  trust_status: "active",
  connection_state: "online",
  freshness: "fresh",
  dropped: { security: 0, health: 0, aggregate: 0, raw: 0 },
  session_count: 0,
  recommended_agent_version: recommended,
  ...extra,
});
const eligibleNode = nodeRow(eligibleNodeId, "node-upgrade", {
  agent_version: "0.1.1",
  agent_version_state: "upgrade_available",
  architecture: "amd64",
  agent_upgrade_eligible: true,
});
const ineligibleNode = nodeRow(ineligibleNodeId, "node-current", {
  agent_version: "0.2.0",
  agent_version_state: "current",
  architecture: "amd64",
  agent_upgrade_eligible: false,
});
const operation = (
  state: string,
  upgradeState: string,
): Record<string, unknown> => ({
  id: operationId,
  state,
  version: 1,
  node_id: eligibleNodeId,
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:00:00Z",
  agent_upgrade_state: upgradeState,
  agent_upgrade_target_version: recommended,
});

test.beforeEach(async ({ page }) => {
  // Replace the event stream with an inert stub so a closing mock cannot
  // flip the fleet into its unavailable state during assertions.
  await page.addInitScript(() => {
    class EventSourceStub {
      static readonly CONNECTING = 0;
      static readonly OPEN = 1;
      static readonly CLOSED = 2;
      readonly CONNECTING = 0;
      readonly OPEN = 1;
      readonly CLOSED = 2;
      readonly withCredentials = false;
      readonly readyState = 1;
      readonly url: string;
      onerror: ((event: Event) => void) | null = null;
      onmessage: ((event: MessageEvent) => void) | null = null;
      onopen: ((event: Event) => void) | null = null;
      constructor(url: string | URL) {
        this.url = String(url);
      }
      addEventListener(): void {}
      removeEventListener(): void {}
      dispatchEvent(): boolean {
        return true;
      }
      close(): void {}
    }
    Object.defineProperty(window, "EventSource", { value: EventSourceStub });
  });
  await page.route("**/api/v1/readyz", (route) =>
    route.fulfill({ status: 200, contentType: "application/json", body: "{}" }),
  );
  await page.route("**/api/v1/version", (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        version: "dev",
        commit: "unknown",
        role: "all",
        recommended_agent_version: recommended,
      }),
    }),
  );
  await page.route("**/api/v1/workspaces", (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        items: [
          { id: workspaceId, name: "Upgrade", slug: "upgrade", version: 1 },
        ],
      }),
    }),
  );
  await page.route("**/api/v1/nodes?**", (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        items: [eligibleNode, ineligibleNode],
        page: { has_more: false },
      }),
    }),
  );
  await page.route(`**/api/v1/nodes/${eligibleNodeId}`, (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(eligibleNode),
    }),
  );
  await page.route(`**/api/v1/nodes/${ineligibleNodeId}`, (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(ineligibleNode),
    }),
  );
  await page.route("**/api/v1/nodes/*/sessions?**", (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/json",
      body: '{"items":[],"page":{"has_more":false}}',
    }),
  );
  await page.route("**/api/v1/nodes/*/ip-bans", (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/json",
      body: '{"items":[]}',
    }),
  );
  await page.route("**/api/v1/nodes/*/user-group-state", (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/json",
      body: '{"items":[]}',
    }),
  );
});

// The browser may only name the trusted release; any attempt to smuggle
// package metadata must be refused before the request leaves the page.
async function fulfillUpgradePost(route: Route): Promise<void> {
  const body = route.request().postDataJSON() as Record<string, unknown>;
  if (
    body.target_version !== recommended ||
    typeof body.approval_id !== "string" ||
    body.approval_id.length === 0 ||
    typeof body.reason !== "string" ||
    body.reason.length === 0 ||
    Object.keys(body).some(
      (key) =>
        key !== "target_version" && key !== "approval_id" && key !== "reason",
    )
  ) {
    await route.fulfill({
      status: 400,
      contentType: "application/json",
      body: "{}",
    });
    return;
  }
  await route.fulfill({
    status: 202,
    contentType: "application/json",
    body: JSON.stringify(operation("queued", "queued")),
  });
}

test("walks the reconciled upgrade lifecycle to success", async ({ page }) => {
  await page.route(
    `**/api/v1/nodes/${eligibleNodeId}/agent-upgrade`,
    fulfillUpgradePost,
  );
  const outcomes = [
    operation("accepted", "accepted"),
    operation("running", "running"),
    operation("succeeded", "succeeded"),
  ];
  await page.route(`**/api/v1/operations/${operationId}`, (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(
        outcomes.shift() ?? operation("succeeded", "succeeded"),
      ),
    }),
  );

  await page.goto(`/nodes/${eligibleNodeId}`);
  await page.getByTestId("upgrade-agent").click();
  await expect(page.locator("#upgrade-target")).toHaveText(recommended);
  await page.locator("#operation-reason").fill("monthly maintained release");
  await page
    .locator("#approval-id")
    .fill("019fc0a4-6d92-765c-a8a1-4af556614ee5");
  await page.getByRole("button", { name: "Confirm" }).click();

  const status = page.locator(".operation-status");
  await expect(status.locator("strong")).toHaveText(
    "Waiting for restart & reconnect",
  );
  await expect(status).toContainText(recommended);
  await expect(status.locator("strong")).toHaveText("Verifying target version");
  await expect(status.locator("strong")).toHaveText("Succeeded");
});

test("surfaces a forced local upgrade failure as the terminal outcome", async ({
  page,
}) => {
  await page.route(
    `**/api/v1/nodes/${eligibleNodeId}/agent-upgrade`,
    fulfillUpgradePost,
  );
  const outcomes = [
    operation("accepted", "accepted"),
    operation("failed", "failed"),
  ];
  await page.route(`**/api/v1/operations/${operationId}`, (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(outcomes.shift() ?? operation("failed", "failed")),
    }),
  );

  await page.goto(`/nodes/${eligibleNodeId}`);
  await page.getByTestId("upgrade-agent").click();
  await page
    .locator("#operation-reason")
    .fill("attempt the maintained release");
  await page
    .locator("#approval-id")
    .fill("019fc0a4-6d92-765c-a8a1-4af556614ee6");
  await page.getByRole("button", { name: "Confirm" }).click();

  const status = page.locator(".operation-status");
  await expect(status.locator("strong")).toHaveText(
    "Waiting for restart & reconnect",
  );
  await expect(status.locator("strong")).toHaveText("Failed");
});

test("hides the upgrade entry point for an ineligible node", async ({
  page,
}) => {
  await page.goto(`/nodes/${ineligibleNodeId}`);
  await expect(page.getByTestId("upgrade-agent")).toHaveCount(0);
});
