import { expect, test } from "@playwright/test";
const workspace = "019fc0a4-6d92-765c-a8a1-4af556614ea1";
const first = "019fc0a4-6d92-765c-a8a1-4af556614ea2";
const second = "019fc0a4-6d92-765c-a8a1-4af556614ea3";
const approval = (id: string, action: string) => ({
  id,
  workspace_id: workspace,
  requester_id: "019fc0a4-6d92-765c-a8a1-4af556614ea4",
  action,
  resource_type: "node",
  resource_id: "019fc0a4-6d92-765c-a8a1-4af556614ea5",
  reason: "maintenance",
  status: "pending",
  request_hash: "ab".repeat(32),
  request_summary: {
    action,
    resource_type: "node",
    resource_id: "019fc0a4-6d92-765c-a8a1-4af556614ea5",
  },
  created_at: "2026-09-30T00:00:00Z",
  expires_at: "2099-09-30T01:00:00Z",
});
test.beforeEach(async ({ page }) => {
  await page.route("**/api/v1/readyz", (route) =>
    route.fulfill({ status: 200, contentType: "application/json", body: "{}" }),
  );
  await page.route("**/api/v1/workspaces", (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        items: [{ id: workspace, name: "Queue", slug: "queue", version: 1 }],
      }),
    }),
  );
});
test("discovers a request and reuses bound independent approval details", async ({
  page,
}) => {
  let approved = false;
  await page.route("**/api/v1/approval-requests?**", (route) => {
    expect(route.request().headers()["x-workspace-id"]).toBe(workspace);
    expect(new URL(route.request().url()).searchParams.get("page_size")).toBe(
      "50",
    );
    return route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        items: approved ? [] : [approval(first, "service.reload")],
        page: { has_more: false },
      }),
    });
  });
  await page.route("**/api/v1/approval-requests/*", (route) => {
    if (route.request().method() === "POST") {
      expect(route.request().postDataJSON()).toEqual({
        expected_request_hash: "ab".repeat(32),
        reason: "independent review",
      });
      approved = true;
    }
    return route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        ...approval(first, "service.reload"),
        ...(approved
          ? {
              status: "approved",
              approver_id: "019fc0a4-6d92-765c-a8a1-4af556614ea6",
            }
          : {}),
      }),
    });
  });
  await page.goto("/approvals");
  await page
    .getByRole("button", { name: "service.reload", exact: true })
    .click();
  await expect(page.getByTestId("approval-hash")).toHaveText("ab".repeat(32));
  const decision = page.getByRole("button", { name: "Approve", exact: true });
  await expect(decision).toBeDisabled();
  await page.getByLabel("Decision reason").fill("independent review");
  await page
    .getByLabel("I have reviewed this request and its bound content")
    .check();
  await decision.click();
  await expect(page.getByTestId("approval-status")).toHaveText("Approved");
  await expect(
    page.getByText("No pending requests you can approve", { exact: true }),
  ).toBeVisible();
});
test("paginates and clears stale rows on a failed refresh", async ({
  page,
}) => {
  let denied = false;
  await page.route("**/api/v1/approval-requests?**", (route) => {
    if (denied) return route.fulfill({ status: 403 });
    const cursor = new URL(route.request().url()).searchParams.get("cursor");
    return route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        items: [
          approval(
            cursor ? second : first,
            cursor ? "user.batch.disable" : "service.reload",
          ),
        ],
        page: cursor
          ? { has_more: false }
          : { has_more: true, next_cursor: first },
      }),
    });
  });
  await page.goto("/approvals");
  await page.getByRole("button", { name: "Next page", exact: true }).click();
  await expect(
    page.getByRole("button", { name: "user.batch.disable", exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "service.reload", exact: true }),
  ).toHaveCount(0);
  denied = true;
  await page.getByRole("button", { name: "Refresh", exact: true }).click();
  await expect(page.getByRole("alert")).toBeVisible();
  await expect(
    page.getByRole("button", { name: "user.batch.disable", exact: true }),
  ).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "Next page", exact: true }),
  ).toHaveCount(0);
});
