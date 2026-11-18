import { expect, test } from "./csp";

test("local-only login restores return path and initializes the shell without storing passwords", async ({
  page,
}) => {
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
  await expect(page.getByRole("navigation")).toHaveCount(0);
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
  await expect(page.getByTestId("workspace")).toContainText("Alpha");
  await expect(page.locator("#workspace-select")).toHaveCount(0);
  await expect(page.getByTestId("readiness")).toHaveText("Ready");
  expect(
    await page.evaluate(() =>
      JSON.stringify([
        Object.entries(localStorage),
        Object.entries(sessionStorage),
      ]),
    ),
  ).not.toContain("test-only-password");
  const openNavigation = page.getByRole("button", { name: "Open navigation" });
  if (await openNavigation.isVisible()) await openNavigation.click();
  await page.getByRole("link", { name: "Settings", exact: true }).click();
  await expect(page).toHaveURL(/\/settings$/);
});

test("local login shows generic credential and rate-limit errors", async ({
  page,
}) => {
  let status = 401;
  await page.route("**/api/v1/auth/login", (route) =>
    route.fulfill({
      status,
      headers: { "Retry-After": "42" },
      json: { detail: "User not found" },
    }),
  );
  await page.goto("/login");
  await expect(
    page.getByRole("button", { name: "Sign in with SSO" }),
  ).toHaveCount(0);
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
});

test("signed-out visits never render the console shell before login", async ({
  page,
}) => {
  await page.addInitScript(() => {
    new MutationObserver(() => {
      if (document.querySelector("#main-content, aside"))
        sessionStorage.setItem("test.shell-rendered", "true");
    }).observe(document, {
      childList: true,
      subtree: true,
    });
  });
  await page.route("**/api/v1/workspaces", (route) =>
    route.fulfill({ status: 401, json: {} }),
  );
  await page.route("**/api/v1/readyz", (route) => route.fulfill({ json: {} }));
  await page.goto("/");
  await expect(page).toHaveURL(/\/login$/);
  await expect(page.getByLabel("Username", { exact: true })).toBeVisible();
  expect(
    await page.evaluate(() => sessionStorage.getItem("test.shell-rendered")),
  ).toBeNull();
});
