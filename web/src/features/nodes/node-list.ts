import type { NodeObservedState } from "@/api/models";
import type { LocationQuery, LocationQueryRaw } from "vue-router";

// Filter buckets only hold values the API defines. Missing optional fields
// fall into "unknown"; owner, tag, IP and region are not node fields.
export const nodeFilterGroups = {
  trust: ["pending", "active", "revoked", "offline"],
  connection: ["online", "offline"],
  freshness: ["fresh", "stale", "never"],
  path: ["direct", "relay", "unknown"],
  agent: ["current", "upgrade_available", "ahead", "unsupported", "unknown"],
} as const;
export type NodeFilterGroup = keyof typeof nodeFilterGroups;
export const nodeFilterGroupNames = Object.keys(
  nodeFilterGroups,
) as NodeFilterGroup[];

export const optionalNodeColumns = ["path", "ocserv", "platform"] as const;
export type OptionalNodeColumn = (typeof optionalNodeColumns)[number];

export const nodeSorts = ["name", "heartbeat"] as const;
export type NodeSort = (typeof nodeSorts)[number];

export interface NodeListState {
  search: string;
  filters: Record<NodeFilterGroup, string[]>;
  sort: NodeSort;
  columns: OptionalNodeColumn[];
}

const agentFilterValues = new Set<string>(nodeFilterGroups.agent);

export function nodeFilterValue(
  node: NodeObservedState,
  group: NodeFilterGroup,
): string {
  switch (group) {
    case "trust":
      return node.trustStatus;
    case "connection":
      return node.connectionState;
    case "freshness":
      return node.freshness;
    case "path":
      return node.path?.mode === "direct" || node.path?.mode === "relay"
        ? node.path.mode
        : "unknown";
    case "agent":
      return agentFilterValues.has(node.agentVersionState ?? "")
        ? (node.agentVersionState as string)
        : "unknown";
  }
}

// Values within a group are alternatives; groups narrow each other.
export function filterNodes(
  nodes: readonly NodeObservedState[],
  state: Pick<NodeListState, "search" | "filters">,
): NodeObservedState[] {
  const search = state.search.trim().toLowerCase();
  const activeFilters: [NodeFilterGroup, Set<string>][] = [];
  for (const group of nodeFilterGroupNames) {
    const values = state.filters[group];
    if (values.length > 0) activeFilters.push([group, new Set(values)]);
  }
  if (!search && activeFilters.length === 0) return [...nodes];
  const matched: NodeObservedState[] = [];
  for (const node of nodes) {
    if (
      search &&
      !node.name.toLowerCase().includes(search) &&
      !node.id.toLowerCase().includes(search)
    ) {
      continue;
    }
    let ok = true;
    for (const [group, allowed] of activeFilters) {
      if (!allowed.has(nodeFilterValue(node, group))) {
        ok = false;
        break;
      }
    }
    if (ok) matched.push(node);
  }
  return matched;
}

export function countFilterValues(
  nodes: readonly NodeObservedState[],
): Record<NodeFilterGroup, Record<string, number>> {
  const counts = Object.fromEntries(
    nodeFilterGroupNames.map((group) => [group, {}]),
  ) as Record<NodeFilterGroup, Record<string, number>>;
  for (const node of nodes)
    for (const group of nodeFilterGroupNames) {
      const value = nodeFilterValue(node, group);
      counts[group][value] = (counts[group][value] ?? 0) + 1;
    }
  return counts;
}

// Missing or invalid timestamps sort last. Precision below milliseconds ties.
export function heartbeatOrder(value: string | undefined): number | undefined {
  if (!value) return undefined;
  const parsed = Date.parse(value);
  return Number.isNaN(parsed) ? undefined : parsed;
}

const nameCollator = new Intl.Collator("en", {
  numeric: true,
  sensitivity: "base",
});

// Name sorts ascending, heartbeat newest first; unknown values sort last and
// the node ID breaks every tie.
export function sortNodes(
  nodes: readonly NodeObservedState[],
  sort: NodeSort,
): NodeObservedState[] {
  if (sort === "heartbeat") {
    const decorated = nodes.map((node) => ({
      node,
      order: heartbeatOrder(node.lastHeartbeatAt),
    }));
    decorated.sort((a, b) => {
      if (a.order !== b.order) {
        if (a.order === undefined) return 1;
        if (b.order === undefined) return -1;
        return a.order > b.order ? -1 : 1;
      }
      return a.node.id < b.node.id ? -1 : a.node.id > b.node.id ? 1 : 0;
    });
    return decorated.map((entry) => entry.node);
  }
  return [...nodes].sort((a, b) => {
    if (a.name !== b.name) {
      if (!a.name) return 1;
      if (!b.name) return -1;
      const order = nameCollator.compare(a.name, b.name);
      if (order !== 0) return order;
    }
    return a.id < b.id ? -1 : a.id > b.id ? 1 : 0;
  });
}

const queryKeys = {
  search: "q",
  sort: "sort",
  columns: "columns",
} as const;

function queryValues(value: LocationQuery[string] | undefined): string[] {
  const values = Array.isArray(value) ? value : [value];
  return values.flatMap((item) => (item ? item.split(",") : []));
}

function known<T extends string>(allowed: readonly T[], values: string[]): T[] {
  return allowed.filter((value) => values.includes(value));
}

export function readNodeListQuery(query: LocationQuery): NodeListState {
  const search = queryValues(query[queryKeys.search]).join(",");
  const sort = queryValues(query[queryKeys.sort])[0];
  return {
    search,
    filters: Object.fromEntries(
      nodeFilterGroupNames.map((group) => [
        group,
        known(nodeFilterGroups[group], queryValues(query[group])),
      ]),
    ) as Record<NodeFilterGroup, string[]>,
    sort: sort === "heartbeat" ? "heartbeat" : "name",
    columns: known(optionalNodeColumns, queryValues(query[queryKeys.columns])),
  };
}

// Writes only this list's keys, dropping defaults, and keeps every other
// query parameter untouched.
export function writeNodeListQuery(
  query: LocationQuery,
  state: NodeListState,
): LocationQueryRaw {
  const ours = new Set<string>([
    ...Object.values(queryKeys),
    ...nodeFilterGroupNames,
  ]);
  const next: LocationQueryRaw = Object.fromEntries(
    Object.entries(query).filter(([key]) => !ours.has(key)),
  );
  const set = (key: string, value: string) => {
    if (value) next[key] = value;
  };
  set(queryKeys.search, state.search);
  for (const group of nodeFilterGroupNames)
    set(group, known(nodeFilterGroups[group], state.filters[group]).join(","));
  set(queryKeys.sort, state.sort === "name" ? "" : state.sort);
  set(queryKeys.columns, known(optionalNodeColumns, state.columns).join(","));
  return next;
}

export function activeFilterCount(state: NodeListState): number {
  return nodeFilterGroupNames.reduce(
    (count, group) => count + state.filters[group].length,
    state.search.trim() ? 1 : 0,
  );
}
