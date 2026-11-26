import type { Page } from "@playwright/test";
import { expect, test } from "./csp";
import { installEventSourceStub } from "./fixtures";

const alphaId = "019fc0a4-6d92-765c-a8a1-4af556614cc1";

async function mockAuthRoutes(
  page: Page,
  initialAuthenticated: boolean,
  assertWorkspaceHeader = false,
) {
  await installEventSourceStub(page, { record: true });
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
  try {
    await submit.click();
    await expect(
      page.getByRole("button", { name: "Changing password…" }),
    ).toBeDisabled();
    await expect.poll(() => requests).toBe(2);
    // Exercise the submit handler while the button is disabled.
    const submitted = await page
      .locator("form")
      .evaluate((form: HTMLFormElement) => {
        let dispatched = false;
        form.addEventListener(
          "submit",
          () => {
            dispatched = true;
          },
          { once: true },
        );
        form.requestSubmit();
        return dispatched;
      });
    expect(submitted).toBe(true);
  } finally {
    reply.resolve(undefined);
  }
  await expect(page).toHaveURL(/\/login$/);
  await expect(page.getByRole("heading", { name: "Sign in" })).toBeVisible();
  expect(requests).toBe(2);
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
