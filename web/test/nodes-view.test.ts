import { createSSRApp } from "vue";
import { renderToString } from "vue/server-renderer";
import { beforeEach, expect, it, vi } from "vitest";
const mocks: {
  fleet: Record<string, unknown>;
  query: Record<string, string>;
} = vi.hoisted(() => ({ fleet: {}, query: {} }));
vi.mock("../src/shared/fleet", () => ({ useFleetStore: () => mocks.fleet }));
vi.mock("vue-router", () => ({
  useRoute: () => ({ query: mocks.query }),
  useRouter: () => ({ push: vi.fn(), replace: vi.fn() }),
}));
vi.mock("vue-i18n", () => ({
  useI18n: () => ({ t: (key: string) => key, locale: { value: "en" } }),
}));
import NodesView from "../src/views/NodesView.vue";
beforeEach(() => {
  mocks.query = {};
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
  app.component("RouterLink", {
    props: ["to"],
    template: "<a><slot /></a>",
  });
  return renderToString(app);
}
it("retains rows without a large Loading during a second request", async () => {
  mocks.fleet.loading = true;
  const html = await render();
  expect(html).toContain("Retained node");
  expect(html).toContain("fleetRefreshing");
  expect(html).not.toContain('role="alert"');
});
it("retains rows with a stale snapshot message after refresh failure", async () => {
  mocks.fleet.unavailable = true;
  const html = await render();
  expect(html).toContain("Retained node");
  expect(html).toContain("fleetRefreshFailed");
  expect(html).toContain("fleetCountScopeStale");
});
it("shows large Loading only before initialization", async () => {
  mocks.fleet.nodes = [];
  mocks.fleet.initialized = false;
  mocks.fleet.loading = true;
  const html = await render();
  expect(html).toContain('role="status"');
  expect(html).toContain("loading");
  expect(html).not.toContain("noNodes");
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
it("distinguishes no filter results from an empty fleet", async () => {
  mocks.query = { trust: "revoked" };
  const html = await render();
  expect(html).not.toContain("Retained node");
  expect(html).toContain("noMatchingNodes");
  expect(html).toContain("clearFilters");
  expect(html).not.toContain("noNodes");
});
it("keeps unknown optional fields readable", async () => {
  mocks.query = { columns: "path,ocserv,platform" };
  const html = await render();
  expect(html).toContain("Retained node");
  expect(html).toContain("notObserved");
  expect(html).toContain("versionNotObserved");
  expect(html).toContain("unknown");
  expect(html).toContain("notAvailable");
});
it("shows a neutral loading badge before the first fleet snapshot", async () => {
  mocks.fleet.initialized = false;
  mocks.fleet.loading = true;
  const html = await render();
  expect(html).not.toContain("liveTelemetry");
  expect(html).not.toContain("systemsUnavailable");
});
