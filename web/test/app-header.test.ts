import { createSSRApp, h } from "vue";
import { renderToString } from "vue/server-renderer";
import { expect, it, vi } from "vitest";
vi.mock("vue-router", () => ({
  useRoute: () => ({ fullPath: "/", path: "/" }),
}));
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

it("shows the management Workspace as text without a selector", async () => {
  const html = await renderHeader({
    workspace: workspace("a", "Administration"),
  });
  expect(html).toContain("Administration");
  expect(html).not.toContain("<select");
});

it("renders the workspace slot empty before bootstrap loads", async () => {
  const html = await renderHeader();
  const content =
    html.match(/data-testid="workspace"[^>]*>([\s\S]*?)<\/div>/)?.[1] ?? "";
  expect(content).not.toContain("[workspace]");
  expect(html).not.toContain("<select");
});
