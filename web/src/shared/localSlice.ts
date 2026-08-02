import {
  PlatformEventFromJSON,
  type Operation,
  type PlatformEvent,
  type SimulationScenario,
} from "@ocservia/api-client";
import { defineStore } from "pinia";
import { computed, onScopeDispose, ref } from "vue";

import { createLocalSimulation, getOperation, listEvents } from "../api/client";

const terminalStates = new Set([
  "succeeded",
  "failed",
  "expired",
  "rolled_back",
  "superseded",
]);

export const useLocalSliceStore = defineStore("local-slice", () => {
  const events = ref<PlatformEvent[]>([]);
  const operation = ref<Operation>();
  const running = ref(false);
  const unavailable = ref(false);
  let source: EventSource | undefined;
  let pollTimer: ReturnType<typeof setTimeout> | undefined;

  const activeNodes = computed(() => {
    const states = new Map<string, boolean>();
    for (const event of events.value) {
      states.set(event.nodeId, event.type !== "disconnected");
    }
    return [...states.values()].filter(Boolean).length;
  });
  const pendingOperations = computed(() =>
    operation.value && !terminalStates.has(operation.value.state) ? 1 : 0,
  );

  async function rebuild(): Promise<void> {
    try {
      const page = await listEvents();
      events.value = page.items.slice(-200);
      unavailable.value = false;
    } catch {
      unavailable.value = true;
    }
  }

  function connect(): void {
    source?.close();
    source = new EventSource("/api/v1/events/stream");
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

  async function run(scenario: SimulationScenario): Promise<void> {
    running.value = true;
    unavailable.value = false;
    try {
      operation.value = await createLocalSimulation(scenario);
      schedulePoll();
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
      if (running.value) schedulePoll();
    } catch {
      unavailable.value = true;
      running.value = false;
    }
  }

  function disconnect(): void {
    source?.close();
    clearTimeout(pollTimer);
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
