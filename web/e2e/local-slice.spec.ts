import { expect, test } from "@playwright/test";

test("runs and rebuilds the local vertical slice", async ({ page }) => {
  const pageErrors: string[] = [];
  page.on("pageerror", (error) => pageErrors.push(error.message));
  await page.goto("/dev");
  await page.waitForTimeout(500);
  expect(pageErrors).toEqual([]);
  await expect(
    page.getByRole("heading", { name: "Development" }),
  ).toBeVisible();

  const operationResponse = page.waitForResponse(
    (response) =>
      response.url().endsWith("/api/v1/development/simulations") &&
      response.status() === 202,
  );
  await page.getByTestId("run-probe").click();
  const response = await operationResponse;
  const operation = (await response.json()) as { node_id: string };

  await expect(page.getByText("Completed").first()).toBeVisible();
  await expect(page.getByTestId("pending-operations")).toHaveText("0");
  await expect
    .poll(async () =>
      Number(await page.getByTestId("active-nodes").innerText()),
    )
    .toBeGreaterThan(0);

  const eventPage = await page.request.get("/api/v1/events?page_size=200");
  expect(eventPage.ok()).toBeTruthy();
  const events = (await eventPage.json()) as {
    items: Array<{ id: string; node_id: string; traceparent: string }>;
  };
  const nodeEvents = events.items.filter(
    (event) => event.node_id === operation.node_id,
  );
  expect(nodeEvents.length).toBeGreaterThanOrEqual(5);
  expect(new Set(nodeEvents.map((event) => event.id)).size).toBe(
    nodeEvents.length,
  );
  expect(nodeEvents.every((event) => event.traceparent.startsWith("00-"))).toBe(
    true,
  );

  await page.reload();
  await expect(
    page.getByTestId("event-list").getByText("Completed").first(),
  ).toBeVisible();
});
