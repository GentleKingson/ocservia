import type { NodeObservedState, NodeSession } from "@ocservia/api-client";
import { defineStore } from "pinia";
import { computed, onScopeDispose, ref } from "vue";

import { getNode, listNodes, listNodeSessions } from "../api/client";

export const useFleetStore = defineStore("fleet", () => {
  const nodes = ref<NodeObservedState[]>([]);
  const selected = ref<NodeObservedState>();
  const sessions = ref<NodeSession[]>([]);
  const loading = ref(false);
  const unavailable = ref(false);
  let source: EventSource | undefined;
  let refreshTimer: ReturnType<typeof setTimeout> | undefined;

  const online = computed(
    () =>
      nodes.value.filter((node) => node.connectionState === "online").length,
  );
  const relay = computed(
    () => nodes.value.filter((node) => node.path?.mode === "relay").length,
  );
  const sessionCount = computed(() =>
    nodes.value.reduce((total, node) => total + node.sessionCount, 0),
  );

  async function rebuild(): Promise<void> {
    loading.value = true;
    try {
      const rebuilt: NodeObservedState[] = [];
      let cursor: string | undefined;
      do {
        const page = await listNodes(cursor);
        rebuilt.push(...page.items);
        cursor = page.page.hasMore ? page.page.nextCursor : undefined;
      } while (cursor);
      nodes.value = rebuilt;
      if (selected.value) await select(selected.value.id);
      unavailable.value = false;
    } catch {
      unavailable.value = true;
    } finally {
      loading.value = false;
    }
  }

  async function select(nodeId: string): Promise<void> {
    try {
      const node = await getNode(nodeId);
      const rebuiltSessions: NodeSession[] = [];
      let cursor: string | undefined;
      do {
        const page = await listNodeSessions(nodeId, cursor);
        rebuiltSessions.push(...page.items);
        cursor = page.page.hasMore ? page.page.nextCursor : undefined;
      } while (cursor);
      selected.value = node;
      sessions.value = rebuiltSessions;
      unavailable.value = false;
    } catch {
      unavailable.value = true;
    }
  }

  function connect(): void {
    source?.close();
    source = new EventSource("/api/v1/events/stream");
    source.addEventListener("platform", () => {
      clearTimeout(refreshTimer);
      refreshTimer = setTimeout(() => void rebuild(), 150);
    });
    source.onerror = () => {
      unavailable.value = true;
    };
  }

  function disconnect(): void {
    source?.close();
    clearTimeout(refreshTimer);
  }
  onScopeDispose(disconnect);
  return {
    nodes,
    selected,
    sessions,
    loading,
    unavailable,
    online,
    relay,
    sessionCount,
    rebuild,
    select,
    connect,
    disconnect,
  };
});
