import { createSSRApp } from "vue";
import { renderToString } from "vue/server-renderer";
import { beforeEach, expect, it, vi } from "vitest";
const mocks: { fleet: Record<string, unknown> } = vi.hoisted(() => ({
  fleet: {},
}));
vi.mock("../src/shared/fleet", () => ({ useFleetStore: () => mocks.fleet }));
vi.mock("vue-router", () => ({ useRouter: () => ({ push: vi.fn() }) }));
vi.mock("vue-i18n", () => ({ useI18n: () => ({ t: (key: string) => key }) }));
import NodesView from "../src/views/NodesView.vue";
beforeEach(() => {
  mocks.fleet = {
    nodes: [
      {
        id: "node-a",
        name: "Retained node",
        freshness: "fresh",
        trustStatus: "active",
        connectionState: "online",
        sessionCount: 1,
      },
    ],
    initialized: true,
    loading: false,
    unavailable: false,
    online: 1,
    sessionCount: 1,
    direct: 1,
    relay: 0,
  };
});
async function render() {
  const app = createSSRApp(NodesView);
  app.config.globalProperties.$t = (key: string) => key;
  return renderToString(app);
}
it("retains rows without a large Loading during a second request", async () => {
  mocks.fleet.loading = true;
  const html = await render();
  expect(html).toContain("Retained node");
  expect(html).toContain("fleetRefreshing");
  expect(html).not.toContain('class="empty-state"');
});
it("retains rows with a stale snapshot message after refresh failure", async () => {
  mocks.fleet.unavailable = true;
  const html = await render();
  expect(html).toContain("Retained node");
  expect(html).toContain("fleetRefreshFailed");
  expect(html).not.toContain('class="empty-state"');
});
it("shows large Loading only before initialization", async () => {
  mocks.fleet.nodes = [];
  mocks.fleet.initialized = false;
  mocks.fleet.loading = true;
  const html = await render();
  expect(html).toContain('class="empty-state"');
  expect(html).toContain("loading");
});
it("distinguishes a successful empty list from first-load failure", async () => {
  mocks.fleet.nodes = [];
  expect(await render()).toContain("noNodes");
  mocks.fleet.initialized = false;
  mocks.fleet.unavailable = true;
  const html = await render();
  expect(html).toContain("systemsUnavailable");
  expect(html).not.toContain("noNodes");
});
