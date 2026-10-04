import type { NodeObservedState } from "@ocservia/api-client";
import { describe, expect, it } from "vitest";

import {
  countFilterValues,
  filterNodes,
  heartbeatOrder,
  readNodeListQuery,
  sortNodes,
  writeNodeListQuery,
  type NodeListState,
} from "../src/features/nodes/node-list";
import { relativeTimestamp } from "../src/shared/timestamp";

function node(
  id: string,
  name: string,
  extra: Partial<NodeObservedState> = {},
): NodeObservedState {
  return {
    id,
    name,
    version: 1,
    trustStatus: "active",
    connectionState: "online",
    freshness: "fresh",
    dropped: { security: 0, health: 0, aggregate: 0, raw: 0 },
    sessionCount: 0,
    ...extra,
  };
}

const fleet = [
  node("id-1", "alpha", {
    path: { mode: "direct", rttMs: 3 },
    agentVersionState: "current",
  }),
  node("id-2", "beta", {
    trustStatus: "pending",
    connectionState: "offline",
    freshness: "stale",
    path: { mode: "relay", rttMs: 40 },
    agentVersionState: "upgrade_available",
  }),
  node("id-3", "gamma", { trustStatus: "revoked", freshness: "never" }),
];

const empty = readNodeListQuery({});
const state = (patch: Partial<NodeListState["filters"]>, search = "") => ({
  search,
  filters: { ...empty.filters, ...patch },
});
const names = (nodes: NodeObservedState[]) => nodes.map((item) => item.name);

describe("filterNodes", () => {
  it("treats values in one group as alternatives", () => {
    expect(
      names(filterNodes(fleet, state({ trust: ["active", "pending"] }))),
    ).toEqual(["alpha", "beta"]);
  });

  it("intersects groups", () => {
    expect(
      names(
        filterNodes(
          fleet,
          state({ trust: ["active", "pending"], connection: ["offline"] }),
        ),
      ),
    ).toEqual(["beta"]);
  });

  it("places missing optional fields in the unknown bucket", () => {
    expect(names(filterNodes(fleet, state({ path: ["unknown"] })))).toEqual([
      "gamma",
    ]);
    expect(names(filterNodes(fleet, state({ agent: ["unknown"] })))).toEqual([
      "gamma",
    ]);
    const counts = countFilterValues(fleet);
    expect(counts.agent).toEqual({
      current: 1,
      upgrade_available: 1,
      unknown: 1,
    });
    expect(counts.freshness).toEqual({ fresh: 1, stale: 1, never: 1 });
  });

  it("searches name and ID case-insensitively with filters", () => {
    expect(names(filterNodes(fleet, state({}, " BET ")))).toEqual(["beta"]);
    expect(names(filterNodes(fleet, state({}, "id-3")))).toEqual(["gamma"]);
    expect(filterNodes(fleet, state({ trust: ["revoked"] }, "alpha"))).toEqual(
      [],
    );
  });
});

describe("sortNodes", () => {
  it("sorts names naturally with an ID tie-break and empty names last", () => {
    const nodes = [
      node("b", "node-10"),
      node("c", ""),
      node("z", "node-2"),
      node("a", "node-2"),
    ];
    expect(sortNodes(nodes, "name").map((item) => item.id)).toEqual([
      "a",
      "z",
      "b",
      "c",
    ]);
  });

  it("sorts heartbeats newest first with unknown values last", () => {
    const nodes = [
      node("e", "missing"),
      node("d", "garbage", { lastHeartbeatAt: "not a time" }),
      node("c", "old", { lastHeartbeatAt: "2026-01-01T00:00:00Z" }),
      node("b", "new", { lastHeartbeatAt: "2026-02-01T00:00:00.123456Z" }),
      node("a", "same", { lastHeartbeatAt: "2026-02-01T00:00:00.123456Z" }),
      node("f", "infinite", { lastHeartbeatAt: "infinity" }),
    ];
    expect(sortNodes(nodes, "heartbeat").map((item) => item.id)).toEqual([
      "f",
      "a",
      "b",
      "c",
      "d",
      "e",
    ]);
  });

  it("orders special timestamps without Date coercion", () => {
    expect(heartbeatOrder("infinity")).toBe(Number.POSITIVE_INFINITY);
    expect(heartbeatOrder("-infinity")).toBe(Number.NEGATIVE_INFINITY);
    expect(heartbeatOrder("+275760-09-13T00:00:00Z")).toBe(Number.MAX_VALUE);
    expect(heartbeatOrder("")).toBeUndefined();
  });
});

describe("node list query", () => {
  it("round-trips state and keeps unrelated parameters", () => {
    const next = writeNodeListQuery(
      { tab: "x", trust: "stale-value" },
      {
        search: "edge",
        filters: { ...empty.filters, trust: ["revoked", "active"] },
        sort: "heartbeat",
        columns: ["platform"],
      },
    );
    expect(next).toEqual({
      tab: "x",
      q: "edge",
      trust: "active,revoked",
      sort: "heartbeat",
      columns: "platform",
    });
    const read = readNodeListQuery(next as Record<string, string>);
    expect(read.filters.trust).toEqual(["active", "revoked"]);
    expect(read.sort).toBe("heartbeat");
    expect(read.columns).toEqual(["platform"]);
  });

  it("drops defaults and unknown values", () => {
    expect(writeNodeListQuery({ q: "old" }, empty)).toEqual({});
    const read = readNodeListQuery({
      trust: ["active", "owner,bogus"],
      sort: "region",
      columns: "ip",
    });
    expect(read.filters.trust).toEqual(["active"]);
    expect(read.sort).toBe("name");
    expect(read.columns).toEqual([]);
  });
});

describe("relativeTimestamp", () => {
  const now = Date.parse("2026-03-01T12:00:00Z");

  it("describes ordinary timestamps relative to now", () => {
    expect(relativeTimestamp("2026-03-01T11:55:00Z", now, "en")).toBe(
      "5 minutes ago",
    );
    expect(relativeTimestamp("2026-02-27T12:00:00Z", now, "en")).toBe(
      "2 days ago",
    );
  });

  it("leaves extended and infinite values to the absolute format", () => {
    expect(relativeTimestamp("infinity", now, "en")).toBeUndefined();
    expect(
      relativeTimestamp("+275760-09-13T00:00:00Z", now, "en"),
    ).toBeUndefined();
  });
});
