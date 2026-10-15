import type { Page } from "@playwright/test";
import { expect, test } from "./csp";

const alphaId = "019fc0a4-6d92-765c-a8a1-4af556614cc1";
const nodeId = "019fc0a4-6d92-765c-a8a1-4af556614cc3";

test.beforeEach(async ({ page }) => {
  await page.route("**/api/v1/**", (route) =>
    route.fulfill({ status: 404, json: {} }),
  );
  await page.route("**/api/v1/readyz", (route) => route.fulfill({ json: {} }));
  await page.route("**/api/v1/workspaces", (route) =>
    route.fulfill({
      json: {
        items: [{ id: alphaId, name: "Alpha", slug: "alpha", version: 1 }],
      },
    }),
  );
  await page.route("**/api/v1/nodes?**", (route) =>
    route.fulfill({ json: { items: [], page: { has_more: false } } }),
  );
});

async function expectNoPageOverflow(page: Page): Promise<void> {
  expect(
    await page.evaluate(
      () =>
        document.documentElement.scrollWidth -
        document.documentElement.clientWidth,
    ),
  ).toBeLessThanOrEqual(0);
}

test("skip link moves focus to the main content", async ({ page }) => {
  await page.goto("/settings");
  await expect(page.getByRole("heading", { name: "Settings" })).toBeVisible();
  await page.keyboard.press("Tab");
  const skip = page.getByRole("link", { name: "Skip to main content" });
  await expect(skip).toBeFocused();
  await expect(skip).toBeInViewport();
  await page.keyboard.press("Enter");
  await expect(page.locator("#main-content")).toBeFocused();
  await expect(page).toHaveURL(/\/settings$/);
});

test("desktop navigation marks the current section on detail routes", async ({
  page,
  isMobile,
}) => {
  test.skip(isMobile, "the sidebar is replaced by the navigation sheet");
  await page.goto(`/nodes/${nodeId}`);
  const navigation = page.getByRole("navigation", {
    name: "Primary navigation",
  });
  await expect(
    navigation.getByRole("link", { name: "Nodes", exact: true }),
  ).toHaveAttribute("aria-current", "page");
  await expect(navigation.locator("[aria-current]")).toHaveCount(1);
  await expect(
    page.getByRole("button", { name: "Open navigation" }),
  ).toBeHidden();
  await expect(page.getByTestId("workspace")).toBeVisible();
});

test("navigation sheet opens and closes from the keyboard", async ({
  page,
}) => {
  // Exercise the narrow layout in every engine, including desktop projects.
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto(`/nodes/${nodeId}`);
  await expect(page.getByTestId("workspace")).toBeVisible();
  await expect(page.getByTestId("workspace")).toContainText("Alpha");
  await expectNoPageOverflow(page);
  await expect(page.getByRole("navigation")).toHaveCount(0);

  // The modal sheet hides outside content from the accessibility tree.
  const trigger = page.locator('button[aria-label="Open navigation"]');
  await trigger.focus();
  await page.keyboard.press("Enter");
  const sheet = page.getByRole("dialog", { name: "Primary navigation" });
  await expect(sheet).toBeVisible();
  await expect(trigger).toHaveAttribute("aria-expanded", "true");
  await expect
    .poll(() =>
      sheet.evaluate((element) => element.contains(document.activeElement)),
    )
    .toBe(true);
  await expect(
    sheet.getByRole("link", { name: "Nodes", exact: true }),
  ).toHaveAttribute("aria-current", "page");
  // Portal content resolves theme tokens, sits above the shell and locks
  // page scrolling.
  expect(
    await sheet.evaluate(
      (element) => getComputedStyle(element).backgroundColor,
    ),
  ).toBe("rgb(255, 255, 255)");
  expect(
    await page.evaluate(() =>
      document
        .elementFromPoint(window.innerWidth - 10, 20)
        ?.getAttribute("data-slot"),
    ),
  ).toBe("sheet-overlay");
  expect(
    await page.evaluate(() => getComputedStyle(document.body).overflow),
  ).toBe("hidden");

  await page.keyboard.press("Escape");
  await expect(sheet).toHaveCount(0);
  await expect(trigger).toBeFocused();
  expect(
    await page.evaluate(() => getComputedStyle(document.body).overflow),
  ).not.toBe("hidden");

  await page.keyboard.press("Enter");
  await expect(sheet).toBeVisible();
  await sheet.getByRole("link", { name: "Settings", exact: true }).focus();
  await page.keyboard.press("Enter");
  await expect(page).toHaveURL(/\/settings$/);
  await expect(sheet).toHaveCount(0);
  await expect(page.locator("#main-content")).toBeFocused();
  await expect(page.getByRole("heading", { name: "Settings" })).toBeVisible();
  await expectNoPageOverflow(page);
});

test("long Workspace choices keep the section and readiness reachable", async ({
  page,
}, info) => {
  const betaId = "019fc0a4-6d92-765c-a8a1-4af556614cc2";
  const longName = "ProductionWorkspaceWithoutWhitespace".repeat(4);
  await page.route("**/api/v1/workspaces", (route) =>
    route.fulfill({
      json: {
        items: [
          { id: alphaId, name: longName, slug: "alpha", version: 1 },
          { id: betaId, name: `${longName}-beta`, slug: "beta", version: 1 },
        ],
      },
    }),
  );
  await page.goto("/settings");
  const select = page.getByLabel("Workspace", { exact: true });
  await expect(select).toHaveValue(alphaId);
  for (const width of [1280, 1440, 1920, 320, 390, 768]) {
    await page.setViewportSize({ width, height: 1000 });
    await expectNoPageOverflow(page);
    await expect(
      page.locator("header").first().getByText("Settings", { exact: true }),
    ).toBeVisible();
    await expect(page.getByTestId("readiness")).toHaveText("Ready");
    const bounds = await select.boundingBox();
    if (!bounds) throw new Error("Workspace select has no layout bounds");
    expect(bounds.width).toBeGreaterThanOrEqual(160);
    expect(bounds.x + bounds.width).toBeLessThanOrEqual(width);
    if (
      process.env.OCSERVIA_UI_EVIDENCE_DIR &&
      (width === 1440 || width === 390)
    )
      await page.screenshot({
        path: `${process.env.OCSERVIA_UI_EVIDENCE_DIR}/${info.project.name}/shell-workspace-${String(width)}.png`,
        fullPage: true,
      });
  }
  await select.selectOption(betaId);
  await expect(select).toHaveValue(betaId);
  await select.focus();
  await page.keyboard.press("ArrowUp");
  await page.keyboard.press("Enter");
  await expect(select).toHaveValue(alphaId);
  await page.setViewportSize({ width: 1280, height: 1000 });
  await page.evaluate(() => {
    document.documentElement.style.zoom = "2";
  });
  await expectNoPageOverflow(page);
  await select.focus();
  await expect(select).toBeFocused();
});
