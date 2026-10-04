import { expect, test, type Page, type Request } from "@playwright/test";

const workspaceId = "019fc0a4-6d92-765c-a8a1-4af556614ea1";
const nodeId = "019fc0a4-6d92-765c-a8a1-4af556614ea2";
const policyPath = `/api/v1/nodes/${nodeId}/users/alice/policy`;
const savedPolicy = {
  node_id: nodeId,
  username: "alice",
  quota_period: "monthly",
  quota_direction: "rx",
  quota_bytes: 0,
  version: 4,
  period_start: "2026-10-01T00:00:00Z",
  observed_rx_bytes: 0,
  observed_tx_bytes: 0,
  exceeded: false,
  expired: false,
  convergence: "converged",
};

async function openNode(
  page: Page,
  effectiveActions: Record<string, unknown>,
): Promise<Request[]> {
  const node = {
    id: nodeId,
    name: "Forms node",
    version: 7,
    effective_actions: effectiveActions,
    trust_status: "active",
    connection_state: "online",
    freshness: "fresh",
    dropped: { security: 0, health: 0, aggregate: 0, raw: 0 },
    session_count: 0,
  };
  const writes: Request[] = [];
  page.on("request", (request) => {
    if (request.method() !== "GET") writes.push(request);
  });
  await page.addInitScript(() => {
    class EventSourceStub {
      readonly readyState = 1;
      onerror = null;
      onmessage = null;
      onopen = null;
      addEventListener(): void {}
      removeEventListener(): void {}
      close(): void {}
    }
    Object.defineProperty(window, "EventSource", { value: EventSourceStub });
  });
  await page.route("**/api/v1/readyz", (route) => route.fulfill({ json: {} }));
  await page.route("**/api/v1/workspaces", (route) =>
    route.fulfill({
      json: {
        items: [{ id: workspaceId, name: "Forms", slug: "forms", version: 1 }],
      },
    }),
  );
  await page.route("**/api/v1/nodes?**", (route) =>
    route.fulfill({ json: { items: [node], page: { has_more: false } } }),
  );
  await page.route(`**/api/v1/nodes/${nodeId}`, (route) =>
    route.fulfill({ json: node }),
  );
  await page.route(`**/api/v1/nodes/${nodeId}/sessions?**`, (route) =>
    route.fulfill({ json: { items: [], page: { has_more: false } } }),
  );
  await page.route(`**/api/v1/nodes/${nodeId}/ip-bans`, (route) =>
    route.fulfill({ json: { items: [] } }),
  );
  await page.route(`**/api/v1/nodes/${nodeId}/user-group-state`, (route) =>
    route.fulfill({
      json: {
        items: [
          {
            kind: "user",
            name: "alice",
            desired_enabled: true,
            desired_version: 4,
            desired_revision: 4,
            convergence: "converged",
            recovery_required: false,
          },
        ],
      },
    }),
  );
  await page.goto(`/nodes/${nodeId}`);
  await expect(page.getByRole("heading", { name: "Forms node" })).toBeVisible();
  return writes;
}

test("explains denied and unknown permissions on disabled controls", async ({
  page,
}) => {
  const writes = await openNode(page, {
    "user.manage": { allowed: false, reason: "forbidden" },
    "service.reload": { allowed: false, reason: "authorization_unavailable" },
  });
  const forbidden = "Your role does not allow this action on this node.";
  const create = page.getByRole("button", { name: "Create user" });
  await expect(create).toBeDisabled();
  await expect(create).toHaveAttribute("title", forbidden);
  await expect(page.getByText(forbidden)).toBeVisible();
  await expect(
    page.getByText("Reload: Action permissions could not be read."),
  ).toBeVisible();
  await expect(page.getByRole("button", { name: "Groups" })).toBeEnabled();
  await page.getByRole("button", { name: "Groups" }).click();
  const apply = page.getByRole("button", { name: "Apply group" });
  await expect(apply).toBeDisabled();
  await expect(apply).toHaveAttribute(
    "title",
    "Action availability has not been read. Refresh the node details.",
  );
  expect(writes).toHaveLength(0);
});

test("closes a form with Escape or Cancel without writing", async ({
  page,
}) => {
  const writes = await openNode(page, {
    "user.manage": { allowed: true, reason: "available" },
  });
  await page.route(`**${policyPath}`, (route) =>
    route.fulfill({ json: savedPolicy }),
  );
  const trigger = page.getByRole("button", { name: "Quota and expiry" });
  const dialog = page.getByRole("dialog", { name: "Quota and expiry" });

  await trigger.click();
  await expect(dialog).toBeVisible();
  await expect(dialog.getByLabel("Quota size")).toHaveValue("0");
  await dialog.getByLabel("Reason").fill("not sent");
  await page.keyboard.press("Escape");
  await expect(dialog).toHaveCount(0);
  await expect(trigger).toBeFocused();

  await trigger.click();
  await expect(dialog.getByLabel("Reason")).toHaveValue("");
  await dialog.getByRole("button", { name: "Cancel" }).click();
  await expect(dialog).toHaveCount(0);
  expect(writes).toHaveLength(0);
});

test("keeps inputs after a server error and submits once per attempt", async ({
  page,
}) => {
  await openNode(page, {
    "user.manage": { allowed: true, reason: "available" },
  });
  const puts: Record<string, unknown>[] = [];
  let release: (() => void) | undefined;
  await page.route(`**${policyPath}`, async (route) => {
    if (route.request().method() === "GET")
      return route.fulfill({ json: savedPolicy });
    puts.push(route.request().postDataJSON() as Record<string, unknown>);
    if (puts.length === 1)
      return route.fulfill({
        status: 409,
        contentType: "application/problem+json",
        json: {
          type: "https://ocservia.dev/problems/version-conflict",
          title: "Version conflict",
          status: 409,
        },
      });
    await new Promise<void>((resolve) => (release = resolve));
    return route.fulfill({ json: { ...savedPolicy, version: 5 } });
  });
  await page.getByRole("button", { name: "Quota and expiry" }).click();
  const dialog = page.getByRole("dialog", { name: "Quota and expiry" });
  await expect(dialog.getByText("0 disables at once")).toBeVisible();
  await dialog.getByLabel("Quota size").fill("5");
  await dialog.getByLabel("Quota unit").selectOption("GiB");
  await dialog.getByLabel("Reason").fill("raise quota");
  const confirm = dialog.getByRole("button", { name: "Confirm" });

  await confirm.click();
  await expect(dialog.getByRole("alert")).toBeVisible();
  await expect(dialog.getByLabel("Quota size")).toHaveValue("5");
  await expect(dialog.getByLabel("Reason")).toHaveValue("raise quota");
  expect(puts).toHaveLength(1);

  await confirm.click();
  await expect(confirm).toBeDisabled();
  await confirm.click({ force: true });
  await dialog.getByLabel("Quota size").press("Enter");
  expect(puts).toHaveLength(2);
  release?.();
  await expect(dialog).toHaveCount(0);
  expect(puts).toHaveLength(2);
  expect(puts[1]).toEqual({
    quota_period: "monthly",
    quota_direction: "rx",
    quota_bytes: 5_368_709_120,
    expected_version: 4,
    reason: "raise quota",
  });
});

test("requires an approval ID before an approved reload", async ({ page }) => {
  const writes = await openNode(page, {
    "service.reload": { allowed: true, reason: "available" },
  });
  await page.route(`**/api/v1/nodes/${nodeId}/service:reload`, (route) =>
    route.fulfill({
      status: 403,
      contentType: "application/problem+json",
      json: {
        type: "https://ocservia.dev/problems/approval-required",
        title: "Approval required",
        status: 403,
      },
    }),
  );
  await page.getByRole("button", { name: "Reload" }).click();
  const dialog = page.getByRole("dialog", { name: "Reload Ocserv" });
  const confirm = dialog.getByRole("button", { name: "Confirm" });
  await dialog.getByLabel("Reason").fill("apply config");
  await expect(confirm).toBeDisabled();
  await expect(
    dialog.getByText("This action runs only with an approved request."),
  ).toBeVisible();
  await dialog.getByLabel("Approval ID").fill("approval-42");
  await confirm.click();
  await expect(dialog).toHaveCount(0);
  await expect(page.getByRole("alert")).toBeVisible();

  expect(writes).toHaveLength(1);
  const request = writes[0];
  expect(request?.url()).toContain(`/nodes/${nodeId}/service:reload`);
  expect(request?.headers()["x-approval-id"]).toBe("approval-42");
  expect(request?.headers()["if-match"]).toBe('"revision-7"');
  expect(request?.postDataJSON()).toEqual({
    reason: "apply config",
    expected_version: 7,
    ttl_seconds: 60,
  });
});
