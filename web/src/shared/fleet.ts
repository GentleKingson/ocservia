import type {
  NodeIpBan,
  NodeObservedState,
  NodeSession,
  Operation,
} from "@ocservia/api-client";
import { defineStore } from "pinia";
import { computed, onScopeDispose, ref } from "vue";

import {
  disconnectSession,
  getNode,
  getOperation,
  eventStreamPath,
  listNodeIpBans,
  listNodes,
  listNodeSessions,
  reloadService,
  removeIpBan,
  terminateSession,
  workspaceChangedEvent,
} from "../api/client";

const terminalStates = new Set([
  "succeeded",
  "failed",
  "unknown",
  "expired",
  "rolled_back",
  "drifted",
  "superseded",
]);

export const useFleetStore = defineStore("fleet", () => {
  const nodes = ref<NodeObservedState[]>([]);
  const selected = ref<NodeObservedState>();
  const sessions = ref<NodeSession[]>([]);
  const ipBans = ref<NodeIpBan[]>([]);
  const latestOperation = ref<Operation>();
  const operationError = ref("");
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
      ipBans.value = (await listNodeIpBans(nodeId)).items;
      unavailable.value = false;
    } catch {
      unavailable.value = true;
    }
  }

  async function runOperation(
    create: (node: NodeObservedState) => Promise<Operation>,
  ): Promise<void> {
    if (
      !selected.value ||
      (latestOperation.value &&
        !terminalStates.has(latestOperation.value.state))
    )
      return;
    operationError.value = "";
    try {
      latestOperation.value = await create(selected.value);
      while (!terminalStates.has(latestOperation.value.state)) {
        await new Promise((resolve) => setTimeout(resolve, 750));
        latestOperation.value = await getOperation(latestOperation.value.id);
      }
      if (latestOperation.value.state === "succeeded") {
        await select(selected.value.id);
      }
    } catch (error) {
      operationError.value =
        error instanceof Error ? error.message : "Operation failed";
    }
  }

  async function disconnectSessionAction(
    sessionId: string,
    reason: string,
  ): Promise<void> {
    await runOperation((node) => disconnectSession(node, sessionId, reason));
  }

  async function terminate(sessionId: string, reason: string): Promise<void> {
    await runOperation((node) => terminateSession(node, sessionId, reason));
  }

  async function unban(ip: string, reason: string): Promise<void> {
    await runOperation((node) => removeIpBan(node, ip, reason));
  }

  async function reload(reason: string, approvalId: string): Promise<void> {
    await runOperation((node) => reloadService(node, reason, approvalId));
  }

  async function connect(): Promise<void> {
    source?.close();
    source = new EventSource(await eventStreamPath());
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
    source = undefined;
    clearTimeout(refreshTimer);
  }
  function resetWorkspace(): void {
    disconnect();
    nodes.value = [];
    selected.value = undefined;
    sessions.value = [];
    ipBans.value = [];
    latestOperation.value = undefined;
    operationError.value = "";
    void rebuild().then(() => connect());
  }
  if (typeof window !== "undefined") {
    window.addEventListener(workspaceChangedEvent, resetWorkspace);
    onScopeDispose(() => {
      window.removeEventListener(workspaceChangedEvent, resetWorkspace);
    });
  }
  onScopeDispose(disconnect);
  return {
    nodes,
    selected,
    sessions,
    ipBans,
    latestOperation,
    operationError,
    loading,
    unavailable,
    online,
    relay,
    sessionCount,
    rebuild,
    select,
    connect,
    disconnect,
    disconnectSession: disconnectSessionAction,
    terminateSession: terminate,
    removeIpBan: unban,
    reloadService: reload,
  };
});
