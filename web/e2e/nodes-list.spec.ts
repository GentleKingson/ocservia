import { expect, test } from "@playwright/test";

const workspaceId = "019fc0a4-6d92-765c-a8a1-4af556614fa1";
const longName =
  "edge-gateway-with-an-exceptionally-long-descriptive-name-for-the-frankfurt-region";
const nodeRow = (
  suffix: string,
  name: string,
  extra: Record<string, unknown> = {},
) => ({
  id: `019fc0a4-6d92-765c-a8a1-4af556614f${suffix}`,
  name,
  version: 1,
  trust_status: "active",
  connection_state: "online",
  freshness: "fresh",
  dropped: { security: 0, health: 0, aggregate: 0, raw: 0 },
  session_count: 2,
  recommended_agent_version: "0.2.0",
  agent_version: "0.1.1",
  agent_version_state: "upgrade_available",
  agent_upgrade_eligible: true,
  last_heartbeat_at: "2026-01-01T00:00:00.123456Z",
  path: { mode: "direct", rtt_ms: 4 },
  ...extra,
});
// Two pages: filters and counts must cover the whole fleet.
const firstPage = [
  nodeRow("b1", "alpha"),
  nodeRow("b2", longName, { path: { mode: "relay", rtt_ms: 41 } }),
];
const secondPage = [
  nodeRow("b3", "stale-node", {
    connection_state: "offline",
    freshness: "stale",
    agent_upgrade_eligible: false,
    agent_version_state: "current",
    agent_version: "0.2.0",
  }),
  nodeRow("b4", "unknown-node", {
    trust_status: "pending",
    freshness: "never",
    agent_upgrade_eligible: false,
    agent_version: undefined,
    agent_version_state: undefined,
    last_heartbeat_at: undefined,
    path: undefined,
  }),
];

test.beforeEach(async ({ page }) => {
  await page.addInitScript(() => {
    class EventSourceStub {
      readonly readyState = 1;
      onerror = null;
      onmessage = null;
      onopen = null;
      addEventListener(): void {}
      removeEventListener(): void {}
      close(): void {}
    }
    Object.defineProperty(window, "EventSource", { value: EventSourceStub });
  });
  await page.route("**/api/v1/readyz", (route) => route.fulfill({ json: {} }));
  await page.route("**/api/v1/workspaces", (route) =>
    route.fulfill({
      json: {
        items: [{ id: workspaceId, name: "Fleet", slug: "fleet", version: 1 }],
      },
    }),
  );
  await page.route("**/api/v1/nodes?**", (route) => {
    const cursor = new URL(route.request().url()).searchParams.get("cursor");
    return route.fulfill({
      json: cursor
        ? { items: secondPage, page: { has_more: false } }
        : { items: firstPage, page: { has_more: true, next_cursor: "next" } },
    });
  });
});

test("filters the complete fleet through the URL without writes", async ({
  page,
}) => {
  const writes: string[] = [];
  page.on("request", (request) => {
    if (request.method() !== "GET") writes.push(request.url());
  });
  await page.goto("/nodes?keep=1");
  await expect(page.getByText("Showing 4 of 4 nodes")).toBeVisible();
  await expect(page.getByText("Counts cover all 4 nodes")).toBeVisible();
  await expect(page.getByRole("link", { name: longName })).toBeVisible();
  const unknownRow = page.locator("tr", { hasText: "unknown-node" });
  await expect(unknownRow).toContainText("Not observed");
  await expect(unknownRow.getByTestId("agent-version-state")).toHaveText(
    "No version observation",
  );

  await page.getByRole("button", { name: "Filters" }).click();
  await page.getByRole("menuitemcheckbox", { name: /^Fresh/ }).click();
  await page.getByRole("menuitemcheckbox", { name: /^Stale/ }).click();
  await expect(page.getByText("Showing 3 of 4 nodes")).toBeVisible();
  await page.getByRole("menuitemcheckbox", { name: /^Relay/ }).click();
  await expect(page.getByText("Showing 1 of 4 nodes")).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(page.getByRole("menu")).toHaveCount(0);
  await expect(page).toHaveURL(/keep=1/);
  await expect(page).toHaveURL(/freshness=fresh%2Cstale|freshness=fresh,stale/);
  await expect(page).toHaveURL(/path=relay/);

  await page.reload();
  await expect(page.getByText("Showing 1 of 4 nodes")).toBeVisible();
  await expect(page.getByRole("link", { name: longName })).toBeVisible();

  await page.getByRole("link", { name: longName }).click();
  await expect(page).toHaveURL(/\/nodes\/019fc0a4/);
  await page.goBack();
  await expect(page.getByText("Showing 1 of 4 nodes")).toBeVisible();

  await page.getByRole("button", { name: "Remove filter Path: Relay" }).click();
  await expect(page.getByText("Showing 3 of 4 nodes")).toBeVisible();
  await expect(page).not.toHaveURL(/path=relay/);

  await page.getByLabel("Search nodes").fill("nothing-matches");
  await expect(
    page.getByText("No nodes match the current filters."),
  ).toBeVisible();
  await page.getByRole("button", { name: "Clear filters" }).last().click();
  await expect(page.getByText("Showing 4 of 4 nodes")).toBeVisible();
  await expect(page).toHaveURL(/\/nodes\?keep=1$/);
  expect(writes).toEqual([]);
});

test("reports selected nodes hidden by filters", async ({ page }) => {
  await page.goto("/nodes");
  await page
    .getByRole("checkbox", {
      name: "Select visible eligible nodes for rolling upgrade",
    })
    .check();
  await expect(
    page.getByRole("button", { name: "Rolling upgrade (2)" }),
  ).toBeEnabled();
  await page.getByLabel("Search nodes").fill("alpha");
  await expect(
    page.getByText("2 selected · 1 hidden by filters"),
  ).toBeVisible();
  await page.getByRole("button", { name: "Rolling upgrade (2)" }).click();
  await expect(page.locator("#rollout-nodes")).toContainText(longName);
});

test("keeps wide tables inside their own scroll region", async ({ page }) => {
  await page.goto("/nodes?columns=path,ocserv,platform&sort=heartbeat");
  await expect(
    page.getByRole("columnheader", { name: "Arch / OS" }),
  ).toBeVisible();
  // Unknown heartbeats sort after known ones.
  await expect(page.locator("tbody tr").last()).toContainText("unknown-node");
  expect(
    await page.evaluate(
      () =>
        document.documentElement.scrollWidth -
        document.documentElement.clientWidth,
    ),
  ).toBeLessThanOrEqual(0);
});
