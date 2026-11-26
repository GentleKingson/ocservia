import { ResponseError } from "@/api/transport";
import type {
  NodeIpBan,
  NodeObservedState,
  NodeSession,
  Operation,
  UserGroupResourceState,
} from "@/api/models";
import { defineStore } from "pinia";
import { computed, onScopeDispose, ref, shallowRef } from "vue";

import {
  disconnectSession,
  getOperation,
  reloadService,
  removeIpBan,
  terminateSession,
} from "../api/operations";
import {
  getNode,
  listNodeIpBans,
  listNodes,
  listNodeSessions,
  listNodeUserGroupState,
} from "../api/nodes";
import {
  eventStreamPath,
  listEvents,
  platformEventsEvent,
} from "../api/events";
import { probeAuthentication } from "../api/platform";
import { upgradeNodeAgent } from "../api/agents";
import {
  createUser,
  disableUser,
  enableUser,
  rotateUserPassword,
  applyGroup,
} from "../api/users";

import { createReadRequest } from "./read-request";
import { abortableDelay } from "./abortable-delay";
import { createForegroundRefresh, isPageActive } from "./foreground-refresh";

const terminalStates = new Set([
  "succeeded",
  "failed",
  "expired",
  "rolled_back",
  "drifted",
  "superseded",
]);
// A reconciled agent upgrade ends in one of the family outcomes; unknown is
// terminal there because no retry of the same upgrade may be inferred.
const upgradeTerminalStates = new Set([...terminalStates, "unknown"]);
const recoveryPollDelays = [1_500, 3_000, 5_000] as const;
const fleetEventRefreshDelay = 5_000;

function pollDelay(state: Operation["state"], recoveryAttempt: number): number {
  if (state === "offline_pending")
    return Math.min(5_000 * 2 ** Math.min(recoveryAttempt, 4), 60_000);
  if (state !== "unknown") return 750;
  return (
    recoveryPollDelays[
      Math.min(recoveryAttempt, recoveryPollDelays.length - 1)
    ] ?? 5_000
  );
}

async function waitForVisiblePage(signal: AbortSignal): Promise<void> {
  signal.throwIfAborted();
  if (typeof document === "undefined" || document.visibilityState !== "hidden")
    return;
  const page = document;
  await new Promise<void>((resolve, reject) => {
    function cleanup() {
      page.removeEventListener("visibilitychange", onVisibility);
      signal.removeEventListener("abort", onAbort);
    }
    function onVisibility() {
      if (page.visibilityState === "hidden") return;
      cleanup();
      resolve();
    }
    function onAbort() {
      cleanup();
      reject(new DOMException("Operation polling aborted", "AbortError"));
    }
    page.addEventListener("visibilitychange", onVisibility);
    signal.addEventListener("abort", onAbort, { once: true });
  });
}

export type DetailReadState = "loading" | "ready" | "unavailable" | "stale";

function createDetailSection<T>(empty: T, onSettled: () => void) {
  const data = shallowRef<T>(empty);
  const state = ref<DetailReadState>("loading");
  const reads = createReadRequest();

  async function load(
    read: (signal: AbortSignal) => Promise<T>,
  ): Promise<void> {
    if (reads.pending) return;
    const request = reads.begin();
    try {
      const result = await read(request.signal);
      if (!request.current()) return;
      data.value = result;
      state.value = "ready";
    } catch {
      if (!request.current()) return;
      state.value =
        state.value === "ready" || state.value === "stale"
          ? "stale"
          : "unavailable";
    } finally {
      if (request.finish()) onSettled();
    }
  }

  function reset(): void {
    reads.cancel();
    data.value = empty;
    state.value = "loading";
  }

  return { data, state, load, reset, cancel: reads.cancel };
}

async function collectAllPages<T>(
  readPage: (cursor: string | undefined) => Promise<{
    items: T[];
    page: { has_more: boolean; next_cursor?: string };
  }>,
  canContinue: () => boolean,
): Promise<T[] | undefined> {
  const items: T[] = [];
  let cursor: string | undefined;
  do {
    const page = await readPage(cursor);
    if (!canContinue()) return undefined;
    items.push(...page.items);
    cursor = page.page.has_more ? page.page.next_cursor : undefined;
  } while (cursor);
  return items;
}

export const useFleetStore = defineStore("fleet", () => {
  const nodes = shallowRef<NodeObservedState[]>([]);
  const selected = shallowRef<NodeObservedState>();
  function rescheduleDetail(): void {
    foregroundRefresh.reschedule();
  }
  const sessionSection = createDetailSection<NodeSession[]>(
    [],
    rescheduleDetail,
  );
  const ipBanSection = createDetailSection<NodeIpBan[]>([], rescheduleDetail);
  const userGroupSection = createDetailSection<UserGroupResourceState[]>(
    [],
    rescheduleDetail,
  );
  const detailSections = [sessionSection, ipBanSection, userGroupSection];
  const latestOperation = ref<Operation>();
  const operationNodeId = ref<string>();
  const operationTracking = ref(false);
  const operationError = ref<string>();
  const loading = ref(false);
  const initialized = ref(false);
  const selecting = ref(false);
  const selectionError = ref<"" | "notFound" | "unavailable">("");
  const unavailable = ref(false);
  // When the complete node snapshot was last replaced, for freshness labels.
  const snapshotAt = ref<Date>();
  let source: EventSource | undefined;
  // Platform event the node snapshot already covers. The stream resumes after
  // it; without a cursor the server replays the whole workspace history and
  // every replayed page triggers another rebuild.
  let eventCursor: string | undefined;
  const streamConnected = ref(false);
  let selectionTarget: string | undefined;
  let detailMode = false;
  let connectSequence = 0;
  const rebuildReads = createReadRequest();
  const selectReads = createReadRequest();
  let operationController: AbortController | undefined;

  // Single-pass fleet rollup; agent_version_state is optional on the wire, so
  // anything missing or not yet classified (unknown, unsupported) rolls up as
  // unknown and current + update + ahead + unknown always sum to nodes.length.
  const fleetStats = computed(() => {
    let onlineCount = 0;
    let offlineCount = 0;
    let relayCount = 0;
    let directCount = 0;
    let totalSessions = 0;
    let currentCount = 0;
    let updateCount = 0;
    let aheadCount = 0;
    let unknownCount = 0;
    for (const node of nodes.value) {
      if (node.connection_state === "online") onlineCount += 1;
      else offlineCount += 1;
      const mode = node.path?.mode;
      if (mode === "relay") relayCount += 1;
      else if (mode === "direct") directCount += 1;
      totalSessions += node.session_count;
      switch (node.agent_version_state) {
        case "current":
          currentCount += 1;
          break;
        case "upgrade_available":
          updateCount += 1;
          break;
        case "ahead":
          aheadCount += 1;
          break;
        default:
          unknownCount += 1;
          break;
      }
    }
    return {
      online: onlineCount,
      offline: offlineCount,
      relay: relayCount,
      direct: directCount,
      sessionCount: totalSessions,
      agentCurrent: currentCount,
      agentUpdateAvailable: updateCount,
      agentAhead: aheadCount,
      agentUnknown: unknownCount,
    };
  });

  async function rebuild(): Promise<void> {
    if (!isPageActive()) return;
    const request = rebuildReads.begin();
    loading.value = true;
    try {
      // Read the cursor before the nodes so events racing the snapshot are
      // still delivered. An open stream keeps the cursor current itself.
      // Retain the last known cursor when the lookup fails.
      const snapshotEvents = source
        ? undefined
        : await listEvents(undefined, request.signal, "desc", 1).catch(
            () => undefined,
          );
      if (!request.current()) return;
      const rebuilt = await collectAllPages(
        (cursor) => listNodes(cursor, request.signal),
        () => request.current(),
      );
      if (!rebuilt) return;
      nodes.value = rebuilt;
      if (!source && snapshotEvents) eventCursor = snapshotEvents.items[0]?.id;
      snapshotAt.value = new Date();
      initialized.value = true;
      unavailable.value = false;
    } catch {
      if (!request.current()) return;
      unavailable.value = true;
    } finally {
      if (request.finish()) {
        loading.value = false;
      }
    }
  }

  async function select(nodeId: string): Promise<void> {
    if (selectionTarget !== nodeId) {
      clearOperation();
      selected.value = undefined;
      for (const section of detailSections) section.reset();
    }
    selectionTarget = nodeId;
    const request = selectReads.begin();
    selecting.value = true;
    selectionError.value = "";
    try {
      const node = await getNode(nodeId, request.signal);
      if (!request.current()) return;
      selected.value = node;
    } catch (error) {
      if (!request.current()) return;
      if (error instanceof ResponseError && error.response.status === 404) {
        selected.value = undefined;
        for (const section of detailSections) section.reset();
        selectionError.value = "notFound";
      } else {
        selectionError.value = "unavailable";
      }
      return;
    } finally {
      if (request.finish()) {
        selecting.value = false;
        foregroundRefresh.reschedule();
      }
    }

    void sessionSection.load(
      async (signal) =>
        (await collectAllPages(
          (cursor) => listNodeSessions(nodeId, cursor, signal),
          () => {
            signal.throwIfAborted();
            return true;
          },
        )) ?? [],
    );
    void ipBanSection.load(
      async (signal) => (await listNodeIpBans(nodeId, signal)).items,
    );
    void userGroupSection.load(
      async (signal) => (await listNodeUserGroupState(nodeId, signal)).items,
    );
  }

  async function runOperation(
    create: (
      node: NodeObservedState,
      signal: AbortSignal,
    ) => Promise<Operation>,
    terminal: Set<string> = terminalStates,
  ): Promise<void> {
    const node = selected.value;
    if (!node || node.id !== selectionTarget || operationTracking.value) return;
    const nodeId = node.id;
    const controller = new AbortController();
    operationController = controller;
    latestOperation.value = undefined;
    operationNodeId.value = nodeId;
    operationTracking.value = true;
    const selectSequenceAtStart = selectReads.revision;
    const isLatestOperation = () => operationController === controller;
    function observeOperation(operation: Operation): void {
      if (operation.node_id !== nodeId)
        throw new Error("Operation does not belong to the selected node");
      latestOperation.value = operation;
    }
    operationError.value = undefined;
    try {
      let currentOperation = await create(node, controller.signal);
      if (!isLatestOperation()) return;
      observeOperation(currentOperation);
      let recoveryAttempt = 0;
      while (!terminal.has(currentOperation.state)) {
        await abortableDelay(
          pollDelay(currentOperation.state, recoveryAttempt),
          controller.signal,
          "Operation polling aborted",
        );
        if (currentOperation.state === "offline_pending")
          await waitForVisiblePage(controller.signal);
        if (!isLatestOperation()) return;
        const nextOperation = await getOperation(
          currentOperation.id,
          controller.signal,
        );
        if (!isLatestOperation()) return;
        observeOperation(nextOperation);
        recoveryAttempt =
          nextOperation.state === currentOperation.state
            ? recoveryAttempt + 1
            : 0;
        currentOperation = nextOperation;
      }
      if (
        isLatestOperation() &&
        currentOperation.state === "succeeded" &&
        selectReads.revision === selectSequenceAtStart &&
        selected.value?.id === nodeId
      ) {
        await select(nodeId);
      }
    } catch (error) {
      if (!isLatestOperation() || controller.signal.aborted) return;
      operationError.value = error instanceof Error ? error.message : "";
    } finally {
      if (operationController === controller) {
        operationController = undefined;
        operationTracking.value = false;
      }
    }
  }

  async function runSectionOperation(
    section: { state: { value: DetailReadState } },
    create: Parameters<typeof runOperation>[0],
  ): Promise<void> {
    if (section.state.value === "ready" && !selectionError.value)
      await runOperation(create);
  }

  function detachOperation(): void {
    operationController?.abort();
    operationController = undefined;
    operationTracking.value = false;
  }

  function clearOperation(): void {
    detachOperation();
    latestOperation.value = undefined;
    operationNodeId.value = undefined;
    operationError.value = undefined;
  }

  function pauseRefresh(): void {
    connectSequence += 1;
    source?.close();
    source = undefined;
    streamConnected.value = false;
    // Suspend snapshot reads only; accepted operations keep their own tracking.
    rebuildReads.cancel();
    selectReads.cancel();
    for (const section of detailSections) section.cancel();
    // Cancellation fences old finalizers; the read owner settles its UI state.
    loading.value = false;
    selecting.value = false;
  }

  const foregroundRefresh = createForegroundRefresh(
    async () => {
      const sequence = connectSequence;
      if (detailMode) {
        if (selectionTarget && !selectReads.pending)
          await select(selectionTarget);
      } else if (!rebuildReads.pending) {
        await rebuild();
      }
      if (sequence === connectSequence && isPageActive() && !source)
        await connect();
    },
    () =>
      streamConnected.value &&
      (detailMode
        ? selectionError.value !== "unavailable" &&
          detailSections.every(
            ({ state }) =>
              state.value !== "stale" && state.value !== "unavailable",
          )
        : !unavailable.value)
        ? 60_000
        : 15_000,
    pauseRefresh,
  );

  // Polling belongs to the view. The Controller owns accepted mutations.
  function stop(): void {
    foregroundRefresh.stop();
    clearOperation();
    selectReads.cancel();
    selectionTarget = undefined;
    selected.value = undefined;
    for (const section of detailSections) section.reset();
    selecting.value = false;
    selectionError.value = "";
  }

  async function connect(): Promise<void> {
    foregroundRefresh.start(false);
    if (!isPageActive()) return;
    const sequence = ++connectSequence;
    source?.close();
    source = undefined;
    streamConnected.value = false;
    try {
      if (detailMode && !eventCursor) {
        const page = await listEvents(undefined, undefined, "desc", 1).catch(
          () => undefined,
        );
        if (sequence !== connectSequence || !isPageActive()) return;
        eventCursor = page?.items[0]?.id;
      }
      const path = await eventStreamPath(eventCursor);
      if (sequence !== connectSequence || !isPageActive()) return;
      const stream = new EventSource(path);
      source = stream;
      stream.onopen = () => {
        if (sequence !== connectSequence || source !== stream) return;
        streamConnected.value = true;
        if (!detailMode && unavailable.value) foregroundRefresh.request();
        else foregroundRefresh.reschedule();
      };
      stream.addEventListener("platform", (message) => {
        if (sequence !== connectSequence || source !== stream) {
          stream.close();
          return;
        }
        if (!(message instanceof MessageEvent)) return;
        if (message.lastEventId) eventCursor = message.lastEventId;
        let event: unknown;
        try {
          event = JSON.parse(
            typeof message.data === "string" ? message.data : "",
          );
        } catch {
          return;
        }
        if (
          typeof event !== "object" ||
          event === null ||
          !("type" in event) ||
          !("node_id" in event) ||
          typeof event.node_id !== "string"
        )
          return;
        // Routine observations are picked up by the periodic snapshot.
        const connectionChanged =
          event.type === "connected" ||
          event.type === "disconnected" ||
          event.type === "path_changed";
        const commandFinished =
          event.type === "command_result" || event.type === "simulation_result";
        const selectedEvent = event.node_id === selectionTarget;
        if (
          (connectionChanged && (!detailMode || selectedEvent)) ||
          (detailMode && commandFinished && selectedEvent)
        )
          foregroundRefresh.request(fleetEventRefreshDelay);
        if (typeof window !== "undefined")
          window.dispatchEvent(new Event(platformEventsEvent));
      });
      stream.onerror = () => {
        if (sequence !== connectSequence || source !== stream) {
          stream.close();
          return;
        }
        stream.close();
        source = undefined;
        streamConnected.value = false;
        if (!detailMode) unavailable.value = true;
        foregroundRefresh.reschedule();
        void probeAuthentication().catch(() => undefined);
      };
    } catch {
      if (sequence === connectSequence && !detailMode) unavailable.value = true;
    }
  }

  function disconnect(): void {
    stop();
    eventCursor = undefined;
  }
  onScopeDispose(disconnect);
  return {
    nodes,
    selected,
    sessions: sessionSection.data,
    sessionsState: sessionSection.state,
    ipBans: ipBanSection.data,
    ipBansState: ipBanSection.state,
    userGroupState: userGroupSection.data,
    userGroupReadState: userGroupSection.state,
    latestOperation,
    operationNodeId,
    operationTracking,
    operationError,
    loading,
    initialized,
    selecting,
    selectionError,
    unavailable,
    snapshotAt,
    stats: fleetStats,
    rebuild,
    start: (mode: "fleet" | "detail" = "fleet") => {
      detailMode = mode === "detail";
      foregroundRefresh.start();
    },
    stop,
    streamConnected,
    select,
    connect,
    disconnect,
    disconnectSession: (sessionId: string, reason: string) =>
      runSectionOperation(sessionSection, (node, signal) =>
        disconnectSession(node, sessionId, reason, signal),
      ),
    terminateSession: (sessionId: string, reason: string) =>
      runSectionOperation(sessionSection, (node, signal) =>
        terminateSession(node, sessionId, reason, signal),
      ),
    removeIpBan: (ip: string, reason: string) =>
      runSectionOperation(ipBanSection, (node, signal) =>
        removeIpBan(node, ip, reason, signal),
      ),
    reloadService: (reason: string) =>
      runOperation((node, signal) => reloadService(node, reason, signal)),
    upgradeAgent: (targetVersion: string, reason: string) =>
      runOperation(
        (node, signal) => upgradeNodeAgent(node, targetVersion, reason, signal),
        upgradeTerminalStates,
      ),
    createUser: (
      name: string,
      version: number,
      sealedPassword: string,
      secretKeyId: string,
      reason: string,
    ) =>
      runSectionOperation(userGroupSection, (node, signal) =>
        createUser(
          node.id,
          name,
          version,
          sealedPassword,
          secretKeyId,
          reason,
          signal,
        ),
      ),
    disableUser: (username: string, version: number, reason: string) =>
      runSectionOperation(userGroupSection, (node, signal) =>
        disableUser(node.id, username, version, reason, signal),
      ),
    enableUser: (username: string, version: number, reason: string) =>
      runSectionOperation(userGroupSection, (node, signal) =>
        enableUser(node.id, username, version, reason, signal),
      ),
    rotateUserPassword: (
      username: string,
      version: number,
      sealedPassword: string,
      secretKeyId: string,
      reason: string,
    ) =>
      runSectionOperation(userGroupSection, (node, signal) =>
        rotateUserPassword(
          node.id,
          username,
          version,
          sealedPassword,
          secretKeyId,
          reason,
          signal,
        ),
      ),
    applyGroup: (
      groupName: string,
      version: number,
      members: string[],
      reason: string,
    ) =>
      runSectionOperation(userGroupSection, (node, signal) =>
        applyGroup(node.id, groupName, version, members, reason, signal),
      ),
    trackOperation: (operationId: string) =>
      runOperation((_node, signal) => getOperation(operationId, signal)),
    detachOperation,
  };
});
