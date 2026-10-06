import { expect, test } from "@playwright/test";

const workspaceId = "019fc0a4-6d92-765c-a8a1-4af556614fa1";
const bootId = "019fc0a4-6d92-765c-a8a1-4af556614fd0-boot-identifier-long";
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
  session_count: 0,
  agent_version: "0.1.1",
  agent_version_state: "current",
  last_heartbeat_at: "2026-01-01T00:00:00Z",
  path: { mode: "relay", rtt_ms: 41 },
  boot_id: bootId,
  ...extra,
});
const relayNode = nodeRow("c1", "relay-node");
const nodes = [
  relayNode,
  nodeRow("c2", "unknown-node", {
    trust_status: "pending",
    connection_state: "offline",
    freshness: "never",
    agent_version: undefined,
    agent_version_state: undefined,
    last_heartbeat_at: undefined,
    path: undefined,
    boot_id: undefined,
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
    const copied: string[] = [];
    Object.defineProperty(window, "copiedValues", { value: copied });
    Object.defineProperty(navigator, "clipboard", {
      value: {
        writeText: (value: string) => {
          copied.push(value);
          return Promise.resolve();
        },
      },
    });
  });
  await page.route("**/api/v1/readyz", (route) => route.fulfill({ json: {} }));
  await page.route("**/api/v1/workspaces", (route) =>
    route.fulfill({
      json: {
        items: [{ id: workspaceId, name: "Fleet", slug: "fleet", version: 1 }],
      },
    }),
  );
  await page.route("**/api/v1/nodes?**", (route) =>
    route.fulfill({ json: { items: nodes, page: { has_more: false } } }),
  );
  await page.route(/\/api\/v1\/nodes\/[^/?]+$/, (route) => {
    const id = new URL(route.request().url()).pathname.split("/").pop();
    const node = nodes.find((candidate) => candidate.id === id);
    return node
      ? route.fulfill({ json: node })
      : route.fulfill({ status: 404, json: {} });
  });
  await page.route("**/api/v1/nodes/*/sessions?**", (route) =>
    route.fulfill({ json: { items: [], page: { has_more: false } } }),
  );
  await page.route("**/api/v1/nodes/*/ip-bans", (route) =>
    route.fulfill({ json: { items: [] } }),
  );
  await page.route("**/api/v1/nodes/*/user-group-state", (route) =>
    route.fulfill({ json: { items: [] } }),
  );
});

test("returns to the filtered list and switches nodes without writes", async ({
  page,
}) => {
  const writes: string[] = [];
  const detailReads: string[] = [];
  page.on("request", (request) => {
    if (request.method() !== "GET") writes.push(request.url());
    else if (/\/api\/v1\/nodes\/[^/?]+$/.test(request.url()))
      detailReads.push(request.url());
  });
  await page.goto("/nodes?path=relay");
  await page.getByRole("link", { name: "relay-node" }).click();
  await expect(page.locator("h1")).toHaveText("relay-node");
  await expect(page.getByTestId("node-detail-status")).toHaveText(
    "Latest observation",
  );
  await expect(
    page.locator("dl div", { has: page.getByText("Path", { exact: true }) }),
  ).toContainText("Relay · 41 ms");
  await expect(
    page.getByRole("navigation", { name: "Node detail" }).getByRole("link"),
  ).toHaveCount(5);

  await page.getByRole("link", { name: "Back to nodes" }).click();
  await expect(page).toHaveURL(/\/nodes\?path=relay$/);
  await expect(page.getByText("Showing 1 of 2 nodes")).toBeVisible();

  await page.getByRole("button", { name: "Clear filters" }).last().click();
  await page.getByRole("link", { name: "unknown-node" }).click();
  await expect(page.locator("h1")).toHaveText("unknown-node");
  const summary = page.getByRole("region", { name: "Observed state" });
  await expect(summary).toContainText("Not observed");
  await expect(summary).toContainText("Unknown");
  await expect(
    page.locator("dl div", { has: page.getByText("Boot ID") }),
  ).toContainText("Not available");
  await expect(page.getByRole("button", { name: "Copy Boot ID" })).toHaveCount(
    0,
  );

  expect(detailReads).toHaveLength(2);
  expect(writes).toEqual([]);
});

test("copies long identifiers from a directly opened detail", async ({
  page,
}) => {
  await page.goto(`/nodes/${relayNode.id}`);
  await expect(page.locator("h1")).toHaveText("relay-node");
  await expect(page.getByText(bootId)).toBeVisible();

  await page.getByRole("button", { name: "Copy Boot ID" }).click();
  await expect(page.getByRole("status").getByText("Copied")).toBeAttached();
  await page.getByRole("button", { name: "Copy Node ID" }).click();
  expect(
    await page.evaluate(
      () => (window as unknown as { copiedValues: string[] }).copiedValues,
    ),
  ).toEqual([bootId, relayNode.id]);

  const overflow = await page.evaluate(
    () => document.documentElement.scrollWidth > window.innerWidth,
  );
  expect(overflow).toBe(false);

  await page.getByRole("link", { name: "Back to nodes" }).click();
  await expect(page).toHaveURL(/\/nodes$/);
});

test("recovers an initial detail failure and stops its reads after leaving", async ({
  page,
}) => {
  let fail = true;
  let reads = 0;
  await page.route("**/api/v1/events?**", (route) =>
    route.fulfill({ json: { items: [], page: { has_more: false } } }),
  );
  await page.route(`**/api/v1/nodes/${relayNode.id}`, (route) => {
    reads += 1;
    return route.fulfill(
      fail ? { status: 503, json: {} } : { json: relayNode },
    );
  });
  await page.clock.install();
  await page.goto(`/nodes/${relayNode.id}`);
  await expect(page.getByText("Node state is unavailable")).toBeVisible();
  fail = false;
  await page.clock.runFor(15_000);
  await expect(page.getByTestId("node-detail-status")).toHaveText(
    "Latest observation",
  );
  await page.getByRole("link", { name: "Back to nodes" }).click();
  await expect(page).toHaveURL(/\/nodes$/);
  const readsOnLeave = reads;
  await page.clock.runFor(120_000);
  expect(reads).toBe(readsOnLeave);
});
