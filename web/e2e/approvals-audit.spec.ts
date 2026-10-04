import { expect, test, type Page, type Request } from "@playwright/test";

const workspace = "019fc0a4-6d92-765c-a8a1-4af556614fa1";
const approvalId = "019fc0a4-6d92-765c-a8a1-4af556614fa2";
const nodeId = "019fc0a4-6d92-765c-a8a1-4af556614fa3";
const approval = (extra = {}) => ({
  id: approvalId,
  workspace_id: workspace,
  requester_id: "019fc0a4-6d92-765c-a8a1-4af556614fa4",
  action: "service.reload",
  resource_type: "node",
  resource_id: nodeId,
  reason: "maintenance",
  status: "pending",
  request_hash: "cd".repeat(32),
  request_summary: { action: "service.reload", resource_id: nodeId },
  created_at: "2026-09-30T00:00:00Z",
  expires_at: "2099-09-30T01:00:00Z",
  ...extra,
});
const audit = (id: string, action: string, result: string, extra = {}) => ({
  id,
  occurred_at: "2026-10-04T00:00:00Z",
  actor_type: "user",
  actor_id: "019fc0a4-6d92-765c-a8a1-4af556614fa5",
  action,
  resource_type: "node",
  resource_id: nodeId,
  node_id: null,
  request_id: `request-${id}`,
  trace_id: null,
  command_id: null,
  approval_id: null,
  result,
  reason: null,
  error_type: null,
  previous_event_hash: "",
  event_hash: "ef".repeat(32),
  ...extra,
});

async function stubShell(page: Page): Promise<Request[]> {
  const writes: Request[] = [];
  page.on("request", (request) => {
    if (request.method() !== "GET") writes.push(request);
  });
  await page.route("**/api/v1/readyz", (route) => route.fulfill({ json: {} }));
  await page.route("**/api/v1/workspaces", (route) =>
    route.fulfill({
      json: {
        items: [{ id: workspace, name: "Review", slug: "review", version: 1 }],
      },
    }),
  );
  await page.route("**/api/v1/approval-requests?**", (route) =>
    route.fulfill({ json: { items: [], page: { has_more: false } } }),
  );
  return writes;
}

test("explains used and expired deep links without a decision form", async ({
  page,
}) => {
  const writes = await stubShell(page);
  let current = approval({
    status: "consumed",
    approver_id: "019fc0a4-6d92-765c-a8a1-4af556614fa6",
  });
  await page.route(`**/api/v1/approval-requests/${approvalId}`, (route) =>
    route.fulfill({ json: current }),
  );
  await page.goto(`/approvals/${approvalId}`);
  await expect(page.getByTestId("approval-status")).toHaveText("Used");
  await expect(page.getByTestId("approval-status-help")).toContainText(
    "resulting operation",
  );
  await expect(page.getByRole("button", { name: "Approve" })).toHaveCount(0);

  current = approval({ expires_at: "2020-01-01T00:00:00Z" });
  await page.getByRole("button", { name: "Refresh", exact: true }).click();
  await expect(page.getByTestId("approval-status")).toHaveText("Expired");
  await expect(page.getByRole("button", { name: "Approve" })).toHaveCount(0);
  expect(writes).toHaveLength(0);
});

test("does not resend a decision after a conflict and separates approval from execution", async ({
  page,
}) => {
  const writes = await stubShell(page);
  let approved = false;
  await page.route(`**/api/v1/approval-requests/${approvalId}`, (route) =>
    route.fulfill({
      json: approval(
        approved
          ? {
              status: "approved",
              approver_id: "019fc0a4-6d92-765c-a8a1-4af556614fa6",
            }
          : {},
      ),
    }),
  );
  await page.route(
    `**/api/v1/approval-requests/${approvalId}:approve`,
    (route) => {
      approved = true;
      return route.fulfill({
        status: 409,
        contentType: "application/problem+json",
        json: { title: "Conflict", status: 409 },
      });
    },
  );
  await page.goto(`/approvals/${approvalId}`);
  await expect(page.getByTestId("approval-status")).toHaveText("Pending");
  await page.getByLabel("Decision reason").fill("checked");
  await page
    .getByLabel("I have reviewed this request and its bound content")
    .check();
  await page.getByRole("button", { name: "Approve" }).click();
  await expect(page.getByRole("alert")).toContainText(
    "Approval was not confirmed",
  );
  await expect(page.getByTestId("approval-status")).toHaveCount(0);
  expect(writes).toHaveLength(1);
  expect(writes[0]?.url()).toContain(
    `/approval-requests/${approvalId}:approve`,
  );

  await page.getByRole("button", { name: "Refresh", exact: true }).click();
  await expect(page.getByTestId("approval-status")).toHaveText("Approved");
  await expect(page.getByTestId("approval-status-help")).toContainText(
    "Approved, not run",
  );
  expect(writes).toHaveLength(1);
});

test("filters the bounded audit slice locally and links details", async ({
  page,
}) => {
  const writes = await stubShell(page);
  const reads: URL[] = [];
  let denied = true;
  await page.route("**/api/v1/audit/events?**", (route) => {
    reads.push(new URL(route.request().url()));
    if (denied)
      return route.fulfill({
        status: 403,
        contentType: "application/problem+json",
        json: { title: "Forbidden", status: 403 },
      });
    return route.fulfill({
      json: {
        items: [
          audit("audit-1", "service.reload", "intent", {
            approval_id: approvalId,
            node_id: nodeId,
          }),
          audit("audit-2", "user.disable", "failed", {
            error_type: "agent_unavailable",
          }),
        ],
      },
    });
  });
  await page.goto("/audit");
  await expect(page.getByRole("alert")).toContainText(
    "You do not have permission",
  );
  denied = false;
  await page.getByRole("button", { name: "Try again" }).click();
  await expect(page.getByRole("alert")).toHaveCount(0);
  const rows = page.getByRole("row");
  await expect(rows.filter({ hasText: "service.reload" })).toContainText(
    "Intent",
  );

  await page.getByLabel("Result").selectOption("failed");
  await expect(rows.filter({ hasText: "service.reload" })).toHaveCount(0);
  await page.getByRole("button", { name: "Details: user.disable" }).click();
  await expect(page.getByText("agent_unavailable")).toBeVisible();

  await page.getByLabel("Result").selectOption("");
  await page.getByLabel("Search loaded records").fill("missing");
  await expect(
    page.getByText("No loaded records match the filters"),
  ).toBeVisible();
  await page.getByLabel("Search loaded records").fill("service");
  const toggle = page.getByRole("button", { name: "Details: service.reload" });
  await toggle.click();
  await expect(toggle).toHaveAttribute("aria-expanded", "true");
  await expect(page.getByRole("link", { name: approvalId })).toHaveAttribute(
    "href",
    `/approvals/${approvalId}`,
  );
  await expect(page.getByRole("link", { name: nodeId })).toHaveAttribute(
    "href",
    `/nodes/${nodeId}`,
  );

  expect(reads).toHaveLength(2);
  for (const url of reads) {
    expect(url.searchParams.get("page_size")).toBe("50");
    expect(url.searchParams.has("cursor")).toBe(false);
  }
  expect(writes).toHaveLength(0);
});

test("shows a confirmed empty audit result", async ({ page }) => {
  await stubShell(page);
  await page.route("**/api/v1/audit/events?**", (route) =>
    route.fulfill({ json: { items: [] } }),
  );
  await page.goto("/audit");
  await expect(page.getByText("No audit records yet")).toBeVisible();
  await expect(page.getByLabel("Search loaded records")).toHaveCount(0);
});
