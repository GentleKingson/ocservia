import type { Route } from "@playwright/test";
import { expect, test } from "./csp";
import { installConsoleMocks } from "./fixtures";

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
  await installConsoleMocks(page, {
    workspace: { id: workspaceId, name: "Rollout", slug: "rollout" },
    recommendedAgentVersion: recommended,
    nodes: [
      nodeRow(canaryNodeId, "node-a"),
      nodeRow(batchNodeId, "node-b"),
      nodeRow(otherNodeId, "node-c"),
    ],
  });
});

async function fulfillRolloutCreate(route: Route): Promise<void> {
  const body = route.request().postDataJSON() as Record<string, unknown>;
  const nodeIds = body.node_ids as unknown[];
  if (
    body.target_version !== recommended ||
    !Array.isArray(nodeIds) ||
    nodeIds.length !== 3 ||
    "approval_id" in body ||
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

test("refreshes the Nodes list without showing a progress message", async ({
  page,
}) => {
  await page.clock.install();
  await page.goto("/nodes");
  await expect(page.getByRole("row", { name: /node-a/ })).toBeVisible();
  const path = "**/api/v1/nodes?**";
  const release = Promise.withResolvers<undefined>();
  await page.route(path, async (route) => {
    await release.promise;
    await route.fallback();
  });
  const request = page.waitForRequest(path);
  try {
    await page.clock.fastForward(16_000);
    await request;
    await expect(page.getByText("Refreshing nodes…")).toHaveCount(0);
    await expect(page.getByRole("row", { name: /node-a/ })).toBeVisible();
  } finally {
    release.resolve(undefined);
  }
});

test("keeps the last rollout details through a temporary polling failure", async ({
  page,
}) => {
  await page.clock.install();
  let polls = 0;
  await page.route(`**/api/v1/agent-rollouts/${rolloutId}`, (route) => {
    if (++polls === 2) return route.fulfill({ status: 503, json: {} });
    if (polls === 4) return route.fulfill({ status: 403, json: {} });
    return route.fulfill({ json: rollout("paused", []) });
  });
  await page.goto(`/rollouts/${rolloutId}`);
  const details = page.getByTestId("rollout-totals");
  const warning = page.getByText(
    "Refresh failed. Showing the last successful data.",
  );
  const resume = page.getByRole("button", { name: /resume/i });
  await expect(details).toBeVisible();
  await expect(resume).toBeEnabled();
  await page.clock.fastForward(2100);
  await expect(warning).toBeVisible();
  await expect(details).toBeVisible();
  await expect(resume).toBeDisabled();
  await page.clock.fastForward(2100);
  await expect(warning).toHaveCount(0);
  await expect(details).toBeVisible();
  await expect(resume).toBeEnabled();
  await page.clock.fastForward(2100);
  await expect(page.getByText("Rollout state is unavailable")).toBeVisible();
  await expect(details).toHaveCount(0);
  await expect(resume).toHaveCount(0);
});

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
  const open = page.getByRole("button", { name: /Rolling upgrade \(3\)/ });
  await open.click();
  await expect(
    page.getByRole("dialog", {
      name: "Start a canary and rolling agent upgrade",
    }),
  ).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(page.getByRole("dialog")).toHaveCount(0);
  await expect(open).toBeFocused();
  await open.click();
  await expect(page.locator("#rollout-target")).toHaveText(recommended);
  await page.locator("#rollout-reason").fill("fleet rollout e2e");
  await expect(page.locator("#rollout-approval")).toHaveCount(0);
  await page.getByRole("button", { name: "Start rollout" }).click();

  await expect(
    page.getByRole("heading", { name: "Rolling upgrade" }),
  ).toBeVisible();
  await expect(page.getByText("Canary").first()).toBeVisible();
  await expect(page.getByTestId("rollout-state")).toHaveText("Running");
  await expect(page.getByTestId("rollout-state")).toHaveText("Succeeded", {
    timeout: 10000,
  });
});

test("pauses on a failed node and resumes only the failed node", async ({
  page,
}) => {
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
  await expect(page.getByTestId("rollout-state")).toHaveText("Running");
  expect(resumed).toBe(true);
});

test("shows not found for a real 404 rollout response", async ({ page }) => {
  await page.route(`**/api/v1/agent-rollouts/${rolloutId}`, (route) =>
    route.fulfill({
      status: 404,
      contentType: "application/json",
      body: "{}",
    }),
  );
  await page.goto(`/rollouts/${rolloutId}`);
  await expect(
    page.getByText("Rollout not found in this workspace"),
  ).toBeVisible();
});

test("reads the next rollout after a terminal one on the same route", async ({
  page,
}) => {
  const nextRolloutId = "019fc0a4-6d92-765c-a8a1-4af556614ee9";
  await page.route(`**/api/v1/agent-rollouts/${rolloutId}`, (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(
        rollout("succeeded", [node(canaryNodeId, 0, 0, "succeeded")]),
      ),
    }),
  );
  await page.route(`**/api/v1/agent-rollouts/${nextRolloutId}`, (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        ...rollout("paused", [node(canaryNodeId, 0, 0, "failed")]),
        id: nextRolloutId,
      }),
    }),
  );
  await page.goto(`/rollouts/${rolloutId}`);
  await expect(page.getByTestId("rollout-state")).toHaveText("Succeeded");
  await page.evaluate((id) => {
    history.pushState({}, "", `/rollouts/${id}`);
    dispatchEvent(new PopStateEvent("popstate"));
  }, nextRolloutId);
  await expect(page.getByTestId("rollout-state")).toHaveText("Paused");
  await expect(
    page.getByRole("button", { name: "Resume rollout" }),
  ).toBeEnabled();
});
