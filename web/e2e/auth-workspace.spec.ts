import type { Page } from "@playwright/test";
import { expect, test } from "./csp";

const alphaId = "019fc0a4-6d92-765c-a8a1-4af556614cc1";

async function installEventSourceProbe(page: Page): Promise<void> {
  await page.addInitScript(() => {
    const sources: Array<{
      url: string;
      closed: boolean;
      emitError: () => void;
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
      private readonly record: {
        url: string;
        closed: boolean;
        emitError: () => void;
      };

      constructor(url: string | URL) {
        this.url = String(url);
        this.record = {
          url: this.url,
          closed: false,
          emitError: () => this.onerror?.(new Event("error")),
        };
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

test("local login returns to the requested console route and opens the workspace stream", async ({
  page,
}) => {
  await installEventSourceProbe(page);
  await mockReadiness(page);
  let authenticated = false;
  await page.route("**/api/v1/workspaces", (route) =>
    authenticated
      ? route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({
            items: [{ id: alphaId, name: "Alpha", slug: "alpha", version: 1 }],
          }),
        })
      : route.fulfill({
          status: 401,
          contentType: "application/problem+json",
          body: "{}",
        }),
  );
  await page.route("**/api/v1/auth/login", (route) => {
    authenticated = true;
    return route.fulfill({ status: 204 });
  });
  await page.route("**/api/v1/nodes?**", (route) => {
    expect(route.request().headers()["x-workspace-id"]).toBe(alphaId);
    return route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ items: [], page: { has_more: false } }),
    });
  });

  await page.goto("/nodes");
  await expect(page).toHaveURL(/\/login$/);
  await page.getByLabel("Username", { exact: true }).fill("admin");
  await page.getByLabel("Password", { exact: true }).fill("test-only-password");
  await page.getByRole("button", { name: "Sign in", exact: true }).click();

  await expect(page).toHaveURL(/\/nodes$/);
  await expect(page.getByRole("heading", { name: "Nodes" })).toBeVisible();
  await expect(page.getByTestId("workspace")).toContainText("Alpha");
  await expect(page.locator("#workspace-select")).toHaveCount(0);
  await expect
    .poll(() =>
      page.evaluate(() => {
        const sources = (
          window as unknown as {
            __eventSources: Array<{ url: string; closed: boolean }>;
          }
        ).__eventSources;
        return sources
          .filter((source) => !source.closed)
          .map((source) => source.url);
      }),
    )
    .toEqual([expect.stringContaining(`workspace_id=${alphaId}`)]);
  expect(
    await page.evaluate(() =>
      sessionStorage.getItem("ocservia.login.return-to"),
    ),
  ).toBeNull();
});

test("an expired SSE session redirects to local login and restores the console", async ({
  page,
}) => {
  await installEventSourceProbe(page);
  await mockReadiness(page);
  let authenticated = true;
  let logins = 0;
  await page.route("**/api/v1/workspaces", (route) =>
    authenticated
      ? route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({
            items: [{ id: alphaId, name: "Alpha", slug: "alpha", version: 1 }],
          }),
        })
      : route.fulfill({
          status: 401,
          contentType: "application/problem+json",
          body: "{}",
        }),
  );
  await page.route("**/api/v1/auth/login", (route) => {
    authenticated = true;
    logins += 1;
    return route.fulfill({ status: 204 });
  });
  await page.route("**/api/v1/nodes?**", (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ items: [], page: { has_more: false } }),
    }),
  );

  await page.goto("/nodes");
  await expect(page.getByRole("heading", { name: "Nodes" })).toBeVisible();
  await expect
    .poll(() =>
      page.evaluate(
        () =>
          (
            window as unknown as {
              __eventSources: Array<{ closed: boolean }>;
            }
          ).__eventSources.filter((source) => !source.closed).length,
      ),
    )
    .toBe(1);

  authenticated = false;
  await page.evaluate(() => {
    const sources = (
      window as unknown as {
        __eventSources: Array<{ emitError: () => void }>;
      }
    ).__eventSources;
    sources.at(-1)?.emitError();
  });

  await expect(page).toHaveURL(/\/login$/);
  await page.getByLabel("Username", { exact: true }).fill("admin");
  await page.getByLabel("Password", { exact: true }).fill("test-only-password");
  await page.getByRole("button", { name: "Sign in", exact: true }).click();

  await expect(page).toHaveURL(/\/nodes$/);
  await expect(page.getByRole("heading", { name: "Nodes" })).toBeVisible();
  expect(logins).toBe(1);
});
