import { readFile } from "node:fs/promises";
import { expect, test } from "./csp";
import { installConsoleMocks } from "./fixtures";

test("filters network audit, pages older flows and exports exact counters", async ({
  page,
}) => {
  const node = "019fc0a4-6d92-765c-a8a1-4af556614cc3";
  await installConsoleMocks(page, {
    workspace: { id: node, name: "Audit fixture", slug: "audit-fixture" },
    default404: true,
  });
  await page.route("**/api/v1/audit/events?**", (route) =>
    route.fulfill({ json: { items: [] } }),
  );
  await page.route("**/api/v1/audit/vpn?**", async (route) => {
    const query = new URL(route.request().url()).searchParams;
    expect(query.get("kind")).toBe("network");
    expect(query.get("destination_ip")).toBe("203.0.113.1");
    expect(query.get("username")).toBe("alice");
    const older = query.has("cursor");
    await route.fulfill({
      json: {
        items: [
          {
            node_id: node,
            event_id: older ? "event2" : "event1",
            session_id: "42",
            username: "alice",
            event_type: "flow",
            client_ip: "192.0.2.1",
            last_seen_at: "2026-08-03T12:00:00Z",
            destination_ip: "203.0.113.1",
            protocol: "tcp",
            bytes_out: "9007199254740993",
          },
        ],
        ...(older ? {} : { next_cursor: "older" }),
      },
    });
  });
  await page.goto("/audit");
  await page.getByLabel("Audit source").selectOption("network");
  await page.getByLabel("Destination IP", { exact: true }).fill("203.0.113.1");
  await page.getByLabel("Username", { exact: true }).fill("alice");
  await page.getByRole("button", { name: "Search history" }).click();
  await page.getByRole("button", { name: "Load older records" }).click();
  await expect(
    page.getByRole("cell", { name: "9007199254740993" }),
  ).toHaveCount(2);
  const downloading = page.waitForEvent("download");
  await page.getByRole("button", { name: "Export results" }).click();
  const download = await downloading;
  const path = await download.path();
  expect(path).toBeTruthy();
  const items: unknown = JSON.parse(await readFile(path, "utf8"));
  expect(items).toHaveLength(2);
  expect(items).toEqual(
    expect.arrayContaining([
      expect.objectContaining({ bytesOut: "9007199254740993" }),
    ]),
  );
  expect(
    await page.evaluate(
      () =>
        document.documentElement.scrollWidth <=
        document.documentElement.clientWidth,
    ),
  ).toBe(true);
});
