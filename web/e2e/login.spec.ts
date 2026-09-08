import { expect, test, type Page } from "@playwright/test";

async function methods(
  page: Page,
  local: boolean,
  oidc: boolean,
): Promise<void> {
  await page.route("**/api/v1/auth/methods", (route) =>
    route.fulfill({ json: { local, oidc } }),
  );
}

test("local-only login restores return path and initializes the shell without storing passwords", async ({
  page,
}) => {
  await methods(page, true, false);
  let authenticated = false;
  await page.route("**/api/v1/workspaces", (route) =>
    authenticated
      ? route.fulfill({
          json: {
            items: [
              { id: "workspace-a", name: "Alpha", slug: "alpha", version: 1 },
            ],
          },
        })
      : route.fulfill({ status: 401, json: {} }),
  );
  await page.route("**/api/v1/readyz", (route) => route.fulfill({ json: {} }));
  await page.route("**/api/v1/nodes?**", (route) =>
    route.fulfill({ json: { items: [], page: { has_more: false } } }),
  );
  await page.route("**/api/v1/auth/login", async (route) => {
    expect(route.request().method()).toBe("POST");
    expect(route.request().postDataJSON()).toEqual({
      username: "alice",
      password: "test-only-password",
    });
    authenticated = true;
    await route.fulfill({ status: 204 });
  });
  await page.goto("/nodes?filter=login#latest");
  await expect(page).toHaveURL(/\/login$/);
  await expect(
    page.getByRole("button", { name: "Sign in with SSO" }),
  ).toHaveCount(0);
  await expect(page.locator(".sidebar")).toHaveCount(0);
  await page.getByLabel("Username", { exact: true }).fill("alice");
  await page.getByLabel("Password", { exact: true }).fill("test-only-password");
  expect(
    await page.evaluate(() =>
      JSON.stringify([
        Object.entries(localStorage),
        Object.entries(sessionStorage),
      ]),
    ),
  ).not.toContain("test-only-password");
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(page).toHaveURL(/\/nodes\?filter=login#latest$/);
  await expect(page.getByRole("heading", { name: "Nodes" })).toBeVisible();
  await expect(page.getByLabel("Workspace")).toContainText("Alpha");
  await expect(page.locator(".status")).toHaveText("Ready");
  expect(
    await page.evaluate(() =>
      JSON.stringify([
        Object.entries(localStorage),
        Object.entries(sessionStorage),
      ]),
    ),
  ).not.toContain("test-only-password");
  await page.getByRole("link", { name: "Settings", exact: true }).click();
  await expect(page).toHaveURL(/\/settings$/);
});

test("combined login shows generic credential and rate-limit errors, and SSO uses GET", async ({
  page,
}) => {
  await methods(page, true, true);
  let status = 401;
  await page.route("**/api/v1/auth/login", (route) =>
    route.request().method() === "GET"
      ? route.fulfill({ contentType: "text/html", body: "SSO provider" })
      : route.fulfill({
          status,
          headers: { "Retry-After": "42" },
          json: { detail: "User not found" },
        }),
  );
  await page.goto("/login");
  await expect(
    page.getByRole("button", { name: "Sign in with SSO" }),
  ).toBeVisible();
  await page.getByLabel("Username", { exact: true }).fill("alice");
  await page.getByLabel("Password", { exact: true }).fill("test-only-password");
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(page.getByRole("alert")).toHaveText(
    "Invalid username or password",
  );
  await expect(page.getByLabel("Password", { exact: true })).toHaveValue("");
  await expect(page).toHaveURL(/\/login$/);
  status = 429;
  await page.getByLabel("Password", { exact: true }).fill("test-only-password");
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(page.getByRole("alert")).toHaveText(
    "Too many attempts. Please try again in 42 seconds.",
  );
  await page.getByRole("button", { name: "Sign in with SSO" }).click();
  await expect(page).toHaveURL(/\/api\/v1\/auth\/login$/);
});

test("OIDC-only redirects without a click", async ({ page }) => {
  await methods(page, false, true);
  await page.route("**/api/v1/auth/login", (route) =>
    route.fulfill({ contentType: "text/html", body: "SSO provider" }),
  );
  await page.goto("/login");
  await expect(page).toHaveURL(/\/api\/v1\/auth\/login$/);
});

test("unavailable methods fail closed and can be retried", async ({ page }) => {
  await methods(page, false, false);
  await page.goto("/login");
  await expect(page.getByRole("alert")).toBeVisible();
  await expect(page.getByLabel("Password", { exact: true })).toHaveCount(0);
  await methods(page, true, false);
  await page.getByRole("button", { name: "Try again" }).click();
  await expect(page.getByLabel("Password", { exact: true })).toBeVisible();
});

test("OIDC return followed by 401 stops automatic login and permits manual retry", async ({
  page,
}) => {
  await methods(page, false, true);
  let starts = 0;
  await page.route("**/api/v1/workspaces", (route) =>
    route.fulfill({ status: 401, json: {} }),
  );
  await page.route("**/api/v1/auth/login", (route) => {
    starts += 1;
    return starts < 3
      ? route.fulfill({ status: 302, headers: { Location: "/" } })
      : route.fulfill({ contentType: "text/html", body: "Loop safety stop" });
  });
  await page.goto("/nodes?filter=login#latest");
  await expect(
    page.getByRole("button", { name: "Sign in with SSO" }),
  ).toBeVisible();
  expect(starts).toBe(1);
  await page.reload();
  await expect(
    page.getByRole("button", { name: "Sign in with SSO" }),
  ).toBeVisible();
  expect(starts).toBe(1);
  expect(
    await page.evaluate(() =>
      sessionStorage.getItem("ocservia.login.return-to"),
    ),
  ).toBe("/nodes?filter=login#latest");
  await page.getByRole("button", { name: "Sign in with SSO" }).click();
  await expect.poll(() => starts).toBe(2);
  await expect(
    page.getByRole("button", { name: "Sign in with SSO" }),
  ).toBeVisible();
});

test("fixed failure marker prevents auto SSO without accepting external returnTo", async ({
  page,
}) => {
  await methods(page, false, true);
  let starts = 0;
  await page.route("**/api/v1/auth/login", (route) => {
    starts += 1;
    return route.fulfill({ contentType: "text/html", body: "Manual SSO" });
  });
  await page.goto(
    "/login?auth=failed&returnTo=https://evil.example&error=provider-secret",
  );
  await expect(page.getByRole("alert")).toHaveText(
    "SSO did not complete. Sign in with SSO to try again.",
  );
  expect(starts).toBe(0);
  await page.getByRole("button", { name: "Sign in with SSO" }).click();
  await expect(page).toHaveURL(/\/api\/v1\/auth\/login$/);
  expect(starts).toBe(1);
});

test("callback rejection is terminal and revisiting login offers manual retry", async ({
  page,
}) => {
  await methods(page, false, true);
  let starts = 0;
  await page.route("**/api/v1/auth/login", (route) => {
    starts += 1;
    return route.fulfill({
      status: 302,
      headers: { Location: "/api/v1/auth/callback?state=test&code=test" },
    });
  });
  await page.route("**/api/v1/auth/callback?**", (route) =>
    route.fulfill({
      status: 401,
      json: {
        type: "https://ocservia.dev/problems/oidc-callback-rejected",
        title: "Login rejected",
      },
    }),
  );
  await page.goto("/login");
  await expect(page).toHaveURL(/\/api\/v1\/auth\/callback\?/);
  expect(starts).toBe(1);
  await page.goto("/login");
  await expect(
    page.getByRole("button", { name: "Sign in with SSO" }),
  ).toBeVisible();
  expect(starts).toBe(1);
});
