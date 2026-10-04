import { createSSRApp, h } from "vue";
import { renderToString } from "vue/server-renderer";
import { expect, it, vi } from "vitest";
vi.mock("vue-router", () => ({ useRoute: () => ({ fullPath: "/" }) }));
import AppHeader from "../src/components/layout/AppHeader.vue";

async function readinessText(
  readiness: "loading" | "ready" | "unavailable",
): Promise<string> {
  const app = createSSRApp({
    render: () => h(AppHeader, { workspaces: [], workspaceId: "", readiness }),
  });
  app.config.globalProperties.$t = (key: string) => `[${key}]`;
  app.component("RouterLink", { template: "<a><slot /></a>" });
  const html = await renderToString(app);
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
