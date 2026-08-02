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
  getOperation,
  listEvents,
  listOperations,
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

  const activeNodes = computed(() => connectedNodes.value.size);
  const pendingOperations = computed(() => pendingOperationIDs.value.size);

  async function rebuild(): Promise<void> {
    try {
      const rebuilt: PlatformEvent[] = [];
      let cursor: string | undefined;
      do {
        const page = await listEvents(cursor);
        rebuilt.push(...page.items);
        cursor = page.page.hasMore ? page.page.nextCursor : undefined;
      } while (cursor);
      const rebuiltNodes = new Set<string>();
      for (const event of rebuilt) updateNodeState(rebuiltNodes, event);
      connectedNodes.value = rebuiltNodes;
      events.value = rebuilt.slice(-200);

      pendingOperationIDs.value = await loadPendingOperationIDs();
      schedulePendingRefresh();
      unavailable.value = false;
    } catch {
      unavailable.value = true;
    }
  }

  function connect(): void {
    source?.close();
    const cursor = events.value.at(-1)?.id;
    source = new EventSource(
      `/api/v1/events/stream${cursor ? `?after=${encodeURIComponent(cursor)}` : ""}`,
    );
    source.addEventListener("platform", (message) => {
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
        unavailable.value = false;
      } catch {
        unavailable.value = true;
      }
    });
    source.onerror = () => {
      unavailable.value = true;
    };
  }

  function updateNodeState(nodes: Set<string>, event: PlatformEvent): void {
    if (event.type === "disconnected") nodes.delete(event.nodeId);
    else nodes.add(event.nodeId);
  }

  async function run(scenario: SimulationScenario): Promise<void> {
    running.value = true;
    unavailable.value = false;
    try {
      operation.value = await createLocalSimulation(scenario);
      pendingOperationIDs.value.add(operation.value.id);
      schedulePoll();
      schedulePendingRefresh();
    } catch {
      unavailable.value = true;
      running.value = false;
    }
  }

  function schedulePoll(): void {
    clearTimeout(pollTimer);
    pollTimer = setTimeout(() => void pollOperation(), 250);
  }

  async function pollOperation(): Promise<void> {
    if (!operation.value) return;
    try {
      operation.value = await getOperation(operation.value.id);
      running.value = !terminalStates.has(operation.value.state);
      if (running.value) pendingOperationIDs.value.add(operation.value.id);
      else pendingOperationIDs.value.delete(operation.value.id);
      if (running.value) schedulePoll();
    } catch {
      unavailable.value = true;
      running.value = false;
    }
  }

  async function loadPendingOperationIDs(): Promise<Set<string>> {
    const pending = new Set<string>();
    let cursor: string | undefined;
    do {
      const page = await listOperations(cursor);
      for (const current of page.items) {
        if (!terminalStates.has(current.state)) pending.add(current.id);
      }
      cursor = page.page.hasMore ? page.page.nextCursor : undefined;
    } while (cursor);
    return pending;
  }

  function schedulePendingRefresh(): void {
    clearTimeout(pendingPollTimer);
    if (pendingOperationIDs.value.size === 0) return;
    pendingPollTimer = setTimeout(() => void refreshPendingOperations(), 1000);
  }

  async function refreshPendingOperations(): Promise<void> {
    try {
      pendingOperationIDs.value = await loadPendingOperationIDs();
      unavailable.value = false;
    } catch {
      unavailable.value = true;
    }
    schedulePendingRefresh();
  }

  function disconnect(): void {
    source?.close();
    clearTimeout(pollTimer);
    clearTimeout(pendingPollTimer);
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
