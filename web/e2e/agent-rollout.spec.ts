import { expect, test, type Route } from "@playwright/test";

const workspaceId = "019fc0a4-6d92-765c-a8a1-4af556614ee1";
const canaryNodeId = "019fc0a4-6d92-765c-a8a1-4af556614ee2";
const batchNodeId = "019fc0a4-6d92-765c-a8a1-4af556614ee3";
const otherNodeId = "019fc0a4-6d92-765c-a8a1-4af556614ee4";
const rolloutId = "019fc0a4-6d92-765c-a8a1-4af556614ee5";
const recommended = "0.2.0";

const nodeRow = (id: string, name: string) => ({
  id,
  name,
  version: 1,
  trust_status: "active",
  connection_state: "online",
  freshness: "fresh",
  dropped: { security: 0, health: 0, aggregate: 0, raw: 0 },
  session_count: 0,
  recommended_agent_version: recommended,
  agent_version: "0.1.1",
  agent_version_state: "upgrade_available",
  architecture: "amd64",
  agent_upgrade_eligible: true,
});

const rollout = (state: string, nodes: Record<string, unknown>[]) => ({
  id: rolloutId,
  workspace_id: workspaceId,
  target_version: recommended,
  state,
  batch_size: 5,
  stop_on_failure: true,
  reason: "fleet rollout e2e",
  approval_id: "019fc0a4-6d92-765c-a8a1-4af556614ee6",
  created_by: "019fc0a4-6d92-765c-a8a1-4af556614ee7",
  current_batch: nodes.some((node) => node.batch === 1) ? 1 : 0,
  pause_code: "",
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:00:00Z",
  nodes,
  excluded: [],
});

const node = (id: string, ordinal: number, batch: number, state: string) => ({
  node_id: id,
  ordinal,
  batch,
  state,
  operation_id: "019fc0a4-6d92-765c-a8a1-4af556614ee8",
  from_version: "0.1.1",
  failure_code: state === "failed" ? "upgrade_failed" : "",
});

test.beforeEach(async ({ page }) => {
  await page.addInitScript(() => {
    class EventSourceStub {
      static readonly CONNECTING = 0;
      static readonly OPEN = 1;
      static readonly CLOSED = 2;
      readonly CONNECTING = 0;
      readonly OPEN = 1;
      readonly CLOSED = 2;
      readonly withCredentials = false;
      readonly readyState = 1;
      readonly url: string;
      onerror: ((event: Event) => void) | null = null;
      onmessage: ((event: MessageEvent) => void) | null = null;
      onopen: ((event: Event) => void) | null = null;
      constructor(url: string | URL) {
        this.url = String(url);
      }
      addEventListener(): void {}
      removeEventListener(): void {}
      dispatchEvent(): boolean {
        return true;
      }
      close(): void {}
    }
    Object.defineProperty(window, "EventSource", { value: EventSourceStub });
  });
  await page.route("**/api/v1/readyz", (route) =>
    route.fulfill({ status: 200, contentType: "application/json", body: "{}" }),
  );
  await page.route("**/api/v1/version", (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        version: "dev",
        commit: "unknown",
        role: "all",
        recommended_agent_version: recommended,
      }),
    }),
  );
  await page.route("**/api/v1/workspaces", (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        items: [
          { id: workspaceId, name: "Rollout", slug: "rollout", version: 1 },
        ],
      }),
    }),
  );
  await page.route("**/api/v1/nodes?**", (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        items: [
          nodeRow(canaryNodeId, "node-a"),
          nodeRow(batchNodeId, "node-b"),
          nodeRow(otherNodeId, "node-c"),
        ],
        page: { has_more: false },
      }),
    }),
  );
});

async function fulfillRolloutCreate(route: Route): Promise<void> {
  const body = route.request().postDataJSON() as Record<string, unknown>;
  const nodeIds = body.node_ids as unknown[];
  if (
    body.target_version !== recommended ||
    !Array.isArray(nodeIds) ||
    nodeIds.length !== 3 ||
    typeof body.approval_id !== "string" ||
    body.approval_id.length === 0 ||
    typeof body.reason !== "string" ||
    body.reason.length === 0 ||
    body.batch_size !== 5
  ) {
    await route.fulfill({
      status: 400,
      contentType: "application/json",
      body: "{}",
    });
    return;
  }
  await route.fulfill({
    status: 201,
    contentType: "application/json",
    body: JSON.stringify(
      rollout("queued", [
        node(canaryNodeId, 0, 0, "pending"),
        node(batchNodeId, 1, 1, "pending"),
        node(otherNodeId, 2, 1, "pending"),
      ]),
    ),
  });
}

test("runs the canary and batch rollout to completion", async ({ page }) => {
  await page.route("**/api/v1/agent-rollouts", fulfillRolloutCreate);
  const states = [
    rollout("running", [
      node(canaryNodeId, 0, 0, "running"),
      node(batchNodeId, 1, 1, "pending"),
      node(otherNodeId, 2, 1, "pending"),
    ]),
    rollout("running", [
      node(canaryNodeId, 0, 0, "succeeded"),
      node(batchNodeId, 1, 1, "running"),
      node(otherNodeId, 2, 1, "running"),
    ]),
    rollout("succeeded", [
      node(canaryNodeId, 0, 0, "succeeded"),
      node(batchNodeId, 1, 1, "succeeded"),
      node(otherNodeId, 2, 1, "succeeded"),
    ]),
  ];
  let fetches = 0;
  await page.route(`**/api/v1/agent-rollouts/${rolloutId}`, (route) => {
    fetches += 1;
    return route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(states[Math.min(fetches - 1, states.length - 1)]),
    });
  });

  await page.goto("/nodes");
  await page.getByRole("checkbox").first().check();
  await page.getByRole("checkbox").nth(1).check();
  await page.getByRole("checkbox").nth(2).check();
  await page.getByRole("button", { name: /Rolling upgrade \(3\)/ }).click();
  await expect(page.locator("#rollout-target")).toHaveText(recommended);
  await page.locator("#rollout-reason").fill("fleet rollout e2e");
  await page
    .locator("#rollout-approval")
    .fill("019fc0a4-6d92-765c-a8a1-4af556614ee6");
  await page.getByRole("button", { name: "Start rollout" }).click();

  await expect(
    page.getByRole("heading", { name: "Rolling upgrade" }),
  ).toBeVisible();
  await expect(page.getByText("Canary").first()).toBeVisible();
  await expect(page.locator(".health")).toHaveText("Running");
  await expect(page.locator(".health")).toHaveText("Succeeded", {
    timeout: 10000,
  });
});

test("pauses on a failed node and resumes only the failed node", async ({
  page,
}) => {
  await page.route("**/api/v1/agent-rollouts", fulfillRolloutCreate);
  const states = [
    rollout("paused", [
      node(canaryNodeId, 0, 0, "succeeded"),
      node(batchNodeId, 1, 1, "failed"),
      node(otherNodeId, 2, 1, "pending"),
    ]),
    rollout("running", [
      node(canaryNodeId, 0, 0, "succeeded"),
      node(batchNodeId, 1, 1, "running"),
      node(otherNodeId, 2, 1, "pending"),
    ]),
  ];
  let resumed = false;
  await page.route(`**/api/v1/agent-rollouts/${rolloutId}`, (route) => {
    const current = resumed ? states[1] : states[0];
    return route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(current),
    });
  });
  await page.route(`**/api/v1/agent-rollouts/${rolloutId}/resume`, (route) => {
    resumed = true;
    return route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(
        rollout("running", [
          node(canaryNodeId, 0, 0, "succeeded"),
          node(batchNodeId, 1, 1, "pending"),
          node(otherNodeId, 2, 1, "pending"),
        ]),
      ),
    });
  });

  await page.goto(`/rollouts/${rolloutId}`);
  await expect(page.getByRole("alert")).toContainText(
    "No next batch starts automatically",
  );
  await expect(
    page.getByRole("button", { name: "Resume rollout" }),
  ).toBeEnabled();
  await page.getByRole("button", { name: "Resume rollout" }).click();
  await expect(page.locator(".health")).toHaveText("Running");
  expect(resumed).toBe(true);
});
