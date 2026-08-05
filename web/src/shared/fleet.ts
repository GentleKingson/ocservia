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
  let connectSequence = 0;
  const controllers = new Set<AbortController>();
  let rebuildController: AbortController | undefined;
  let rebuildSequence = 0;
  let selectController: AbortController | undefined;
  let selectSequence = 0;
  let operationController: AbortController | undefined;
  let operationSequence = 0;

  function trackRequest(): AbortController {
    const controller = new AbortController();
    controllers.add(controller);
    return controller;
  }

  function releaseRequest(controller: AbortController): void {
    controllers.delete(controller);
  }

  function cancelRequest(controller: AbortController | undefined): void {
    if (!controller) return;
    controller.abort();
    controllers.delete(controller);
  }

  function abortRequests(): void {
    for (const controller of controllers) controller.abort();
    controllers.clear();
    rebuildController = undefined;
    selectController = undefined;
    operationController = undefined;
    rebuildSequence += 1;
    selectSequence += 1;
    operationSequence += 1;
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
    cancelRequest(rebuildController);
    const controller = trackRequest();
    rebuildController = controller;
    const sequence = ++rebuildSequence;
    const selectSequenceAtStart = selectSequence;
    let context: WorkspaceContext | undefined;
    loading.value = true;
    try {
      context = await currentContext();
      const isLatestRebuild = () =>
        Boolean(
          context &&
          rebuildController === controller &&
          sequence === rebuildSequence &&
          isCurrent(context, controller),
        );
      if (!isLatestRebuild()) return;
      const rebuilt: NodeObservedState[] = [];
      let cursor: string | undefined;
      do {
        const page = await listNodes(cursor, controller.signal);
        if (!isLatestRebuild()) return;
        rebuilt.push(...page.items);
        cursor = page.page.hasMore ? page.page.nextCursor : undefined;
      } while (cursor);
      if (!isLatestRebuild()) return;
      nodes.value = rebuilt;
      if (selected.value && selectSequence === selectSequenceAtStart)
        await select(selected.value.id);
      if (!isLatestRebuild()) return;
      unavailable.value = false;
    } catch {
      if (!context) return;
      const isLatestRebuild =
        rebuildController === controller &&
        sequence === rebuildSequence &&
        isCurrent(context, controller);
      if (!isLatestRebuild) return;
      unavailable.value = true;
    } finally {
      releaseRequest(controller);
      if (rebuildController === controller) {
        rebuildController = undefined;
        loading.value = false;
      }
    }
  }

  async function select(nodeId: string): Promise<void> {
    cancelRequest(selectController);
    const controller = trackRequest();
    selectController = controller;
    const sequence = ++selectSequence;
    let context: WorkspaceContext | undefined;
    try {
      context = await currentContext();
      const isLatestSelect = () =>
        Boolean(
          context &&
          selectController === controller &&
          sequence === selectSequence &&
          isCurrent(context, controller),
        );
      if (!isLatestSelect()) return;
      const node = await getNode(nodeId, controller.signal);
      if (!isLatestSelect()) return;
      const rebuiltSessions: NodeSession[] = [];
      let cursor: string | undefined;
      do {
        const page = await listNodeSessions(nodeId, cursor, controller.signal);
        if (!isLatestSelect()) return;
        rebuiltSessions.push(...page.items);
        cursor = page.page.hasMore ? page.page.nextCursor : undefined;
      } while (cursor);
      if (!isLatestSelect()) return;
      const rebuiltIpBans = (await listNodeIpBans(nodeId, controller.signal))
        .items;
      if (!isLatestSelect()) return;
      selected.value = node;
      sessions.value = rebuiltSessions;
      ipBans.value = rebuiltIpBans;
      unavailable.value = false;
    } catch {
      if (!context) return;
      const isLatestSelect =
        selectController === controller &&
        sequence === selectSequence &&
        isCurrent(context, controller);
      if (!isLatestSelect) return;
      unavailable.value = true;
    } finally {
      releaseRequest(controller);
      if (selectController === controller) selectController = undefined;
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
    cancelRequest(operationController);
    const context = workspaceContext();
    const controller = trackRequest();
    operationController = controller;
    const sequence = ++operationSequence;
    const selectSequenceAtStart = selectSequence;
    const isLatestOperation = () =>
      operationController === controller &&
      sequence === operationSequence &&
      isCurrent(context, controller);
    const node = selected.value;
    operationError.value = "";
    try {
      let currentOperation = await create(node, controller.signal);
      if (!isLatestOperation()) return;
      latestOperation.value = currentOperation;
      while (!terminalStates.has(currentOperation.state)) {
        await new Promise((resolve) => setTimeout(resolve, 750));
        if (!isLatestOperation()) return;
        currentOperation = await getOperation(
          currentOperation.id,
          controller.signal,
        );
        if (!isLatestOperation()) return;
        latestOperation.value = currentOperation;
      }
      if (
        isLatestOperation() &&
        currentOperation.state === "succeeded" &&
        selectSequence === selectSequenceAtStart &&
        selected.value.id === node.id
      ) {
        await select(node.id);
      }
    } catch (error) {
      if (!isLatestOperation() || controller.signal.aborted) return;
      operationError.value =
        error instanceof Error ? error.message : "Operation failed";
    } finally {
      releaseRequest(controller);
      if (operationController === controller) operationController = undefined;
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
    const sequence = ++connectSequence;
    source?.close();
    source = undefined;
    let context: WorkspaceContext;
    try {
      context = await currentContext();
      const stream = new EventSource(await eventStreamPath());
      if (sequence !== connectSequence || !isCurrent(context)) {
        stream.close();
        return;
      }
      source = stream;
      stream.addEventListener("platform", () => {
        if (
          sequence !== connectSequence ||
          !isCurrent(context) ||
          source !== stream
        ) {
          stream.close();
          return;
        }
        clearTimeout(refreshTimer);
        refreshTimer = setTimeout(() => void rebuild(), 150);
      });
      stream.onerror = () => {
        if (
          sequence !== connectSequence ||
          !isCurrent(context) ||
          source !== stream
        ) {
          stream.close();
          return;
        }
        unavailable.value = true;
        void probeAuthentication().catch(() => undefined);
      };
    } catch {
      if (sequence === connectSequence) unavailable.value = true;
    }
  }

  function disconnect(): void {
    connectSequence += 1;
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
