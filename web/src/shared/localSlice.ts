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
  let connectSequence = 0;
  const controllers = new Set<AbortController>();
  let rebuildController: AbortController | undefined;
  let rebuildSequence = 0;
  let pendingController: AbortController | undefined;
  let pendingSequence = 0;
  let pollController: AbortController | undefined;
  let pollSequence = 0;
  let runController: AbortController | undefined;
  let runSequence = 0;

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
    pendingController = undefined;
    pollController = undefined;
    runController = undefined;
    rebuildSequence += 1;
    pendingSequence += 1;
    pollSequence += 1;
    runSequence += 1;
  }

  function beginPendingRequest(): {
    controller: AbortController;
    sequence: number;
  } {
    cancelRequest(pendingController);
    const controller = trackRequest();
    pendingController = controller;
    return { controller, sequence: ++pendingSequence };
  }

  function isLatestPending(
    context: WorkspaceContext,
    controller: AbortController,
    sequence: number,
  ): boolean {
    return (
      pendingController === controller &&
      sequence === pendingSequence &&
      isCurrent(context, controller)
    );
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
    cancelRequest(rebuildController);
    const controller = trackRequest();
    rebuildController = controller;
    const sequence = ++rebuildSequence;
    let pendingRequest:
      { controller: AbortController; sequence: number } | undefined;
    let context: WorkspaceContext | undefined;
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
      const rebuilt: PlatformEvent[] = [];
      let cursor: string | undefined;
      do {
        const page = await listEvents(cursor, controller.signal);
        if (!isLatestRebuild()) return;
        rebuilt.push(...page.items);
        cursor = page.page.hasMore ? page.page.nextCursor : undefined;
      } while (cursor);
      if (!isLatestRebuild()) return;
      const rebuiltNodes = new Set<string>();
      for (const event of rebuilt) updateNodeState(rebuiltNodes, event);
      connectedNodes.value = rebuiltNodes;
      events.value = rebuilt.slice(-200);

      const currentPendingRequest = beginPendingRequest();
      pendingRequest = currentPendingRequest;
      const abortPendingWithRebuild = () => {
        currentPendingRequest.controller.abort();
      };
      controller.signal.addEventListener("abort", abortPendingWithRebuild, {
        once: true,
      });
      let pending = new Set<string>();
      let pendingIsLatest = false;
      try {
        pending = await loadPendingOperationIDs(
          context,
          currentPendingRequest.controller.signal,
        );
        pendingIsLatest = isLatestPending(
          context,
          currentPendingRequest.controller,
          currentPendingRequest.sequence,
        );
      } finally {
        controller.signal.removeEventListener("abort", abortPendingWithRebuild);
        releaseRequest(currentPendingRequest.controller);
        if (pendingController === currentPendingRequest.controller)
          pendingController = undefined;
      }
      if (!isLatestRebuild() || !pendingIsLatest) return;
      pendingOperationIDs.value = pending;
      schedulePendingRefresh();
      unavailable.value = false;
    } catch {
      if (!context) return;
      if (
        pendingRequest &&
        (pendingRequest.controller.signal.aborted ||
          pendingController !== pendingRequest.controller ||
          pendingSequence !== pendingRequest.sequence)
      )
        return;
      const isLatestRebuild =
        rebuildController === controller &&
        sequence === rebuildSequence &&
        isCurrent(context, controller);
      if (!isLatestRebuild) return;
      unavailable.value = true;
    } finally {
      releaseRequest(controller);
      if (rebuildController === controller) rebuildController = undefined;
    }
  }

  async function connect(): Promise<void> {
    const sequence = ++connectSequence;
    source?.close();
    source = undefined;
    let context: WorkspaceContext;
    try {
      context = await currentContext();
      const cursor = events.value.at(-1)?.id;
      const stream = new EventSource(await eventStreamPath(cursor));
      if (sequence !== connectSequence || !isCurrent(context)) {
        stream.close();
        return;
      }
      source = stream;
      stream.addEventListener("platform", (message) => {
        if (
          sequence !== connectSequence ||
          !isCurrent(context) ||
          source !== stream
        ) {
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

  function updateNodeState(nodes: Set<string>, event: PlatformEvent): void {
    if (event.type === "disconnected") nodes.delete(event.nodeId);
    else nodes.add(event.nodeId);
  }

  async function run(scenario: SimulationScenario): Promise<void> {
    cancelRequest(runController);
    cancelRequest(pollController);
    pollController = undefined;
    ++pollSequence;
    clearTimeout(pollTimer);
    const controller = trackRequest();
    runController = controller;
    const sequence = ++runSequence;
    let context: WorkspaceContext | undefined;
    running.value = true;
    unavailable.value = false;
    try {
      context = await currentContext();
      const isLatestRun = () =>
        Boolean(
          context &&
          runController === controller &&
          sequence === runSequence &&
          isCurrent(context, controller),
        );
      if (!isLatestRun()) return;
      const created = await createLocalSimulation(scenario, controller.signal);
      if (!isLatestRun()) return;
      cancelRequest(pendingController);
      pendingController = undefined;
      ++pendingSequence;
      operation.value = created;
      pendingOperationIDs.value.add(operation.value.id);
      schedulePoll();
      schedulePendingRefresh();
    } catch {
      if (!context) return;
      const isLatestRun =
        runController === controller &&
        sequence === runSequence &&
        isCurrent(context, controller);
      if (!isLatestRun) return;
      unavailable.value = true;
      running.value = false;
    } finally {
      releaseRequest(controller);
      if (runController === controller) runController = undefined;
    }
  }

  function schedulePoll(): void {
    clearTimeout(pollTimer);
    pollTimer = setTimeout(() => void pollOperation(), 250);
  }

  async function pollOperation(): Promise<void> {
    if (!operation.value) return;
    cancelRequest(pollController);
    const context = workspaceContext();
    const controller = trackRequest();
    pollController = controller;
    const sequence = ++pollSequence;
    const operationID = operation.value.id;
    const isLatestPoll = () =>
      pollController === controller &&
      sequence === pollSequence &&
      operation.value?.id === operationID &&
      isCurrent(context, controller);
    try {
      const refreshed = await getOperation(operationID, controller.signal);
      if (!isLatestPoll()) return;
      cancelRequest(pendingController);
      pendingController = undefined;
      ++pendingSequence;
      operation.value = refreshed;
      running.value = !terminalStates.has(refreshed.state);
      if (running.value) pendingOperationIDs.value.add(refreshed.id);
      else pendingOperationIDs.value.delete(refreshed.id);
      if (running.value) schedulePoll();
      else schedulePendingRefresh();
    } catch {
      if (!isLatestPoll()) return;
      unavailable.value = true;
      running.value = false;
    } finally {
      releaseRequest(controller);
      if (pollController === controller) pollController = undefined;
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
      if (signal?.aborted || !isCurrent(context)) return pending;
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
    const { controller, sequence } = beginPendingRequest();
    let context: WorkspaceContext | undefined;
    let schedule: boolean | undefined;
    try {
      context = await currentContext();
      if (!isLatestPending(context, controller, sequence)) return;
      const pending = await loadPendingOperationIDs(context, controller.signal);
      if (!isLatestPending(context, controller, sequence)) return;
      pendingOperationIDs.value = pending;
      unavailable.value = false;
    } catch {
      if (!context || !isLatestPending(context, controller, sequence)) return;
      unavailable.value = true;
    } finally {
      schedule =
        context !== undefined && isLatestPending(context, controller, sequence);
      releaseRequest(controller);
      if (pendingController === controller) pendingController = undefined;
    }
    if (schedule) schedulePendingRefresh();
  }

  function disconnect(): void {
    connectSequence += 1;
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
