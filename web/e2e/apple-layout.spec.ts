import type { Page, TestInfo } from "@playwright/test";
import { join } from "node:path";
import { expect, test } from "./csp";

const workspaceId = "019fc0a4-6d92-765c-a8a1-4af556614cc1";
const nodeId = "019fc0a4-6d92-765c-a8a1-4af556614cc3";
const offlineNodeId = "019fc0a4-6d92-765c-a8a1-4af556614cc4";
const longName = "ProductionWorkspaceOrNodeWithoutWhitespace".repeat(4);
const fixedTime = new Date("2026-10-15T08:00:00Z");
const metricIds = [
  "overview-control-plane",
  "overview-nodes",
  "overview-sessions",
  "overview-operations",
  "overview-connectivity",
  "overview-agent-versions",
];

async function mockOverview(page: Page, state: "normal" | "empty" | "error") {
  let unavailable = state === "error";
  const unhandled: string[] = [];
  await page.clock.setFixedTime(fixedTime);
  // A quiet stream makes REST fixtures deterministic without contacting a
  // Controller. Timer-driven refresh still runs through the real stores.
  await page.addInitScript(() => {
    class QuietEventSource {
      onopen: (() => void) | null = null;
      onerror: (() => void) | null = null;
      addEventListener(): void {}
      close(): void {}
    }
    Object.defineProperty(window, "EventSource", { value: QuietEventSource });
  });
  const nodes = [
    {
      id: nodeId,
      name: `${longName}-online`,
      version: 1,
      trust_status: "active",
      connection_state: "online",
      freshness: "fresh",
      session_count: 7,
      agent_version_state: "current",
      dropped: { security: 0, health: 0, aggregate: 0, raw: 0 },
      path: { mode: "relay", rtt_ms: 42 },
    },
    {
      id: offlineNodeId,
      name: `${longName}-offline`,
      version: 1,
      trust_status: "active",
      connection_state: "offline",
      freshness: "stale",
      session_count: 3,
      dropped: { security: 0, health: 0, aggregate: 0, raw: 0 },
      path: { mode: "direct", rtt_ms: 12 },
    },
  ];
  const operations = ["queued", "failed"].map((operationState, index) => ({
    id: `019fc0a4-6d92-765c-a8a1-4af556614cd${String(index)}`,
    state: operationState,
    node_id: nodeId,
    version: 1,
    created_at: fixedTime.toISOString(),
    updated_at: fixedTime.toISOString(),
  }));
  const events = ["telemetry", "path_changed"].map((type, index) => ({
    id: `019fc0a4-6d92-765c-a8a1-4af556614ce${String(index)}`,
    node_id: nodeId,
    type,
    occurred_at: fixedTime.toISOString(),
    traceparent: "00-trace-span-01",
  }));
  await page.route("**/api/v1/**", (route) => {
    const path = new URL(route.request().url()).pathname;
    if (path === "/api/v1/workspaces")
      return route.fulfill({
        json: {
          items: [
            { id: workspaceId, name: longName, slug: "production", version: 1 },
          ],
        },
      });
    if (
      ![
        "/api/v1/readyz",
        "/api/v1/nodes",
        "/api/v1/operations",
        "/api/v1/operations/summary",
        "/api/v1/events",
      ].includes(path)
    ) {
      unhandled.push(path);
      return route.fulfill({ status: 501, json: {} });
    }
    if (unavailable)
      return route.fulfill({
        status: 503,
        contentType: "application/problem+json",
        body: JSON.stringify({ title: "Fixture source unavailable" }),
      });
    if (path === "/api/v1/readyz") return route.fulfill({ json: {} });
    if (path === "/api/v1/operations/summary")
      return route.fulfill({
        json:
          state === "empty"
            ? { active: 0, unknown: 0 }
            : { active: 4, unknown: 2 },
      });
    const items =
      state === "empty"
        ? []
        : path === "/api/v1/nodes"
          ? nodes
          : path === "/api/v1/operations"
            ? operations
            : events;
    return route.fulfill({ json: { items, page: { has_more: false } } });
  });
  return {
    failRefresh: () => {
      unavailable = true;
    },
    unhandled,
  };
}

async function expectMetrics(page: Page, values: string[]) {
  for (const [index, id] of metricIds.entries())
    await expect(page.getByTestId(id)).toHaveText(values[index] ?? "");
  const sources = [
    "Console readiness check. Not a node online rate.",
    "All nodes in this workspace's latest snapshot.",
    "Sum of each node's last reported session count; not live users.",
    "Active and unknown cover the whole workspace.",
    "Last observed path per node, not current traffic.",
    "Compared with the recommended version; missing versions count as unknown.",
  ];
  for (const source of sources)
    await expect(page.getByText(source, { exact: true })).toBeVisible();
}

async function expectLayout(page: Page) {
  const dimensions = await page.evaluate(() => ({
    document: document.documentElement.scrollWidth,
    viewport: document.documentElement.clientWidth,
  }));
  expect
    .soft(dimensions.document, "document must not scroll horizontally")
    .toBeLessThanOrEqual(dimensions.viewport);
  for (const id of [...metricIds, "workspace", "readiness"]) {
    const bounds = await page.getByTestId(id).boundingBox();
    if (!bounds) throw new Error(`${id} has no layout bounds`);
    expect.soft(bounds.x, `${id} left edge`).toBeGreaterThanOrEqual(0);
    expect
      .soft(bounds.x + bounds.width, `${id} right edge`)
      .toBeLessThanOrEqual(dimensions.viewport + 1);
  }
}

async function evidence(page: Page, info: TestInfo, label: string) {
  const directory = process.env.OCSERVIA_UI_EVIDENCE_DIR;
  if (directory)
    await page.screenshot({
      path: join(directory, info.project.name, `overview-${label}.png`),
      fullPage: true,
      animations: "disabled",
    });
}

test("Overview retains six sourced metrics and links at responsive widths", async ({
  page,
}, info) => {
  const fixture = await mockOverview(page, "normal");
  await page.goto("/");
  await expectMetrics(page, ["Ready", "2", "10", "4", "1", "1"]);
  await expect(
    page.getByTestId("overview-operations-list").locator("li"),
  ).toHaveCount(2);
  await expect(page.getByTestId("overview-events").locator("li")).toHaveCount(
    2,
  );
  const notable = page.getByTestId("overview-notable-nodes");
  await expect(notable).toContainText("Offline");
  await expect(notable).toContainText("Stale");
  await expect(notable.getByRole("link")).toHaveAttribute(
    "href",
    `/nodes/${offlineNodeId}`,
  );
  await expect(
    page.getByTestId("overview-sessions").locator("../.."),
  ).toContainText("Nodes with stale data: 1.");
  await expect(
    page.getByTestId("overview-connectivity").locator("../.."),
  ).toContainText("Includes offline nodes");
  const destinations = [
    "/settings",
    "/nodes",
    "/nodes",
    "/operations",
    "/nodes?columns=path",
    "/nodes",
  ];
  for (const [index, id] of metricIds.entries())
    await expect(
      page.getByTestId(id).locator("xpath=ancestor::a"),
    ).toHaveAttribute("href", destinations[index] ?? "");
  for (const width of [1280, 1440, 1920, 320, 390, 768]) {
    await page.setViewportSize({ width, height: 1000 });
    if (width === 1280 || width === 1440 || width === 390)
      await evidence(page, info, `normal-${String(width)}`);
    await expectLayout(page);
  }
  // CSS zoom exercises enlarged content/reflow; this is not an OS/browser
  // accessibility zoom certification or a physical Safari-device check.
  await page.setViewportSize({ width: 1280, height: 1000 });
  await page.evaluate(() => {
    document.documentElement.style.zoom = "2";
  });
  await expectLayout(page);
  await evidence(page, info, "normal-zoom-200");
  expect(fixture.unhandled).toEqual([]);
});

test("Overview distinguishes empty snapshots from unavailable sources", async ({
  page,
}, info) => {
  const fixture = await mockOverview(page, "empty");
  await page.goto("/");
  await expectMetrics(page, ["Ready", "0", "0", "0", "0", "0"]);
  await expect(
    page.getByText("No operations in this workspace", { exact: true }),
  ).toBeVisible();
  await expect(
    page.getByText("No activity yet", { exact: true }),
  ).toBeVisible();
  await expect(page.getByTestId("overview-notable-nodes")).toHaveCount(0);
  await expectLayout(page);
  await evidence(page, info, "empty");
  expect(fixture.unhandled).toEqual([]);
});

test("Overview exposes initial source errors without inventing zero counts", async ({
  page,
}, info) => {
  const fixture = await mockOverview(page, "error");
  await page.goto("/");
  await expectMetrics(page, ["Unavailable", "–", "–", "–", "–", "–"]);
  await expect(page.getByTestId("overview-readiness")).toHaveText(
    "System status unavailable",
  );
  await expect(
    page.getByText("Operations are unavailable", { exact: true }).last(),
  ).toBeVisible();
  await expect(
    page.getByText("Events are unavailable", { exact: true }),
  ).toBeVisible();
  await expect(page.getByTestId("overview-notable-nodes")).toHaveCount(0);
  await expectLayout(page);
  await evidence(page, info, "error");
  expect(fixture.unhandled).toEqual([]);
});

test("Overview keeps last successful values and labels failed refreshes stale", async ({
  page,
}, info) => {
  await page.clock.install({ time: fixedTime });
  const fixture = await mockOverview(page, "normal");
  await page.goto("/");
  await expectMetrics(page, ["Ready", "2", "10", "4", "1", "1"]);
  fixture.failRefresh();
  await page.clock.fastForward(16_000);
  await expect(
    page.getByText("Refresh failed. Showing the last successful snapshot.", {
      exact: true,
    }),
  ).toBeVisible();
  await expect(
    page.getByText("Refresh failed. Showing the last successful data.", {
      exact: true,
    }),
  ).toHaveCount(3);
  await expectMetrics(page, ["Ready", "2", "10", "4", "1", "1"]);
  await expect(
    page.getByTestId("overview-operations-list").locator("li"),
  ).toHaveCount(2);
  await expect(page.getByTestId("overview-events").locator("li")).toHaveCount(
    2,
  );
  await expectLayout(page);
  await evidence(page, info, "stale");
  expect(fixture.unhandled).toEqual([]);
});
