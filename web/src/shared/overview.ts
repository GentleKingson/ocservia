import type { Operation, PlatformEvent } from "@ocservia/api-client";
import { defineStore } from "pinia";
import { computed, ref } from "vue";

import {
  getWorkspace,
  listEvents,
  listOperations,
  platformEventsEvent,
  workspaceContext,
  workspaceChangedEvent,
  type WorkspaceContext,
} from "../api/client";

// Matches the terminal semantics of operation polling in fleet.ts; the
// "unknown" recovery state stays non-terminal and is classified separately.
const terminalStates = new Set([
  "succeeded",
  "failed",
  "expired",
  "rolled_back",
  "drifted",
  "superseded",
]);
const recentOperationWindow = 20;
const recentEventLimit = 12;
const retainedEventLimit = 50;
const platformRefreshDelay = 500;

export const useOverviewStore = defineStore("overview", () => {
  const operations = ref<Operation[]>([]);
  const events = ref<PlatformEvent[]>([]);
  const operationsLoading = ref(false);
  const eventsLoading = ref(false);
  const operationsLoaded = ref(false);
  const eventsLoaded = ref(false);
  const operationsUnavailable = ref(false);
  const eventsUnavailable = ref(false);
  let operationsController: AbortController | undefined;
  let eventsController: AbortController | undefined;
  let operationsSequence = 0;
  let eventsSequence = 0;
  let refreshTimer: ReturnType<typeof setTimeout> | undefined;
  let started = false;

  function isCurrent(
    context: WorkspaceContext,
    controller: AbortController,
    sequence: number,
    current: number,
  ): boolean {
    const now = workspaceContext();
    return (
      !controller.signal.aborted &&
      current === sequence &&
      now.id === context.id &&
      now.generation === context.generation
    );
  }

  async function currentContext(): Promise<WorkspaceContext> {
    // Development authentication can run without a persisted workspace.
    // Keep the generation at its current value so workspace-bound requests
    // still fail closed while the console remains available.
    try {
      await getWorkspace();
    } catch {
      // Workspace-bound listing fails below and surfaces as unavailable.
    }
    return workspaceContext();
  }

  async function loadOperations(): Promise<void> {
    operationsController?.abort();
    const controller = new AbortController();
    operationsController = controller;
    const sequence = ++operationsSequence;
    operationsLoading.value = true;
    let context: WorkspaceContext | undefined;
    try {
      context = await currentContext();
      const isLatest = () =>
        Boolean(
          context &&
          operationsController === controller &&
          isCurrent(context, controller, sequence, operationsSequence),
        );
      if (!isLatest()) return;
      const rebuilt: Operation[] = [];
      let cursor: string | undefined;
      do {
        const page = await listOperations(cursor, controller.signal);
        if (!isLatest()) return;
        rebuilt.push(...page.items);
        cursor = page.page.hasMore ? page.page.nextCursor : undefined;
      } while (cursor);
      if (!isLatest()) return;
      operations.value = rebuilt;
      operationsLoaded.value = true;
      operationsUnavailable.value = false;
    } catch {
      if (!context) return;
      if (
        operationsController === controller &&
        isCurrent(context, controller, sequence, operationsSequence)
      )
        operationsUnavailable.value = true;
    } finally {
      if (operationsController === controller) {
        operationsController = undefined;
        operationsLoading.value = false;
      }
    }
  }

  async function loadEvents(): Promise<void> {
    eventsController?.abort();
    const controller = new AbortController();
    eventsController = controller;
    const sequence = ++eventsSequence;
    eventsLoading.value = true;
    // Events are listed oldest-first; incremental refreshes resume after the
    // newest retained event instead of re-walking the whole history.
    const since = eventsLoaded.value ? events.value.at(-1)?.id : undefined;
    let context: WorkspaceContext | undefined;
    try {
      context = await currentContext();
      const isLatest = () =>
        Boolean(
          context &&
          eventsController === controller &&
          isCurrent(context, controller, sequence, eventsSequence),
        );
      if (!isLatest()) return;
      const appended: PlatformEvent[] = [];
      let cursor = since;
      do {
        const page = await listEvents(cursor, controller.signal);
        if (!isLatest()) return;
        appended.push(...page.items);
        cursor = page.page.hasMore ? page.page.nextCursor : undefined;
      } while (cursor);
      if (!isLatest()) return;
      events.value = since
        ? [...events.value, ...appended].slice(-retainedEventLimit)
        : appended.slice(-retainedEventLimit);
      eventsLoaded.value = true;
      eventsUnavailable.value = false;
    } catch {
      if (!context) return;
      if (
        eventsController === controller &&
        isCurrent(context, controller, sequence, eventsSequence)
      )
        eventsUnavailable.value = true;
    } finally {
      if (eventsController === controller) {
        eventsController = undefined;
        eventsLoading.value = false;
      }
    }
  }

  function refresh(): void {
    void loadOperations();
    void loadEvents();
  }

  function schedulePlatformRefresh(): void {
    clearTimeout(refreshTimer);
    refreshTimer = setTimeout(refresh, platformRefreshDelay);
  }

  function clearState(): void {
    operationsController?.abort();
    eventsController?.abort();
    operationsController = undefined;
    eventsController = undefined;
    operationsSequence += 1;
    eventsSequence += 1;
    operations.value = [];
    events.value = [];
    operationsLoading.value = false;
    eventsLoading.value = false;
    operationsLoaded.value = false;
    eventsLoaded.value = false;
    operationsUnavailable.value = false;
    eventsUnavailable.value = false;
  }

  function resetWorkspace(): void {
    clearState();
    refresh();
  }

  function start(): void {
    if (started) return;
    started = true;
    window.addEventListener(workspaceChangedEvent, resetWorkspace);
    window.addEventListener(platformEventsEvent, schedulePlatformRefresh);
    clearState();
    refresh();
  }

  function stop(): void {
    if (!started) return;
    started = false;
    window.removeEventListener(workspaceChangedEvent, resetWorkspace);
    window.removeEventListener(platformEventsEvent, schedulePlatformRefresh);
    clearTimeout(refreshTimer);
    refreshTimer = undefined;
    clearState();
  }

  const activeOperations = computed(
    () =>
      operations.value.filter(
        (operation) =>
          !terminalStates.has(operation.state) && operation.state !== "unknown",
      ).length,
  );
  const unknownOperations = computed(
    () =>
      operations.value.filter((operation) => operation.state === "unknown")
        .length,
  );
  const recentFailedOperations = computed(
    () =>
      operations.value
        .slice(0, recentOperationWindow)
        .filter((operation) => operation.state === "failed").length,
  );
  const recentOperations = computed(() =>
    operations.value.slice(0, recentEventLimit),
  );
  const recentEvents = computed(() =>
    [...events.value].reverse().slice(0, recentEventLimit),
  );

  return {
    operations,
    events,
    operationsLoading,
    eventsLoading,
    operationsLoaded,
    eventsLoaded,
    operationsUnavailable,
    eventsUnavailable,
    activeOperations,
    unknownOperations,
    recentFailedOperations,
    recentOperations,
    recentEvents,
    refresh,
    start,
    stop,
  };
});
