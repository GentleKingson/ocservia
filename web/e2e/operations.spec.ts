import { expect, test, type Page, type Request } from "@playwright/test";

const workspaceId = "019fc0a4-6d92-765c-a8a1-4af556614da1";
const nodeId = "019fc0a4-6d92-765c-a8a1-4af556614da2";
const otherNodeId = "019fc0a4-6d92-765c-a8a1-4af556614da3";
const thirdNodeId = "019fc0a4-6d92-765c-a8a1-4af556614da4";
const rolloutId = "019fc0a4-6d92-765c-a8a1-4af556614da5";
const plainUnknownId = "019fc0a4-6d92-765c-a8a1-4af556614db1";
const upgradeUnknownId = "019fc0a4-6d92-765c-a8a1-4af556614db2";
const failedApplyId = "019fc0a4-6d92-765c-a8a1-4af556614db3";

const operation = (id: string, state: string, extra = {}) => ({
  id,
  state,
  node_id: nodeId,
  version: 1,
  created_at: "2026-10-04T00:00:00Z",
  updated_at: "2026-10-04T00:01:00Z",
  ...extra,
});
const rolloutNode = (
  id: string,
  ordinal: number,
  batch: number,
  state: string,
) => ({
  node_id: id,
  ordinal,
  batch,
  state,
  operation_id: plainUnknownId,
  from_version: "0.1.1",
  failure_code: state === "failed" ? "upgrade_failed" : "",
});
const rollout = (state: string) => ({
  id: rolloutId,
  workspace_id: workspaceId,
  target_version: "0.2.0",
  state,
  batch_size: 2,
  stop_on_failure: true,
  reason: "partial rollout",
  approval_id: "019fc0a4-6d92-765c-a8a1-4af556614da6",
  created_by: "019fc0a4-6d92-765c-a8a1-4af556614da7",
  current_batch: 1,
  pause_code: state === "paused" ? "batch_failed" : "",
  created_at: "2026-10-04T00:00:00Z",
  updated_at: "2026-10-04T00:02:00Z",
  nodes: [
    rolloutNode(nodeId, 0, 0, "succeeded"),
    rolloutNode(otherNodeId, 1, 1, "failed"),
    rolloutNode(thirdNodeId, 2, 1, "unknown"),
  ],
  excluded: [],
});

async function stubShell(page: Page): Promise<Request[]> {
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
        items: [{ id: workspaceId, name: "Ops", slug: "ops", version: 1 }],
      },
    }),
  );
  return writes;
}

test("separates unknown outcomes, pages by cursor and refreshes details read-only", async ({
  page,
}) => {
  const writes = await stubShell(page);
  const listUrls: URL[] = [];
  await page.route("**/api/v1/agent-rollouts?**", (route) =>
    route.fulfill({ json: { rollouts: [] } }),
  );
  await page.route(/\/api\/v1\/operations(\?.*)?$/, (route) => {
    const url = new URL(route.request().url());
    listUrls.push(url);
    return route.fulfill({
      json: url.searchParams.get("cursor")
        ? {
            items: [
              operation(failedApplyId, "failed", {
                config_apply_state: "failed",
                config_apply_failure_code: "validation_failed",
              }),
            ],
            page: { has_more: false },
          }
        : {
            items: [
              operation(plainUnknownId, "unknown"),
              operation(upgradeUnknownId, "unknown", {
                agent_upgrade_state: "unknown",
                agent_upgrade_target_version: "0.2.0",
              }),
            ],
            page: { has_more: true, next_cursor: "cursor-2" },
          },
    });
  });
  let detailReads = 0;
  await page.route(`**/api/v1/operations/${failedApplyId}`, (route) => {
    detailReads += 1;
    return route.fulfill({
      json: operation(failedApplyId, "failed", {
        config_apply_state: "failed",
        config_apply_failure_code: "validation_failed",
      }),
    });
  });

  await page.goto("/operations");
  await expect(page.getByText("No fleet rollouts yet")).toBeVisible();
  const rows = page.getByRole("row");
  await expect(rows.filter({ hasText: plainUnknownId })).toContainText(
    "Unknown",
  );
  await expect(rows.filter({ hasText: upgradeUnknownId })).toContainText(
    "Outcome unknown",
  );

  await page.getByRole("button", { name: "Load more" }).click();
  await expect(rows.filter({ hasText: failedApplyId })).toContainText("Failed");
  await expect(page.getByRole("button", { name: "Load more" })).toHaveCount(0);
  expect(listUrls).toHaveLength(2);
  expect(listUrls[1]?.searchParams.get("cursor")).toBe("cursor-2");

  await page.getByRole("button", { name: failedApplyId }).click();
  const detail = page.getByTestId("operation-detail");
  await expect(detail).toContainText("validation_failed");
  await detail.getByRole("button", { name: "Refresh" }).click();
  await expect.poll(() => detailReads).toBe(2);
  expect(listUrls).toHaveLength(2);
  expect(writes).toHaveLength(0);

  await detail.getByRole("link", { name: nodeId }).click();
  await expect(page).toHaveURL(`/nodes/${nodeId}`);
});

test("keeps partial rollout failures visible and stops polling after leaving", async ({
  page,
}) => {
  const writes = await stubShell(page);
  await page.route(/\/api\/v1\/operations(\?.*)?$/, (route) =>
    route.fulfill({ json: { items: [], page: { has_more: false } } }),
  );
  await page.route("**/api/v1/agent-rollouts?**", (route) =>
    route.fulfill({ json: { rollouts: [rollout("running")] } }),
  );
  let rolloutReads = 0;
  await page.route(`**/api/v1/agent-rollouts/${rolloutId}`, (route) => {
    rolloutReads += 1;
    return route.fulfill({ json: rollout("running") });
  });

  await page.goto("/operations");
  await page.getByRole("link", { name: "0.2.0" }).click();
  await expect(page).toHaveURL(`/rollouts/${rolloutId}`);
  await expect(page.getByTestId("rollout-state")).toHaveText("Running");
  const totals = page.getByTestId("rollout-totals");
  await expect(totals).toContainText("1/3 Succeeded");
  await expect(totals).toContainText("1 Failed");
  await expect(totals).toContainText("1 Unknown");
  const batch = page.getByRole("region", { name: "Batch 1" });
  await expect(batch).toContainText("0/2 Succeeded");
  await expect(batch).toContainText("upgrade_failed");
  await expect(
    batch.getByRole("link", { description: otherNodeId }),
  ).toHaveAttribute("href", `/nodes/${otherNodeId}`);
  await expect.poll(() => rolloutReads).toBeGreaterThan(1);

  await page.getByRole("link", { name: "Back to operations" }).click();
  await expect(page).toHaveURL("/operations");
  const readsAfterLeaving = rolloutReads;
  await page.waitForTimeout(4500);
  expect(rolloutReads).toBe(readsAfterLeaving);
  expect(writes).toHaveLength(0);
});

test("sends one resume request however often it is clicked", async ({
  page,
}) => {
  const writes = await stubShell(page);
  let release: (() => void) | undefined;
  await page.route(`**/api/v1/agent-rollouts/${rolloutId}`, (route) =>
    route.fulfill({ json: rollout("paused") }),
  );
  await page.route(
    `**/api/v1/agent-rollouts/${rolloutId}/resume`,
    async (route) => {
      await new Promise<void>((resolve) => (release = resolve));
      await route.fulfill({ json: rollout("running") });
    },
  );

  await page.goto(`/rollouts/${rolloutId}`);
  await expect(page.getByTestId("rollout-state")).toHaveText("Paused");
  const resume = page.getByRole("button", { name: "Resume rollout" });
  await resume.click();
  await expect(resume).toBeDisabled();
  await resume.click({ force: true });
  expect(writes).toHaveLength(1);
  release?.();
  await expect(page.getByTestId("rollout-state")).toHaveText("Running");
  expect(writes).toHaveLength(1);
  expect(writes[0]?.url()).toContain(`/agent-rollouts/${rolloutId}/resume`);
});
