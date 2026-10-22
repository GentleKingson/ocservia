import type { Page } from "@playwright/test";
import { expect, test } from "./csp";
const alpha = "019fc0a4-6d92-765c-a8a1-4af556614fa1";
const beta = "019fc0a4-6d92-765c-a8a1-4af556614fa2";
const secret = "obt1_" + "b".repeat(43);

async function setup(page: Page, empty: boolean) {
  await page.addInitScript(() => {
    class EventSourceStub {
      readyState = 1;
      onerror = null;
      onmessage = null;
      onopen = null;
      addEventListener() {}
      removeEventListener() {}
      close() {}
    }
    Object.defineProperty(window, "EventSource", { value: EventSourceStub });
  });
  await page.route("**/api/v1/readyz", (route) => route.fulfill({ json: {} }));
  await page.route("**/api/v1/workspaces", (route) =>
    route.fulfill({
      json: {
        items: [
          { id: alpha, name: "Alpha", slug: "alpha", version: 1 },
          { id: beta, name: "Beta", slug: "beta", version: 1 },
        ],
      },
    }),
  );
  await page.route("**/api/v1/nodes?**", (route) =>
    route.fulfill({
      json: {
        items: empty
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
        page: { has_more: false },
      },
    }),
  );
}
async function prepare(page: Page) {
  await page.getByRole("button", { name: "Install node", exact: true }).click();
  await page.getByLabel("Node name", { exact: true }).fill("edge-1");
  await page.getByLabel("Install Release tag").fill("v1.2.3");
  await page
    .getByLabel("Controller EndpointID", { exact: true })
    .fill("a".repeat(64));
  await page
    .getByLabel("Dedicated Relay HTTPS URL")
    .fill("https://relay.example.com");
  await page.getByRole("button", { name: "Prepare install.env" }).click();
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
      expect(route.request().headers()["x-workspace-id"]).toBe(beta);
      expect(route.request().headers()["content-type"]).toBe(
        "application/json",
      );
      expect(route.request().postDataJSON()).toEqual({
        workspace_id: beta,
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
    await page.getByLabel("Workspace", { exact: true }).selectOption(beta);
    await prepare(page);
    expect(posts).toBe(0);
    await expect(
      page.getByLabel("Run in the directory containing install.env"),
    ).toHaveValue(/--version v1\.2\.3/);
    await expect(page.getByText(/For a slow first installation/)).toContainText(
      "ENROLLMENT_READY",
    );
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
      await page.getByLabel("install.env (no secrets)").inputValue(),
    ).not.toContain(secret);
    expect(
      await page.evaluate(() => JSON.stringify([localStorage, sessionStorage])),
    ).not.toContain(secret);
    expect(page.url()).not.toContain(secret);
    expect(logs.join("\n")).not.toContain(secret);
    await page
      .getByRole("button", { name: "Token saved — installation instructions" })
      .click();
    await expect(page.getByLabel("Bootstrap token (shown once)")).toHaveCount(
      0,
    );
    expect(
      await page
        .getByLabel("Run in the directory containing install.env")
        .inputValue(),
    ).not.toContain(secret);
    await expect(page.getByText(/ENROLLED_LOCAL means/)).toBeVisible();
    await expect(page.getByText(/second authorized identity/)).toBeVisible();
    await expect(page.getByText(/trust_status=active/)).toBeVisible();
    expect(posts).toBe(1);
    const dialog = page.getByRole("dialog");
    const box = await dialog.boundingBox();
    const viewport = page.viewportSize();
    if (!box || !viewport) throw new Error("Missing dialog or viewport");
    expect(box.width).toBeLessThanOrEqual(viewport.width);
    await page.getByRole("button", { name: "Cancel", exact: true }).click();
    await prepare(page);
    await expect(page.getByLabel("Bootstrap token (shown once)")).toHaveCount(
      0,
    );
    expect(posts).toBe(1);
  });
}

test("clears a displayed token when Workspace changes and when the page is left", async ({
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
  // A second UI context can switch Workspace while the modal owns focus.
  await page
    .getByLabel("Workspace", { exact: true })
    .evaluate((element: HTMLSelectElement, id) => {
      element.value = id;
      element.dispatchEvent(new Event("change", { bubbles: true }));
    }, beta);
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
