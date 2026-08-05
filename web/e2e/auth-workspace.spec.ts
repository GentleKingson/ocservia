import { expect, test, type Page } from "@playwright/test";

const alphaId = "019fc0a4-6d92-765c-a8a1-4af556614cc1";
const betaId = "019fc0a4-6d92-765c-a8a1-4af556614cc2";

async function installEventSourceProbe(page: Page): Promise<void> {
  await page.addInitScript(() => {
    const sources: Array<{ url: string; closed: boolean }> = [];
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

test("fresh OIDC login returns to the requested console route", async ({
  page,
}) => {
  await installEventSourceProbe(page);
  await mockReadiness(page);
  let workspaceRequests = 0;
  await page.route("**/api/v1/workspaces", (route) => {
    workspaceRequests += 1;
    if (workspaceRequests === 1) {
      return route.fulfill({
        status: 401,
        contentType: "application/problem+json",
        body: "{}",
      });
    }
    return route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        items: [{ id: alphaId, name: "Alpha", slug: "alpha", version: 1 }],
      }),
    });
  });
  await page.route("**/api/v1/auth/login", (route) =>
    route.fulfill({ status: 302, headers: { location: "/fake-idp" } }),
  );
  await page.route("**/fake-idp", (route) =>
    route.fulfill({
      status: 302,
      headers: { location: "/api/v1/auth/callback?state=test&code=test" },
    }),
  );
  await page.route("**/api/v1/auth/callback?**", (route) =>
    route.fulfill({ status: 302, headers: { location: "/" } }),
  );
  await page.route("**/api/v1/nodes?**", (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ items: [], page: { has_more: false } }),
    }),
  );

  await page.goto("/nodes");

  await expect(page).toHaveURL(/\/nodes$/);
  await expect(page.getByRole("heading", { name: "Nodes" })).toBeVisible();
  expect(workspaceRequests).toBeGreaterThanOrEqual(2);
});

test("switching workspace clears fleet state and reconnects its stream", async ({
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
    const name = workspaceId === betaId ? "Beta node" : "Alpha node";
    return route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        items: [
          {
            id: workspaceId === betaId ? betaId : alphaId,
            name,
            version: 1,
            trust_status: "active",
            connection_state: "online",
            freshness: "fresh",
            dropped: { security: 0, health: 0, aggregate: 0, raw: 0 },
            session_count: 0,
          },
        ],
        page: { has_more: false },
      }),
    });
  });

  await page.goto("/nodes");
  await expect(page.getByText("Alpha node")).toBeVisible();
  await page.getByLabel("Workspace").selectOption(betaId);

  await expect(page.getByText("Beta node")).toBeVisible();
  await expect(page.getByText("Alpha node")).toHaveCount(0);
  await expect
    .poll(() =>
      page.evaluate(() => {
        const sources = (
          window as unknown as {
            __eventSources: Array<{ url: string; closed: boolean }>;
          }
        ).__eventSources;
        return {
          count: sources.length,
          firstClosed: sources[0]?.closed,
          latestURL: sources.at(-1)?.url,
        };
      }),
    )
    .toMatchObject({
      count: 2,
      firstClosed: true,
      latestURL: expect.stringContaining(`workspace_id=${betaId}`),
    });
});
