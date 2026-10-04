import { createSSRApp, h } from "vue";
import { renderToString } from "vue/server-renderer";
import { expect, it, vi } from "vitest";
vi.mock("vue-router", () => ({ useRoute: () => ({ fullPath: "/" }) }));
import AppHeader from "../src/components/layout/AppHeader.vue";

const workspace = (id: string, name: string) => ({
  id,
  name,
  slug: id,
  version: 1,
});

async function renderHeader(
  props: Record<string, unknown> = {},
): Promise<string> {
  const app = createSSRApp({
    render: () =>
      h(AppHeader, {
        workspaces: [],
        workspaceId: "",
        readiness: "ready",
        ...props,
      }),
  });
  app.config.globalProperties.$t = (key: string) => `[${key}]`;
  app.component("RouterLink", { template: "<a><slot /></a>" });
  return renderToString(app);
}

async function readinessText(
  readiness: "loading" | "ready" | "unavailable",
): Promise<string> {
  const html = await renderHeader({ readiness });
  return (
    html.match(/data-testid="readiness"[^>]*>([\s\S]*?)<\/div>/)?.[1] ?? ""
  );
}

it("keeps readiness neutral until the first check settles", async () => {
  const loading = await readinessText("loading");
  expect(loading).toContain("[loading]");
  expect(loading).not.toContain("bg-destructive");
  expect(await readinessText("ready")).toContain("[ready]");
  expect(await readinessText("unavailable")).toContain("bg-destructive");
});

it("shows a single Workspace as text instead of a locked selector", async () => {
  const html = await renderHeader({
    workspaces: [workspace("a", "Administration")],
    workspaceId: "a",
  });
  expect(html).toContain("Administration");
  expect(html).not.toContain("<select");
});

it("offers a selector when several Workspaces are authorized", async () => {
  const html = await renderHeader({
    workspaces: [workspace("a", "Alpha"), workspace("b", "Beta")],
    workspaceId: "a",
  });
  const select = html.match(/<select id="workspace-select"[^>]*>/)?.[0];
  expect(select).toBeDefined();
  expect(select).not.toContain("disabled");
});
