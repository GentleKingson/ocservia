import type { NodeObservedState } from "@/api/models";
import { bench, describe } from "vitest";
import { filterNodes, sortNodes } from "@/features/nodes/node-list";
import { relativeTimestamp } from "@/shared/timestamp";

function makeNodes(count: number): NodeObservedState[] {
  const base = Date.UTC(2026, 10, 19, 12, 0, 0);
  const modes = ["direct", "relay"] as const;
  const freshness = ["fresh", "stale", "never"] as const;
  const out: NodeObservedState[] = [];
  for (let i = 0; i < count; i += 1) {
    out.push({
      id: `01900001-0000-7000-8000-${String(i).padStart(12, "0")}`,
      name: `edge-node-${String(i).padStart(4, "0")}`,
      trustStatus: i % 20 === 0 ? "pending" : "active",
      connectionState: i % 10 === 0 ? "offline" : "online",
      freshness: freshness[i % freshness.length] ?? "fresh",
      lastHeartbeatAt: new Date(base - ((i * 7919) % 3_600_000)).toISOString(),
      path: { mode: modes[i % 2] ?? "direct", rttMs: 12 },
      agentVersionState: i % 5 === 0 ? "upgrade_available" : "current",
      dropped: { security: 0, health: 0, aggregate: 0, raw: 0 },
      sessionCount: i % 120,
      version: 1,
    });
  }
  return out;
}

const fleet1000 = makeNodes(1000);
const filterState = {
  search: "",
  filters: {
    trust: ["active"],
    connection: [],
    freshness: [],
    path: [],
    agent: [],
  },
};
const nowMs = Date.UTC(2026, 10, 19, 12, 5, 0);
const timestamps = fleet1000.map(
  (n) => n.lastHeartbeatAt ?? "2026-11-19T12:00:00.000Z",
);

describe("Web fleet & timestamp hot paths", () => {
  bench("sortNodes heartbeat (1000 nodes)", () => {
    sortNodes(fleet1000, "heartbeat");
  });

  bench("filterNodes + sortNodes heartbeat (1000 nodes)", () => {
    sortNodes(filterNodes(fleet1000, filterState), "heartbeat");
  });

  bench("relativeTimestamp x1000", () => {
    for (const ts of timestamps) {
      relativeTimestamp(ts, nowMs, "en");
    }
  });
});
