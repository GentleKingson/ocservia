import type { NodeObservedState } from "@ocservia/api-client";
import { createPinia, setActivePinia } from "pinia";
import { createSSRApp, h, type Component } from "vue";
import { renderToString } from "vue/server-renderer";
import { createMemoryHistory, createRouter } from "vue-router";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { getWorkspace, workspaceContext } from "../src/api/workspace";
import { listNodes } from "../src/api/nodes";
import { agentVersionLabel } from "../src/shared/agent-version";
import { useFleetStore } from "../src/shared/fleet";
import { i18n } from "../src/shared/i18n";
import NodeObservedDetails from "../src/components/nodes/NodeObservedDetails.vue";
import NodesView from "../src/views/NodesView.vue";
import OverviewView from "../src/views/OverviewView.vue";

vi.mock("../src/api/workspace", async (original) => ({
  ...(await original<typeof import("../src/api/workspace")>()),
  getWorkspace: vi.fn().mockResolvedValue({ id: "workspace" }),
  workspaceContext: vi.fn().mockReturnValue({ id: "workspace", generation: 1 }),
}));
vi.mock("../src/api/nodes", () => ({
  listNodes: vi.fn(),
}));

const versionNode = (
  id: string,
  agentVersionState?: NodeObservedState["agentVersionState"],
  agentVersion?: string,
): NodeObservedState => ({
  id,
  name: `node-${id.slice(-1)}`,
  version: 1,
  trustStatus: "active",
  connectionState: "online",
  freshness: "fresh",
  dropped: { security: 0, health: 0, aggregate: 0, raw: 0 },
  sessionCount: 0,
  ...(agentVersion ? { agentVersion } : {}),
  ...(agentVersionState ? { agentVersionState } : {}),
});

describe("fleet agent version intelligence", () => {
  beforeEach(() => {
    setActivePinia(createPinia());
    vi.mocked(getWorkspace).mockReset();
    vi.mocked(listNodes).mockReset();
    vi.mocked(getWorkspace).mockResolvedValue({
      id: "workspace",
      name: "Workspace",
      slug: "workspace",
      version: 1,
    });
    vi.mocked(workspaceContext).mockReturnValue({
      id: "workspace",
      generation: 1,
    });
  });

  afterEach(() => {
    vi.mocked(workspaceContext).mockReturnValue({
      id: "workspace",
      generation: 1,
    });
  });

  it("aggregates server-provided version states without re-classifying", async () => {
    vi.mocked(listNodes).mockResolvedValue({
      items: [
        versionNode("019fc0a4-6d92-765c-a8a1-4af556614cc1", "current", "0.2.0"),
        versionNode(
          "019fc0a4-6d92-765c-a8a1-4af556614cc2",
          "upgrade_available",
          "0.1.1",
        ),
        versionNode("019fc0a4-6d92-765c-a8a1-4af556614cc3", "unknown", "test"),
        versionNode(
          "019fc0a4-6d92-765c-a8a1-4af556614cc4",
          "unsupported",
          "0.0.1",
        ),
        versionNode("019fc0a4-6d92-765c-a8a1-4af556614cc5", "ahead", "0.3.0"),
        versionNode("019fc0a4-6d92-765c-a8a1-4af556614cc6"),
      ],
      page: { hasMore: false },
    });
    const fleet = useFleetStore();
    await fleet.rebuild();

    expect(fleet.agentCurrent).toBe(1);
    expect(fleet.agentUpdateAvailable).toBe(1);
    expect(fleet.agentAhead).toBe(1);
    // unknown, unsupported, and a missing optional state all roll up as unknown
    // so the summary always covers the whole fleet.
    expect(fleet.agentUnknown).toBe(3);
    expect(
      fleet.agentCurrent +
        fleet.agentUpdateAvailable +
        fleet.agentAhead +
        fleet.agentUnknown,
    ).toBe(fleet.nodes.length);
    fleet.$dispose();
  });
});

describe("version intelligence presentation", () => {
  const current = {
    ...versionNode("current", "current", "0.1.0"),
    recommendedAgentVersion: "0.2.0",
  };
  const update = {
    ...versionNode("update", "upgrade_available", "0.1.0"),
    recommendedAgentVersion: "0.2.0",
    osRelease: "Debian 12",
  };
  const ahead = {
    ...versionNode("ahead", "ahead", "0.3.0"),
    recommendedAgentVersion: "0.2.0",
  };
  const nodes = [current, update, ahead, versionNode("unknown")];

  async function render(component: Component, props = {}) {
    const pinia = createPinia();
    setActivePinia(pinia);
    useFleetStore().$patch({ nodes, initialized: true });
    const router = createRouter({
      history: createMemoryHistory(),
      routes: [
        { path: "/", component: {} },
        { path: "/nodes/:nodeId", name: "node-detail", component: {} },
      ],
    });
    await router.push("/");
    const app = createSSRApp({ render: () => h(component, props) });
    app.use(pinia).use(router).use(i18n);
    return renderToString(app);
  }

  it("renders the server's version states, even when versions suggest otherwise", async () => {
    const html = await render(NodesView);
    const badges = html.match(
      /data-testid="agent-version-state"[^>]*>[\s\S]*?<\/span>/g,
    );
    expect(badges).toHaveLength(4);
    for (const label of [
      "Current",
      "Update available",
      "Ahead",
      "No version observation",
    ]) {
      expect(badges).toEqual(
        expect.arrayContaining([expect.stringContaining(label)]),
      );
    }
  });

  it("renders observed and recommended versions and the server state on node detail", async () => {
    const html = await render(NodeObservedDetails, { node: update });
    expect(html).toContain("Update available");
    expect(html).toContain("Recommended Agent version");
    expect(html).toContain("0.1.0");
    expect(html).toContain("0.2.0");
    expect(html).toContain("Debian 12");
  });

  it("renders every version bucket on the overview", async () => {
    const html = await render(OverviewView);
    expect(html).toMatch(/data-testid="overview-agent-versions"[^>]*>1</);
    expect(html).toContain("1 Update available · 1 Ahead · 1 Unknown");
  });
});

it("explains missing observations and recommendations without comparing versions", () => {
  const node = versionNode("node", "unknown", "0.1.0");
  expect(agentVersionLabel(versionNode("node", "unknown"))).toBe(
    "versionNotObserved",
  );
  expect(agentVersionLabel(node)).toBe("recommendationNotConfigured");
  expect(agentVersionLabel({ ...node, recommendedAgentVersion: "0.2.0" })).toBe(
    "versionNotComparable",
  );
  for (const state of [
    "current",
    "ahead",
    "upgrade_available",
    "unsupported",
  ] as const) {
    expect(
      agentVersionLabel({
        ...node,
        recommendedAgentVersion: "0.2.0",
        agentVersionState: state,
      }),
    ).toBe(state);
  }
});
