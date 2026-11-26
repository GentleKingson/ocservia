import type { Page } from "@playwright/test";
import { expect, test } from "./csp";
import { installConsoleMocks, wireNode } from "./fixtures";
const alpha = "019fc0a4-6d92-765c-a8a1-4af556614fa1";
const secret = "oit1_aHR0cHM6Ly9jb250cm9sbGVyLmV4YW1wbGU." + "b".repeat(43);

async function setup(page: Page, empty: boolean) {
  await installConsoleMocks(page, {
    workspace: { id: alpha, name: "Alpha", slug: "alpha" },
    nodes: empty
      ? []
      : [wireNode("019fc0a4-6d92-765c-a8a1-4af556614fb1", "existing-node")],
  });
}
async function prepare(page: Page) {
  await page.getByRole("button", { name: "Install node", exact: true }).click();
  await page.getByLabel("Node name", { exact: true }).fill("edge-1");
  await page.getByLabel("Install Release tag").fill("v1.2.3");
  await page.getByRole("button", { name: "Continue", exact: true }).click();
}
test("installs from populated inventory with one scoped token request", async ({
  page,
}) => {
  await setup(page, false);
  const logs: string[] = [];
  page.on("console", (message) => logs.push(message.text()));
  let posts = 0;
  await page.route("**/api/v1/node-bootstrap-tokens", async (route) => {
    posts += 1;
    expect(route.request().method()).toBe("POST");
    expect(route.request().headers()["x-workspace-id"]).toBe(alpha);
    expect(route.request().headers()["content-type"]).toBe("application/json");
    expect(route.request().postDataJSON()).toEqual({
      workspace_id: alpha,
      installation: true,
      environment: "production",
      expected_node_name: "edge-1",
      ttl_seconds: 900,
      reason: "Bootstrap managed node",
    });
    await route.fulfill({
      status: 201,
      json: {
        id: alpha,
        token: secret,
        expires_at: new Date(Date.now() + 900_000).toISOString(),
      },
    });
  });
  await page.goto("/nodes");
  await prepare(page);
  expect(posts).toBe(0);
  await expect(page.getByLabel("Run on the Agent host")).toHaveValue(
    /--version v1\.2\.3/,
  );
  await expect(page.getByText(/hidden prompt/).first()).toBeVisible();
  await page
    .getByRole("button", { name: "Create bootstrap token", exact: true })
    .click();
  await expect(page.getByLabel("Bootstrap token (shown once)")).toHaveValue(
    secret,
  );
  await expect(
    page.getByRole("button", { name: "Create bootstrap token", exact: true }),
  ).toBeDisabled();
  await expect(page.getByText(/^Expires at /)).toBeVisible();
  expect(
    await page.getByLabel("Run on the Agent host").inputValue(),
  ).not.toContain(secret);
  expect(
    await page.evaluate(() => JSON.stringify([localStorage, sessionStorage])),
  ).not.toContain(secret);
  expect(page.url()).not.toContain(secret);
  expect(logs.join("\n")).not.toContain(secret);
  await expect(
    page.getByLabel("Controller EndpointID", { exact: true }),
  ).toHaveCount(0);
  expect(posts).toBe(1);
  const dialog = page.getByRole("dialog");
  const box = await dialog.boundingBox();
  const viewport = page.viewportSize();
  if (!box || !viewport) throw new Error("Missing dialog or viewport");
  expect(box.width).toBeLessThanOrEqual(viewport.width);
});

test("clears a displayed token when the dialog closes and when the page is left", async ({
  page,
}) => {
  await setup(page, true);
  await page.route("**/api/v1/node-bootstrap-tokens", (route) =>
    route.fulfill({
      status: 201,
      json: {
        id: alpha,
        token: secret,
        expires_at: new Date(Date.now() + 900_000).toISOString(),
      },
    }),
  );
  await page.goto("/nodes");
  await prepare(page);
  await page
    .getByRole("button", { name: "Create bootstrap token", exact: true })
    .click();
  await expect(page.getByLabel("Bootstrap token (shown once)")).toHaveValue(
    secret,
  );
  await page.getByRole("button", { name: "Cancel", exact: true }).click();
  await expect(page.getByRole("dialog")).toHaveCount(0);
  await prepare(page);
  await expect(page.getByLabel("Bootstrap token (shown once)")).toHaveCount(0);
  await page
    .getByRole("button", { name: "Create bootstrap token", exact: true })
    .click();
  await expect(page.getByLabel("Bootstrap token (shown once)")).toHaveValue(
    secret,
  );
  await page.goto("/nodes");
  await prepare(page);
  await expect(page.getByLabel("Bootstrap token (shown once)")).toHaveCount(0);
});

test("discards a bootstrap token if the dialog closes during issuance", async ({
  page,
}) => {
  await setup(page, true);
  const received = Promise.withResolvers<undefined>();
  const release = Promise.withResolvers<undefined>();
  const finished = Promise.withResolvers<undefined>();
  let posts = 0;
  await page.route("**/api/v1/node-bootstrap-tokens", async (route) => {
    posts += 1;
    received.resolve(undefined);
    await release.promise;
    try {
      await route.fulfill({
        status: 201,
        json: {
          id: alpha,
          token: secret,
          expires_at: new Date().toISOString(),
        },
      });
    } finally {
      finished.resolve(undefined);
    }
  });
  await page.goto("/nodes");
  await prepare(page);
  await page
    .getByRole("button", { name: "Create bootstrap token", exact: true })
    .click();
  await received.promise;
  await page.getByRole("button", { name: "Cancel", exact: true }).click();
  release.resolve(undefined);
  await finished.promise;
  await prepare(page);
  await expect(page.getByLabel("Bootstrap token (shown once)")).toHaveCount(0);
  expect(posts).toBe(1);
});

for (const [status, message] of [
  [400, /^400: The Controller rejected/],
  [403, /^403: You are not authorized/],
  [503, /The request outcome is unknown/],
  ["network", /The request outcome is unknown/],
  ["malformed", /The request outcome is unknown/],
] as const) {
  test(`reports bootstrap issuance failure ${String(status)} without exposing the response or retrying`, async ({
    page,
  }) => {
    await setup(page, true);
    let posts = 0;
    await page.route("**/api/v1/node-bootstrap-tokens", (route) => {
      posts += 1;
      if (status === "network") return route.abort("failed");
      return route.fulfill({
        status: status === "malformed" ? 201 : status,
        json: { token: secret, expires_at: "invalid" },
      });
    });
    await page.goto("/nodes");
    await prepare(page);
    const issue = page.getByRole("button", {
      name: "Create bootstrap token",
      exact: true,
    });
    await issue.click();
    await expect(page.getByRole("dialog").getByRole("alert")).toHaveText(
      message,
    );
    await expect(issue).toBeDisabled();
    await expect(page.getByLabel("Bootstrap token (shown once)")).toHaveCount(
      0,
    );
    expect(posts).toBe(1);
  });
}

test("shows a fresh token despite clock skew and clears it when its lifetime elapses", async ({
  page,
}) => {
  await setup(page, true);
  await page.clock.install();
  let posts = 0;
  await page.route("**/api/v1/node-bootstrap-tokens", (route) => {
    posts += 1;
    return route.fulfill({
      status: 201,
      json: { id: alpha, token: secret, expires_at: "2025-01-01T00:15:00Z" },
    });
  });
  await page.goto("/nodes");
  await prepare(page);
  await page
    .getByRole("button", { name: "Create bootstrap token", exact: true })
    .click();
  await expect(page.getByLabel("Bootstrap token (shown once)")).toHaveValue(
    secret,
  );
  await page.clock.fastForward(900_000);
  await expect(page.getByLabel("Bootstrap token (shown once)")).toHaveCount(0);
  await expect(page.getByRole("dialog").getByRole("alert")).toContainText(
    "This token has expired",
  );
  expect(posts).toBe(1);
});
