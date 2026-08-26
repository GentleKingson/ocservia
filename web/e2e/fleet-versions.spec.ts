import { expect, test } from "@playwright/test";

const workspaceId = "019fc0a4-6d92-765c-a8a1-4af556614ed1";
const currentNodeId = "019fc0a4-6d92-765c-a8a1-4af556614ed2";
const updateNodeId = "019fc0a4-6d92-765c-a8a1-4af556614ed3";
const unknownNodeId = "019fc0a4-6d92-765c-a8a1-4af556614ed4";
const aheadNodeId = "019fc0a4-6d92-765c-a8a1-4af556614ed5";
const recommended = "0.2.0";
const nodeRow = (id: string, name: string, extra: Record<string, unknown>) => ({
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
const nodes = [
  nodeRow(currentNodeId, "node-current", {
    agent_version: "0.2.0",
    agent_version_state: "current",
  }),
  nodeRow(updateNodeId, "node-update", {
    agent_version: "0.1.1",
    agent_version_state: "upgrade_available",
    ocserv_version: "1.3.0",
    os_release: "Debian GNU/Linux 12",
  }),
  nodeRow(unknownNodeId, "node-unknown", {
    agent_version_state: "unknown",
  }),
  nodeRow(aheadNodeId, "node-ahead", {
    agent_version: "0.3.0",
    agent_version_state: "ahead",
  }),
];

test.beforeEach(async ({ page }) => {
  // Replace the event stream with a inert stub so a closing mock cannot flip
  // the fleet into its unavailable state during assertions.
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
          { id: workspaceId, name: "Versions", slug: "versions", version: 1 },
        ],
      }),
    }),
  );
  await page.route("**/api/v1/nodes?**", (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ items: nodes, page: { has_more: false } }),
    }),
  );
  await page.route(`**/api/v1/nodes/${updateNodeId}`, (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(nodes[1]),
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

test("shows server-derived version badges on the fleet list", async ({
  page,
}) => {
  await page.goto("/nodes");

  const currentRow = page.locator("tr", { hasText: "node-current" });
  await expect(currentRow.locator(".version-badge")).toHaveText("Current");
  await expect(currentRow).toContainText("0.2.0");
  const updateRow = page.locator("tr", { hasText: "node-update" });
  await expect(updateRow.locator(".version-badge")).toHaveText(
    "Update available",
  );
  const unknownRow = page.locator("tr", { hasText: "node-unknown" });
  await expect(unknownRow.locator(".version-badge")).toHaveText("Unknown");
  const aheadRow = page.locator("tr", { hasText: "node-ahead" });
  await expect(aheadRow.locator(".version-badge")).toHaveText("Ahead");
});

test("summarizes version states on the overview dashboard", async ({
  page,
}) => {
  await page.goto("/");

  await expect(page.getByTestId("overview-agent-versions")).toHaveText("1");
  // Every classified bucket is visible so an ahead-only fleet cannot read as
  // an unclassified one.
  await expect(
    page.getByTestId("overview-agent-versions").locator(".."),
  ).toContainText(/1 Update available · 1 Ahead · 1 Unknown/);
});

test("shows observed, recommended, and state on node detail", async ({
  page,
}) => {
  await page.goto(`/nodes/${updateNodeId}`);

  await expect(page.locator("h1")).toHaveText("node-update");
  const agentRow = page.locator("dl div", {
    has: page.getByText("Agent", { exact: true }),
  });
  await expect(agentRow).toContainText("0.1.1");
  await expect(agentRow.locator(".version-badge")).toHaveText(
    "Update available",
  );
  await expect(
    page.locator("dl div", { has: page.getByText("Version state") }),
  ).toContainText("Update available");
  await expect(
    page.locator("dl div", {
      has: page.getByText("Recommended Agent version"),
    }),
  ).toContainText(recommended);
  await expect(
    page.locator("dl div", { has: page.getByText("OS release") }),
  ).toContainText("Debian GNU/Linux 12");
});

test("shows the recommended agent version in settings", async ({ page }) => {
  await page.goto("/settings");

  await expect(
    page.locator("dl div", {
      has: page.getByText("Recommended Agent version"),
    }),
  ).toContainText(recommended);
});
