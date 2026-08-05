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
  getWorkspace,
  getNode,
  getOperation,
  eventStreamPath,
  listNodeIpBans,
  listNodes,
  listNodeSessions,
  reloadService,
  removeIpBan,
  terminateSession,
  probeAuthentication,
  workspaceContext,
  workspaceChangedEvent,
  type WorkspaceContext,
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
  const controllers = new Set<AbortController>();

  function trackRequest(): AbortController {
    const controller = new AbortController();
    controllers.add(controller);
    return controller;
  }

  function releaseRequest(controller: AbortController): void {
    controllers.delete(controller);
  }

  function abortRequests(): void {
    for (const controller of controllers) controller.abort();
    controllers.clear();
  }

  function isCurrent(
    context: WorkspaceContext,
    controller?: AbortController,
  ): boolean {
    const current = workspaceContext();
    return (
      !controller?.signal.aborted &&
      current.id === context.id &&
      current.generation === context.generation
    );
  }

  async function currentContext(): Promise<WorkspaceContext> {
    // Development authentication can run without a persisted workspace.
    // Keep the generation at its current value so workspace-bound requests
    // still fail closed while the console remains available.
    try {
      await getWorkspace();
    } catch {
      // eventStreamPath preserves the development-token fallback below.
    }
    return workspaceContext();
  }

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
    const controller = trackRequest();
    let context: WorkspaceContext | undefined;
    loading.value = true;
    try {
      context = await currentContext();
      const rebuilt: NodeObservedState[] = [];
      let cursor: string | undefined;
      do {
        const page = await listNodes(cursor, controller.signal);
        if (!isCurrent(context, controller)) return;
        rebuilt.push(...page.items);
        cursor = page.page.hasMore ? page.page.nextCursor : undefined;
      } while (cursor);
      if (!isCurrent(context, controller)) return;
      nodes.value = rebuilt;
      if (selected.value) await select(selected.value.id);
      if (!isCurrent(context, controller)) return;
      unavailable.value = false;
    } catch {
      if (!context || !isCurrent(context, controller)) return;
      unavailable.value = true;
    } finally {
      releaseRequest(controller);
      if (context && isCurrent(context)) loading.value = false;
    }
  }

  async function select(nodeId: string): Promise<void> {
    const controller = trackRequest();
    let context: WorkspaceContext | undefined;
    try {
      context = await currentContext();
      const node = await getNode(nodeId, controller.signal);
      if (!isCurrent(context, controller)) return;
      const rebuiltSessions: NodeSession[] = [];
      let cursor: string | undefined;
      do {
        const page = await listNodeSessions(nodeId, cursor, controller.signal);
        if (!isCurrent(context, controller)) return;
        rebuiltSessions.push(...page.items);
        cursor = page.page.hasMore ? page.page.nextCursor : undefined;
      } while (cursor);
      if (!isCurrent(context, controller)) return;
      const rebuiltIpBans = (await listNodeIpBans(nodeId, controller.signal))
        .items;
      if (!isCurrent(context, controller)) return;
      selected.value = node;
      sessions.value = rebuiltSessions;
      ipBans.value = rebuiltIpBans;
      unavailable.value = false;
    } catch {
      if (!context || !isCurrent(context, controller)) return;
      unavailable.value = true;
    } finally {
      releaseRequest(controller);
    }
  }

  async function runOperation(
    create: (
      node: NodeObservedState,
      signal: AbortSignal,
    ) => Promise<Operation>,
  ): Promise<void> {
    if (
      !selected.value ||
      (latestOperation.value &&
        !terminalStates.has(latestOperation.value.state))
    )
      return;
    const context = workspaceContext();
    const controller = trackRequest();
    const node = selected.value;
    operationError.value = "";
    try {
      let currentOperation = await create(node, controller.signal);
      if (!isCurrent(context, controller)) return;
      latestOperation.value = currentOperation;
      while (!terminalStates.has(currentOperation.state)) {
        await new Promise((resolve) => setTimeout(resolve, 750));
        if (!isCurrent(context, controller)) return;
        currentOperation = await getOperation(
          currentOperation.id,
          controller.signal,
        );
        if (!isCurrent(context, controller)) return;
        latestOperation.value = currentOperation;
      }
      if (
        isCurrent(context, controller) &&
        currentOperation.state === "succeeded" &&
        selected.value.id === node.id
      ) {
        await select(node.id);
      }
    } catch (error) {
      if (!isCurrent(context, controller) || controller.signal.aborted) return;
      operationError.value =
        error instanceof Error ? error.message : "Operation failed";
    } finally {
      releaseRequest(controller);
    }
  }

  async function disconnectSessionAction(
    sessionId: string,
    reason: string,
  ): Promise<void> {
    await runOperation((node, signal) =>
      disconnectSession(node, sessionId, reason, signal),
    );
  }

  async function terminate(sessionId: string, reason: string): Promise<void> {
    await runOperation((node, signal) =>
      terminateSession(node, sessionId, reason, signal),
    );
  }

  async function unban(ip: string, reason: string): Promise<void> {
    await runOperation((node, signal) => removeIpBan(node, ip, reason, signal));
  }

  async function reload(reason: string, approvalId: string): Promise<void> {
    await runOperation((node, signal) =>
      reloadService(node, reason, approvalId, signal),
    );
  }

  async function connect(): Promise<void> {
    source?.close();
    let context: WorkspaceContext;
    try {
      context = await currentContext();
      const stream = new EventSource(await eventStreamPath());
      if (!isCurrent(context)) {
        stream.close();
        return;
      }
      source = stream;
      stream.addEventListener("platform", () => {
        if (!isCurrent(context) || source !== stream) {
          stream.close();
          return;
        }
        clearTimeout(refreshTimer);
        refreshTimer = setTimeout(() => void rebuild(), 150);
      });
      stream.onerror = () => {
        if (!isCurrent(context) || source !== stream) {
          stream.close();
          return;
        }
        unavailable.value = true;
        void probeAuthentication().catch(() => undefined);
      };
    } catch {
      unavailable.value = true;
    }
  }

  function disconnect(): void {
    source?.close();
    source = undefined;
    clearTimeout(refreshTimer);
    abortRequests();
  }
  function resetWorkspace(): void {
    disconnect();
    nodes.value = [];
    selected.value = undefined;
    sessions.value = [];
    ipBans.value = [];
    latestOperation.value = undefined;
    operationError.value = "";
    loading.value = false;
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
