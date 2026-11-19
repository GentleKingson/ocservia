import type {
  Operation,
  OperationSummary,
  PlatformEvent,
} from "@ocservia/api-client";
import { defineStore } from "pinia";
import { computed, onScopeDispose, ref, shallowRef, watch } from "vue";
import { createReadRequest } from "./read-request";
import { createForegroundRefresh } from "./foreground-refresh";

import {
  getWorkspace,
  workspaceContext,
  type WorkspaceContext,
} from "../api/workspace";
import { listEvents, platformEventsEvent } from "../api/events";
import { listOperations, operationSummary } from "../api/operations";
import { useFleetStore } from "./fleet";

export const recentOperationWindow = 20;
export const recentEventLimit = 12;
const retainedEventLimit = 50;
const platformRefreshDelay = 500;

export const useOverviewStore = defineStore("overview", () => {
  const fleet = useFleetStore();
  const operations = shallowRef<Operation[]>([]);
  const events = shallowRef<PlatformEvent[]>([]);
  const summary = ref<OperationSummary>({ active: 0, unknown: 0 });
  const operationsLoaded = ref(false);
  const eventsLoaded = ref(false);
  const operationsUnavailable = ref(false);
  const eventsUnavailable = ref(false);
  // Last successful load of each source, kept while a later refresh fails.
  const operationsAt = ref<Date>();
  const eventsAt = ref<Date>();
  const operationsReads = createReadRequest();
  const eventsReads = createReadRequest();
  let started = false;

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
    const request = operationsReads.begin();
    let context: WorkspaceContext | undefined;
    try {
      context = await currentContext();
      const isLatest = () => request.current(context);
      if (!isLatest()) return;
      // Operations are listed newest-first, so a single page bounds every
      // refresh to one request. The page feeds the recent list only; the
      // active/unknown totals come from the workspace summary, which stays
      // accurate when the backlog outgrows the page limit.
      const [page, counts] = await Promise.all([
        listOperations(undefined, request.signal),
        operationSummary(request.signal),
      ]);
      if (!isLatest()) return;
      operations.value = page.items;
      summary.value = counts;
      operationsLoaded.value = true;
      operationsAt.value = new Date();
      operationsUnavailable.value = false;
    } catch {
      if (request.current(context)) operationsUnavailable.value = true;
    } finally {
      request.finish();
    }
  }

  async function loadEvents(): Promise<void> {
    const request = eventsReads.begin();
    const incremental = eventsLoaded.value;
    // The first load reads one newest-first page so the cost stays bounded
    // no matter how much durable history the workspace has. Incremental
    // refreshes resume ascending after the newest retained event.
    const since = incremental ? events.value.at(-1)?.id : undefined;
    let context: WorkspaceContext | undefined;
    try {
      context = await currentContext();
      const isLatest = () => request.current(context);
      if (!isLatest()) return;
      const collected: PlatformEvent[] = [];
      if (!incremental) {
        const page = await listEvents(undefined, request.signal, "desc");
        if (!isLatest()) return;
        collected.push(...[...page.items].reverse());
      } else {
        let cursor = since;
        do {
          const page = await listEvents(cursor, request.signal);
          if (!isLatest()) return;
          collected.push(...page.items);
          cursor = page.page.hasMore ? page.page.nextCursor : undefined;
        } while (cursor);
      }
      if (!isLatest()) return;
      events.value = incremental
        ? [...events.value, ...collected].slice(-retainedEventLimit)
        : collected.slice(-retainedEventLimit);
      eventsLoaded.value = true;
      eventsAt.value = new Date();
      eventsUnavailable.value = false;
    } catch {
      if (request.current(context)) eventsUnavailable.value = true;
    } finally {
      request.finish();
    }
  }

  const foregroundRefresh = createForegroundRefresh(
    async () => {
      await Promise.all([loadOperations(), loadEvents()]);
    },
    () =>
      fleet.streamConnected &&
      !operationsUnavailable.value &&
      !eventsUnavailable.value
        ? 60_000
        : 15_000,
    () => {
      operationsReads.cancel();
      eventsReads.cancel();
    },
  );

  watch(
    () => fleet.streamConnected,
    () => {
      foregroundRefresh.reschedule();
    },
  );

  function schedulePlatformRefresh(): void {
    foregroundRefresh.request(platformRefreshDelay);
  }

  function clearState(): void {
    operationsReads.cancel();
    eventsReads.cancel();
    operations.value = [];
    events.value = [];
    summary.value = { active: 0, unknown: 0 };
    operationsLoaded.value = false;
    eventsLoaded.value = false;
    operationsUnavailable.value = false;
    eventsUnavailable.value = false;
    operationsAt.value = undefined;
    eventsAt.value = undefined;
  }

  function start(): void {
    if (started) return;
    started = true;
    window.addEventListener(platformEventsEvent, schedulePlatformRefresh);
    clearState();
    foregroundRefresh.start();
  }

  function stop(): void {
    if (!started) return;
    started = false;
    foregroundRefresh.stop();
    window.removeEventListener(platformEventsEvent, schedulePlatformRefresh);
    clearState();
  }

  onScopeDispose(stop);

  // Active/unknown totals come from the backend summary so they count the
  // whole workspace, not just the newest operations page.
  const activeOperations = computed(() => summary.value.active);
  const unknownOperations = computed(() => summary.value.unknown);
  const recentFailedOperations = computed(() => {
    const list = operations.value;
    const limit = Math.min(list.length, recentOperationWindow);
    let failed = 0;
    for (let index = 0; index < limit; index += 1) {
      if (list[index]?.state === "failed") failed += 1;
    }
    return failed;
  });
  const recentOperations = computed(() =>
    operations.value.slice(0, recentEventLimit),
  );
  const recentEvents = computed(() =>
    events.value.slice(-recentEventLimit).reverse(),
  );

  return {
    operations,
    events,
    operationsLoaded,
    eventsLoaded,
    operationsUnavailable,
    eventsUnavailable,
    operationsAt,
    eventsAt,
    activeOperations,
    unknownOperations,
    recentFailedOperations,
    recentOperations,
    recentEvents,
    refresh: () => {
      foregroundRefresh.request();
    },
    start,
    stop,
  };
});
