import { readFile } from "node:fs/promises";
import { resolve } from "node:path";

import type { NodeObservedState } from "@ocservia/api-client";
import { createPinia, setActivePinia } from "pinia";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { getWorkspace, listNodes, workspaceContext } from "../src/api/client";
import { useFleetStore } from "../src/shared/fleet";

vi.mock("../src/api/client", () => ({
  getWorkspace: vi.fn().mockResolvedValue({ id: "workspace" }),
  listNodes: vi.fn(),
  workspaceContext: vi.fn().mockReturnValue({ id: "workspace", generation: 1 }),
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
  it("renders server-derived version badges on the fleet list", async () => {
    const source = await readFile(
      resolve(import.meta.dirname, "../src/views/NodesView.vue"),
      "utf8",
    );
    expect(source).toContain("agentVersionState");
    expect(source).toContain("version-badge");
    expect(source).not.toContain("semver");
  });

  it("shows observed, recommended, and state on node detail", async () => {
    const source = await readFile(
      resolve(import.meta.dirname, "../src/views/NodeDetailView.vue"),
      "utf8",
    );
    expect(source).toContain("recommendedAgentVersion");
    expect(source).toContain('$t("versionState")');
    expect(source).toContain('$t("osRelease")');
    expect(source).not.toContain("semver");
  });

  it("summarizes version states on the overview dashboard", async () => {
    const source = await readFile(
      resolve(import.meta.dirname, "../src/views/OverviewView.vue"),
      "utf8",
    );
    expect(source).toContain('data-testid="overview-agent-versions"');
    expect(source).toContain("agentUpdateAvailable");
    expect(source).toContain("agentAhead");
    expect(source).toContain("agentUnknown");
  });

  it("labels version states through the i18n layer", async () => {
    const source = await readFile(
      resolve(import.meta.dirname, "../src/shared/i18n.ts"),
      "utf8",
    );
    expect(source).toContain('upgrade_available: "Update available"');
    expect(source).toContain('current: "Current"');
    expect(source).toContain('ahead: "Ahead"');
    expect(source).toContain('unsupported: "Unsupported"');
    expect(source).toContain(
      'recommendedAgentVersion: "Recommended Agent version"',
    );
  });
});
