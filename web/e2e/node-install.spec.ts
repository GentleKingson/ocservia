import type { Page } from "@playwright/test";
import { expect, test } from "./csp";
import { installConsoleMocks } from "./fixtures";
const alpha = "019fc0a4-6d92-765c-a8a1-4af556614fa1";
const secret = "oit1_aHR0cHM6Ly9jb250cm9sbGVyLmV4YW1wbGU." + "b".repeat(43);

async function setup(page: Page, empty: boolean) {
  await installConsoleMocks(page, {
    workspace: { id: alpha, name: "Alpha", slug: "alpha" },
    nodes: empty
      ? []
      : [
          {
            id: "019fc0a4-6d92-765c-a8a1-4af556614fb1",
            name: "existing-node",
            version: 1,
            trust_status: "active",
            connection_state: "online",
            freshness: "fresh",
            dropped: { security: 0, health: 0, aggregate: 0, raw: 0 },
            session_count: 0,
          },
        ],
  });
}
async function prepare(page: Page) {
  await page.getByRole("button", { name: "Install node", exact: true }).click();
  await page.getByLabel("Node name", { exact: true }).fill("edge-1");
  await page.getByLabel("Install Release tag").fill("v1.2.3");
  await page.getByRole("button", { name: "Continue", exact: true }).click();
}
for (const empty of [true, false]) {
  test(`installs from ${empty ? "empty" : "populated"} inventory with one scoped token request`, async ({
    page,
  }) => {
    await setup(page, empty);
    const logs: string[] = [];
    page.on("console", (message) => logs.push(message.text()));
    let posts = 0;
    await page.route("**/api/v1/node-bootstrap-tokens", async (route) => {
      posts += 1;
      expect(route.request().method()).toBe("POST");
      expect(route.request().headers()["x-workspace-id"]).toBe(alpha);
      expect(route.request().headers()["content-type"]).toBe(
        "application/json",
      );
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
}

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
