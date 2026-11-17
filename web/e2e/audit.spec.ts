import { expect, test } from "@playwright/test";

const workspaceId = "019fc0a4-6d92-765c-a8a1-4af556614fa1";
const nodeId = "019fc0a4-6d92-765c-a8a1-4af556614fa3";

test("reads bounded workspace audit records and verifies the hash chain", async ({
  page,
}) => {
  const approvalRequests: string[] = [];
  page.on("request", (request) => {
    if (request.url().includes("/api/v1/approval-requests")) {
      approvalRequests.push(request.url());
    }
  });
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
          {
            id: workspaceId,
            name: "Administration",
            slug: "admin",
            version: 1,
          },
        ],
      },
    }),
  );

  let auditRequests = 0;
  await page.route("**/api/v1/audit/events?**", (route) => {
    auditRequests += 1;
    expect(route.request().headers()["x-workspace-id"]).toBe(workspaceId);
    expect(route.request().url()).toContain("page_size=50");
    return route.fulfill({
      json: {
        items: [
          {
            id: "019fc0a4-6d92-765c-a8a1-4af556614fa4",
            occurred_at: "2026-09-30T10:00:00Z",
            actor_type: "local_admin",
            actor_id: "019fc0a4-6d92-765c-a8a1-4af556614fa5",
            action: "node.revocation",
            resource_type: "node",
            resource_id: nodeId,
            node_id: nodeId,
            request_id: "req-1",
            trace_id: "trace-1",
            command_id: null,
            result: "succeeded",
            reason: "decommission",
            error_type: null,
            previous_event_hash: "0".repeat(64),
            event_hash: "a".repeat(64),
          },
          {
            id: "019fc0a4-6d92-765c-a8a1-4af556614fa6",
            occurred_at: "2026-09-30T09:00:00Z",
            actor_type: "local_admin",
            actor_id: "019fc0a4-6d92-765c-a8a1-4af556614fa5",
            action: "user.create",
            resource_type: "ocserv_user",
            resource_id: "alice",
            node_id: nodeId,
            request_id: "req-2",
            trace_id: null,
            command_id: null,
            result: "failed",
            reason: null,
            error_type: "node_offline",
            previous_event_hash: "b".repeat(64),
            event_hash: "c".repeat(64),
          },
        ],
      },
    });
  });

  let verifyCount = 0;
  await page.route("**/api/v1/audit:verify", (route) => {
    verifyCount += 1;
    expect(route.request().method()).toBe("POST");
    expect(route.request().headers()["x-workspace-id"]).toBe(workspaceId);
    if (verifyCount === 1) {
      return route.fulfill({
        status: 200,
        json: {
          workspace_id: workspaceId,
          events: 2,
          valid: true,
          checkpoint_valid: true,
        },
      });
    }
    return route.fulfill({
      status: 409,
      json: {
        workspace_id: workspaceId,
        events: 2,
        valid: false,
        checkpoint_valid: false,
      },
    });
  });

  await page.goto("/audit");
  await expect(
    page.getByText("node.revocation", { exact: true }),
  ).toBeVisible();
  await expect(page.getByText("user.create", { exact: true })).toBeVisible();
  await expect(
    page.getByText("Most recent 50 audit records in this workspace"),
  ).toBeVisible();

  await page.getByLabel("Result", { exact: true }).selectOption("failed");
  await expect(page.getByText("node.revocation")).toHaveCount(0);
  await expect(page.getByText("user.create", { exact: true })).toBeVisible();
  await page.getByLabel("Result", { exact: true }).selectOption("");
  await page.getByLabel("Search loaded records").fill("decommission");
  await expect(page.getByText("node.revocation")).toHaveCount(0);
  await page.getByLabel("Search loaded records").fill("");

  await page.getByRole("button", { name: "Details: node.revocation" }).click();
  await expect(page.getByText("a".repeat(64), { exact: true })).toBeVisible();
  await expect(
    page.getByRole("link", { name: nodeId, exact: true }),
  ).toHaveAttribute("href", `/nodes/${nodeId}`);
  expect(auditRequests).toBe(1);
  expect(approvalRequests).toEqual([]);

  await page.getByRole("button", { name: "Verify chain", exact: true }).click();
  await expect(page.getByTestId("audit-verification")).toContainText(
    "Audit chain verified",
  );
  await expect(page.getByTestId("audit-verification")).toContainText(
    "2 audit records checked",
  );

  await page.getByRole("button", { name: "Verify chain", exact: true }).click();
  await expect(page.getByTestId("audit-verification")).toContainText(
    "Audit chain invalid",
  );
  expect(verifyCount).toBe(2);
});
