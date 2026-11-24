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

async function mockAuthRoutes(
  page: Page,
  initialAuthenticated: boolean,
  assertWorkspaceHeader = false,
) {
  await installEventSourceProbe(page);
  await page.route("**/api/v1/readyz", (route) =>
    route.fulfill({ status: 200, contentType: "application/json", body: "{}" }),
  );
  await page.route("**/api/v1/version", (route) =>
    route.fulfill({ status: 200, json: { version: "dev", commit: "fixture" } }),
  );
  const state = { authenticated: initialAuthenticated, logins: 0 };
  await page.route("**/api/v1/workspaces", (route) =>
    state.authenticated
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
    state.authenticated = true;
    state.logins += 1;
    return route.fulfill({ status: 204 });
  });
  await page.route("**/api/v1/nodes?**", (route) => {
    if (assertWorkspaceHeader)
      expect(route.request().headers()["x-workspace-id"]).toBe(alphaId);
    return route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ items: [], page: { has_more: false } }),
    });
  });
  return state;
}

test("local login returns to the requested console route and opens the workspace stream", async ({
  page,
}) => {
  await mockAuthRoutes(page, false, true);

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
  const auth = await mockAuthRoutes(page, true);

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

  auth.authenticated = false;
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
  expect(auth.logins).toBe(1);
});

test("changes the Local password from Settings and requires sign-in again", async ({
  page,
}) => {
  const auth = await mockAuthRoutes(page, true);
  let requests = 0;
  const reply = Promise.withResolvers<undefined>();
  await page.route("**/api/v1/auth/change-password", async (route) => {
    requests += 1;
    expect(route.request().postDataJSON()).toEqual({
      current_password:
        requests === 1 ? "fixture-incorrect" : "fixture-old-password",
      new_password: "fixture-new-password",
    });
    if (requests === 1) {
      await route.fulfill({
        status: 401,
        contentType: "application/problem+json",
        body: JSON.stringify({
          type: "https://ocservia.dev/problems/password-change",
        }),
      });
      return;
    }
    await reply.promise;
    auth.authenticated = false;
    await route.fulfill({ status: 204 });
  });
  await page.goto("/settings");
  const submit = page.getByRole("button", {
    name: "Change password",
    exact: true,
  });
  const current = page.getByLabel("Current password", { exact: true });
  const next = page.getByLabel("New password", { exact: true });
  const confirm = page.getByLabel("Confirm new password", { exact: true });
  await current.fill("fixture-incorrect");
  await next.fill("fixture-new-password");
  await confirm.fill("fixture-mismatch");
  await submit.click();
  await expect(page.getByRole("alert")).toHaveText(
    "The new passwords do not match.",
  );
  expect(requests).toBe(0);

  await confirm.fill("fixture-new-password");
  await submit.click();
  await expect(page.getByRole("alert")).toContainText(
    "Check your current password",
  );
  await expect(page).toHaveURL(/\/settings$/);
  for (const input of [current, next, confirm])
    await expect(input).toHaveValue("");

  await current.fill("fixture-old-password");
  await next.fill("fixture-new-password");
  await confirm.fill("fixture-new-password");
  await submit.click();
  await expect(
    page.getByRole("button", { name: "Changing password…" }),
  ).toBeDisabled();
  await expect.poll(() => requests).toBe(2);
  await page
    .getByRole("form", { name: "Change password" })
    .dispatchEvent("submit");
  expect(requests).toBe(2);
  reply.resolve(undefined);
  await expect(page).toHaveURL(/\/login$/);
  await expect(page.getByRole("heading", { name: "Sign in" })).toBeVisible();
});

test("redirects Settings to login when the password change session is expired", async ({
  page,
}) => {
  await mockAuthRoutes(page, true);
  await page.route("**/api/v1/auth/change-password", (route) =>
    route.fulfill({
      status: 401,
      contentType: "application/problem+json",
      body: JSON.stringify({
        type: "https://ocservia.dev/problems/unauthenticated",
      }),
    }),
  );
  await page.goto("/settings");
  await page
    .getByLabel("Current password", { exact: true })
    .fill("fixture-old-password");
  await page
    .getByLabel("New password", { exact: true })
    .fill("fixture-new-password");
  await page
    .getByLabel("Confirm new password", { exact: true })
    .fill("fixture-new-password");
  await page
    .getByRole("button", { name: "Change password", exact: true })
    .click();
  await expect(page).toHaveURL(/\/login$/);
});
