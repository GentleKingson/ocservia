import { expect, test, type Page } from "@playwright/test";

const alphaId = "019fc0a4-6d92-765c-a8a1-4af556614cc1";
const betaId = "019fc0a4-6d92-765c-a8a1-4af556614cc2";
const alphaNodeA = "019fc0a4-6d92-765c-a8a1-4af556614cc3";
const alphaNodeB = "019fc0a4-6d92-765c-a8a1-4af556614cc4";
const betaNodeA = "019fc0a4-6d92-765c-a8a1-4af556614cc5";

interface SimulationResponse {
  id: string;
  node_id?: string;
}

async function installEventSourceProbe(page: Page): Promise<void> {
  await page.addInitScript(() => {
    const sources: Array<{
      url: string;
      closed: boolean;
    }> = [];
    class EventSourceProbe {
      static readonly CONNECTING = 0;
      static readonly OPEN = 1;
      static readonly CLOSED = 2;
      readonly CONNECTING = 0;
      readonly OPEN = 1;
      readonly CLOSED = 2;
      readonly withCredentials = false;
      readonly readyState = 1;
      onerror: ((event: Event) => void) | null = null;
      onmessage: ((event: MessageEvent) => void) | null = null;
      onopen: ((event: Event) => void) | null = null;
      readonly url: string;
      private readonly record: { url: string; closed: boolean };

      constructor(url: string | URL) {
        this.url = String(url);
        this.record = { url: this.url, closed: false };
        sources.push(this.record);
      }

      addEventListener(): void {}
      removeEventListener(): void {}
      dispatchEvent(): boolean {
        return true;
      }
      close(): void {
        this.record.closed = true;
      }
    }
    Object.defineProperty(window, "EventSource", { value: EventSourceProbe });
    Object.defineProperty(window, "__eventSources", { value: sources });
  });
}

async function mockReadiness(page: Page): Promise<void> {
  await page.route("**/api/v1/readyz", (route) =>
    route.fulfill({ status: 200, contentType: "application/json", body: "{}" }),
  );
}

test("renders real fleet counts without simulator controls", async ({
  page,
}) => {
  const seeded = await page.request.post("/api/v1/development/simulations", {
    data: { heartbeat_count: 3, delay_millis: 100 },
  });
  expect(seeded.status()).toBe(202);

  await page.goto("/");
  await expect(page.getByRole("heading", { name: "Overview" })).toBeVisible();
  await expect(page.getByTestId("run-probe")).toHaveCount(0);
  await expect(page.getByTestId("active-nodes")).toHaveCount(0);
  await expect(page.getByTestId("pending-operations")).toHaveCount(0);

  await expect
    .poll(async () =>
      Number(await page.getByTestId("overview-nodes").innerText()),
    )
    .toBeGreaterThan(0);
  await expect(page.getByTestId("overview-sessions")).toBeVisible();
  await expect(page.getByTestId("overview-connectivity")).toBeVisible();
  await expect
    .poll(async () =>
      page.getByTestId("overview-operations-list").locator("li").count(),
    )
    .toBeGreaterThan(0);
  await expect
    .poll(async () => page.getByTestId("overview-events").locator("li").count())
    .toBeGreaterThan(0);
});

test("shows a live platform event without a page reload", async ({ page }) => {
  const initial = await page.request.post("/api/v1/development/simulations", {
    data: { heartbeat_count: 3, delay_millis: 100 },
  });
  expect(initial.status()).toBe(202);

  await page.goto("/");
  await expect(page.getByRole("heading", { name: "Overview" })).toBeVisible();
  await expect
    .poll(async () => page.getByTestId("overview-events").locator("li").count())
    .toBeGreaterThan(0);

  const triggered = await page.request.post("/api/v1/development/simulations", {
    data: { heartbeat_count: 3, delay_millis: 100 },
  });
  expect(triggered.status()).toBe(202);
  const simulation = (await triggered.json()) as SimulationResponse;
  const simulatedNodeId = simulation.node_id;
  if (!simulatedNodeId) throw new Error("simulation did not report a node");

  await expect
    .poll(
      async () =>
        page
          .getByTestId("overview-events")
          .getByText(`sim-${simulatedNodeId}`)
          .count(),
      { timeout: 20_000 },
    )
    .toBeGreaterThan(0);
  await expect
    .poll(async () =>
      Number(await page.getByTestId("overview-nodes").innerText()),
    )
    .toBeGreaterThan(0);
});

test("switching workspaces replaces every overview number and list", async ({
  page,
}) => {
  await installEventSourceProbe(page);
  await mockReadiness(page);
  await page.route("**/api/v1/workspaces", (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        items: [
          { id: alphaId, name: "Alpha", slug: "alpha", version: 1 },
          { id: betaId, name: "Beta", slug: "beta", version: 1 },
        ],
      }),
    }),
  );
  await page.route("**/api/v1/nodes?**", (route) => {
    const workspaceId = route.request().headers()["x-workspace-id"];
    const isAlpha = workspaceId === alphaId;
    const items = isAlpha
      ? [
          {
            id: alphaNodeA,
            name: "Alpha node A",
            version: 1,
            trust_status: "active",
            connection_state: "online",
            freshness: "fresh",
            dropped: { security: 0, health: 0, aggregate: 0, raw: 0 },
            session_count: 2,
            path: { mode: "direct", rtt_ms: 14 },
          },
          {
            id: alphaNodeB,
            name: "Alpha node B",
            version: 1,
            trust_status: "active",
            connection_state: "offline",
            freshness: "stale",
            dropped: { security: 0, health: 0, aggregate: 0, raw: 0 },
            session_count: 0,
          },
        ]
      : [
          {
            id: betaNodeA,
            name: "Beta node A",
            version: 1,
            trust_status: "active",
            connection_state: "online",
            freshness: "fresh",
            dropped: { security: 0, health: 0, aggregate: 0, raw: 0 },
            session_count: 5,
            path: { mode: "relay", rtt_ms: 61 },
          },
        ];
    return route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ items, page: { has_more: false } }),
    });
  });
  await page.route("**/api/v1/operations?**", (route) => {
    const workspaceId = route.request().headers()["x-workspace-id"];
    const isAlpha = workspaceId === alphaId;
    const items = isAlpha
      ? [
          {
            id: "019fc0a4-6d92-765c-a8a1-4af556614cd1",
            state: "queued",
            node_id: alphaNodeA,
            version: 1,
            created_at: "2026-08-26T08:00:00Z",
            updated_at: "2026-08-26T08:00:00Z",
          },
          {
            id: "019fc0a4-6d92-765c-a8a1-4af556614cd2",
            state: "failed",
            node_id: alphaNodeA,
            version: 1,
            created_at: "2026-08-26T07:00:00Z",
            updated_at: "2026-08-26T07:00:00Z",
          },
        ]
      : [
          {
            id: "019fc0a4-6d92-765c-a8a1-4af556614cd3",
            state: "unknown",
            node_id: betaNodeA,
            version: 1,
            created_at: "2026-08-26T09:00:00Z",
            updated_at: "2026-08-26T09:00:00Z",
          },
        ];
    return route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ items, page: { has_more: false } }),
    });
  });
  await page.route("**/api/v1/operations/summary", (route) => {
    const workspaceId = route.request().headers()["x-workspace-id"];
    const summary =
      workspaceId === alphaId
        ? { active: 1, unknown: 0 }
        : { active: 0, unknown: 1 };
    return route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(summary),
    });
  });
  await page.route("**/api/v1/events?**", (route) => {
    const workspaceId = route.request().headers()["x-workspace-id"];
    const isAlpha = workspaceId === alphaId;
    const items = isAlpha
      ? [
          {
            id: "019fc0a4-6d92-765c-a8a1-4af556614ce1",
            node_id: alphaNodeA,
            type: "connected",
            traceparent: "00-trace-span-01",
            occurred_at: "2026-08-26T08:00:00Z",
          },
        ]
      : [
          {
            id: "019fc0a4-6d92-765c-a8a1-4af556614ce2",
            node_id: betaNodeA,
            type: "heartbeat",
            traceparent: "00-trace-span-01",
            occurred_at: "2026-08-26T09:00:00Z",
          },
        ];
    return route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ items, page: { has_more: false } }),
    });
  });

  await page.goto("/");
  await expect(page.getByTestId("overview-nodes")).toHaveText("2");
  await expect(page.getByTestId("overview-sessions")).toHaveText("2");
  await expect(page.getByTestId("overview-operations")).toHaveText("1");
  await expect(page.getByTestId("overview-connectivity")).toHaveText("1");
  await expect(
    page.getByTestId("overview-events").getByText("Alpha node A"),
  ).toBeVisible();
  await expect(
    page.getByTestId("overview-notable-nodes").getByText("Alpha node B"),
  ).toBeVisible();

  await page.getByLabel("Workspace").selectOption(betaId);

  await expect(page.getByTestId("overview-nodes")).toHaveText("1");
  await expect(page.getByTestId("overview-sessions")).toHaveText("5");
  await expect(page.getByTestId("overview-operations")).toHaveText("0");
  await expect(page.getByTestId("overview-connectivity")).toHaveText("0");
  await expect(page.getByTestId("overview-events")).toContainText(
    "Beta node A",
  );
  await expect(page.getByText("Alpha node A")).toHaveCount(0);
  await expect(page.getByText("Alpha node B")).toHaveCount(0);
  await expect
    .poll(() =>
      page.evaluate(() => {
        const sources = (
          window as unknown as {
            __eventSources: Array<{ url: string; closed: boolean }>;
          }
        ).__eventSources;
        return sources.filter((source) => !source.closed).length;
      }),
    )
    .toBe(1);
});
