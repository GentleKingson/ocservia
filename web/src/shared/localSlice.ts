import {
  PlatformEventFromJSON,
  type Operation,
  type PlatformEvent,
  type SimulationScenario,
} from "@ocservia/api-client";
import { defineStore } from "pinia";
import { computed, onScopeDispose, ref } from "vue";

import {
  createLocalSimulation,
  eventStreamPath,
  getWorkspace,
  getOperation,
  listEvents,
  listOperations,
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
  "superseded",
]);

export const useLocalSliceStore = defineStore("local-slice", () => {
  const events = ref<PlatformEvent[]>([]);
  const connectedNodes = ref(new Set<string>());
  const pendingOperationIDs = ref(new Set<string>());
  const operation = ref<Operation>();
  const running = ref(false);
  const unavailable = ref(false);
  let source: EventSource | undefined;
  let pollTimer: ReturnType<typeof setTimeout> | undefined;
  let pendingPollTimer: ReturnType<typeof setTimeout> | undefined;
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
    // still fail closed while the local simulator remains usable.
    try {
      await getWorkspace();
    } catch {
      // eventStreamPath preserves the development-token fallback below.
    }
    return workspaceContext();
  }

  const activeNodes = computed(() => connectedNodes.value.size);
  const pendingOperations = computed(() => pendingOperationIDs.value.size);

  async function rebuild(): Promise<void> {
    const controller = trackRequest();
    let context: WorkspaceContext | undefined;
    try {
      context = await currentContext();
      const rebuilt: PlatformEvent[] = [];
      let cursor: string | undefined;
      do {
        const page = await listEvents(cursor, controller.signal);
        if (!isCurrent(context, controller)) return;
        rebuilt.push(...page.items);
        cursor = page.page.hasMore ? page.page.nextCursor : undefined;
      } while (cursor);
      if (!isCurrent(context, controller)) return;
      const rebuiltNodes = new Set<string>();
      for (const event of rebuilt) updateNodeState(rebuiltNodes, event);
      connectedNodes.value = rebuiltNodes;
      events.value = rebuilt.slice(-200);

      const pending = await loadPendingOperationIDs(context, controller.signal);
      if (!isCurrent(context, controller)) return;
      pendingOperationIDs.value = pending;
      schedulePendingRefresh();
      unavailable.value = false;
    } catch {
      if (!context || !isCurrent(context, controller)) return;
      unavailable.value = true;
    } finally {
      releaseRequest(controller);
    }
  }

  async function connect(): Promise<void> {
    source?.close();
    let context: WorkspaceContext;
    try {
      context = await currentContext();
      const cursor = events.value.at(-1)?.id;
      const stream = new EventSource(await eventStreamPath(cursor));
      if (!isCurrent(context)) {
        stream.close();
        return;
      }
      source = stream;
      stream.addEventListener("platform", (message) => {
        if (!isCurrent(context) || source !== stream) {
          stream.close();
          return;
        }
        if (
          !(message instanceof MessageEvent) ||
          typeof message.data !== "string"
        ) {
          return;
        }
        try {
          const event = PlatformEventFromJSON(
            JSON.parse(message.data) as unknown,
          );
          if (!events.value.some((current) => current.id === event.id)) {
            updateNodeState(connectedNodes.value, event);
            events.value = [...events.value.slice(-199), event];
          }
          if (isCurrent(context) && source === stream)
            unavailable.value = false;
        } catch {
          unavailable.value = true;
        }
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

  function updateNodeState(nodes: Set<string>, event: PlatformEvent): void {
    if (event.type === "disconnected") nodes.delete(event.nodeId);
    else nodes.add(event.nodeId);
  }

  async function run(scenario: SimulationScenario): Promise<void> {
    const controller = trackRequest();
    let context: WorkspaceContext | undefined;
    running.value = true;
    unavailable.value = false;
    try {
      context = await currentContext();
      const created = await createLocalSimulation(scenario, controller.signal);
      if (!isCurrent(context, controller)) return;
      operation.value = created;
      pendingOperationIDs.value.add(operation.value.id);
      schedulePoll();
      schedulePendingRefresh();
    } catch {
      if (!context || !isCurrent(context, controller)) return;
      unavailable.value = true;
      running.value = false;
    } finally {
      releaseRequest(controller);
    }
  }

  function schedulePoll(): void {
    clearTimeout(pollTimer);
    pollTimer = setTimeout(() => void pollOperation(), 250);
  }

  async function pollOperation(): Promise<void> {
    if (!operation.value) return;
    const context = workspaceContext();
    const controller = trackRequest();
    try {
      const operationID = operation.value.id;
      const refreshed = await getOperation(operationID, controller.signal);
      if (!isCurrent(context, controller)) return;
      operation.value = refreshed;
      running.value = !terminalStates.has(refreshed.state);
      if (running.value) pendingOperationIDs.value.add(refreshed.id);
      else pendingOperationIDs.value.delete(refreshed.id);
      if (running.value) schedulePoll();
    } catch {
      if (!isCurrent(context, controller)) return;
      unavailable.value = true;
      running.value = false;
    } finally {
      releaseRequest(controller);
    }
  }

  async function loadPendingOperationIDs(
    context: WorkspaceContext,
    signal?: AbortSignal,
  ): Promise<Set<string>> {
    const pending = new Set<string>();
    let cursor: string | undefined;
    do {
      const page = await listOperations(cursor, signal);
      if (!isCurrent(context)) return pending;
      for (const current of page.items) {
        if (!terminalStates.has(current.state)) pending.add(current.id);
      }
      cursor = page.page.hasMore ? page.page.nextCursor : undefined;
    } while (cursor);
    return pending;
  }

  function schedulePendingRefresh(): void {
    clearTimeout(pendingPollTimer);
    const delay = pendingOperationIDs.value.size === 0 ? 5000 : 1000;
    pendingPollTimer = setTimeout(() => void refreshPendingOperations(), delay);
  }

  async function refreshPendingOperations(): Promise<void> {
    const controller = trackRequest();
    let context: WorkspaceContext | undefined;
    try {
      context = await currentContext();
      const pending = await loadPendingOperationIDs(context, controller.signal);
      if (!isCurrent(context, controller)) return;
      pendingOperationIDs.value = pending;
      unavailable.value = false;
    } catch {
      if (!context || !isCurrent(context, controller)) return;
      unavailable.value = true;
    } finally {
      releaseRequest(controller);
    }
    if (isCurrent(context)) schedulePendingRefresh();
  }

  function disconnect(): void {
    source?.close();
    source = undefined;
    clearTimeout(pollTimer);
    clearTimeout(pendingPollTimer);
    abortRequests();
  }

  function resetWorkspace(): void {
    disconnect();
    events.value = [];
    connectedNodes.value = new Set();
    pendingOperationIDs.value = new Set();
    operation.value = undefined;
    running.value = false;
    unavailable.value = false;
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
    activeNodes,
    events,
    operation,
    pendingOperations,
    running,
    unavailable,
    rebuild,
    connect,
    run,
    disconnect,
  };
});
