import {
  ResponseError,
  type NodeIpBan,
  type NodeObservedState,
  type NodeSession,
  type Operation,
  type UserGroupResourceState,
} from "@ocservia/api-client";
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
  getWorkspace,
  sameWorkspaceContext,
  workspaceContext,
  type WorkspaceContext,
} from "../api/workspace";
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

function pollDelay(state: Operation["state"], recoveryAttempt: number): number {
  if (state !== "unknown") return 750;
  return (
    recoveryPollDelays[
      Math.min(recoveryAttempt, recoveryPollDelays.length - 1)
    ] ?? 5_000
  );
}

export const useFleetStore = defineStore("fleet", () => {
  const nodes = shallowRef<NodeObservedState[]>([]);
  const selected = shallowRef<NodeObservedState>();
  const sessions = shallowRef<NodeSession[]>([]);
  const ipBans = shallowRef<NodeIpBan[]>([]);
  const userGroupState = shallowRef<UserGroupResourceState[]>([]);
  const latestOperation = ref<Operation>();
  const operationTracking = ref(false);
  const operationError = ref("");
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
  let connectSequence = 0;
  const rebuildReads = createReadRequest();
  const selectReads = createReadRequest();
  let operationController: AbortController | undefined;
  let operationSequence = 0;

  function isCurrent(
    context: WorkspaceContext,
    controller?: AbortController,
  ): boolean {
    return (
      !controller?.signal.aborted &&
      sameWorkspaceContext(workspaceContext(), context)
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

  // Single-pass fleet rollup; agentVersionState is optional on the wire, so
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
      if (node.connectionState === "online") onlineCount += 1;
      else offlineCount += 1;
      const mode = node.path?.mode;
      if (mode === "relay") relayCount += 1;
      else if (mode === "direct") directCount += 1;
      totalSessions += node.sessionCount;
      switch (node.agentVersionState) {
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

  const online = computed(() => fleetStats.value.online);
  const offline = computed(() => fleetStats.value.offline);
  const relay = computed(() => fleetStats.value.relay);
  const direct = computed(() => fleetStats.value.direct);
  const sessionCount = computed(() => fleetStats.value.sessionCount);
  const agentCurrent = computed(() => fleetStats.value.agentCurrent);
  const agentUpdateAvailable = computed(
    () => fleetStats.value.agentUpdateAvailable,
  );
  const agentAhead = computed(() => fleetStats.value.agentAhead);
  const agentUnknown = computed(() => fleetStats.value.agentUnknown);

  async function rebuild(): Promise<void> {
    if (!isPageActive()) return;
    const request = rebuildReads.begin();
    const selectSequenceAtStart = selectReads.revision;
    let context: WorkspaceContext | undefined;
    loading.value = true;
    try {
      context = await currentContext();
      const isLatestRebuild = () => request.current(context);
      if (!isLatestRebuild()) return;
      // Read the cursor before the nodes so events racing the snapshot are
      // still delivered. An open stream keeps the cursor current itself.
      // A failed lookup falls back to a full replay rather than failing.
      const snapshotCursor = source
        ? undefined
        : await listEvents(undefined, request.signal, "desc", 1).then(
            (page) => page.items[0]?.id,
            () => undefined,
          );
      if (!isLatestRebuild()) return;
      const rebuilt: NodeObservedState[] = [];
      let cursor: string | undefined;
      do {
        const page = await listNodes(cursor, request.signal);
        if (!isLatestRebuild()) return;
        rebuilt.push(...page.items);
        cursor = page.page.hasMore ? page.page.nextCursor : undefined;
      } while (cursor);
      if (!isLatestRebuild()) return;
      nodes.value = rebuilt;
      if (!source) eventCursor = snapshotCursor;
      snapshotAt.value = new Date();
      initialized.value = true;
      if (selectionTarget && selectReads.revision === selectSequenceAtStart)
        await select(selectionTarget);
      if (!isLatestRebuild()) return;
      unavailable.value = false;
    } catch {
      if (!request.current(context)) return;
      unavailable.value = true;
    } finally {
      if (request.finish()) {
        loading.value = false;
      }
    }
  }

  async function select(nodeId: string): Promise<void> {
    selectionTarget = nodeId;
    if (selected.value && selected.value.id !== nodeId) {
      detachOperation();
      selected.value = undefined;
      sessions.value = [];
      ipBans.value = [];
      userGroupState.value = [];
    }
    const request = selectReads.begin();
    let context: WorkspaceContext | undefined;
    selecting.value = true;
    selectionError.value = "";
    try {
      context = await currentContext();
      const isLatestSelect = () => request.current(context);
      if (!isLatestSelect()) return;
      const node = await getNode(nodeId, request.signal);
      if (!isLatestSelect()) return;
      const loadNodeSessions = async (): Promise<NodeSession[]> => {
        const rebuiltSessions: NodeSession[] = [];
        let cursor: string | undefined;
        do {
          const page = await listNodeSessions(nodeId, cursor, request.signal);
          if (!isLatestSelect()) return rebuiltSessions;
          rebuiltSessions.push(...page.items);
          cursor = page.page.hasMore ? page.page.nextCursor : undefined;
        } while (cursor);
        return rebuiltSessions;
      };
      const [rebuiltSessions, ipBansPage, userGroupStatePage] =
        await Promise.all([
          loadNodeSessions(),
          listNodeIpBans(nodeId, request.signal),
          listNodeUserGroupState(nodeId, request.signal),
        ]);
      if (!isLatestSelect()) return;
      selected.value = node;
      sessions.value = rebuiltSessions;
      ipBans.value = ipBansPage.items;
      userGroupState.value = userGroupStatePage.items;
      selectionError.value = "";
      unavailable.value = false;
    } catch (error) {
      if (!request.current(context)) return;
      if (error instanceof ResponseError && error.response.status === 404) {
        selectionError.value = "notFound";
      } else {
        unavailable.value = true;
        selectionError.value = "unavailable";
      }
    } finally {
      if (request.finish()) {
        selecting.value = false;
        foregroundRefresh.reschedule();
      }
    }
  }

  async function runOperation(
    create: (
      node: NodeObservedState,
      signal: AbortSignal,
    ) => Promise<Operation>,
    terminal: Set<string> = terminalStates,
  ): Promise<void> {
    if (!selected.value || operationTracking.value) return;
    operationController?.abort();
    const context = workspaceContext();
    const controller = new AbortController();
    operationController = controller;
    operationTracking.value = true;
    const sequence = ++operationSequence;
    const selectSequenceAtStart = selectReads.revision;
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
      let recoveryAttempt = 0;
      while (!terminal.has(currentOperation.state)) {
        const recovering = currentOperation.state === "unknown";
        await abortableDelay(
          pollDelay(currentOperation.state, recoveryAttempt),
          controller.signal,
          "Operation polling aborted",
        );
        if (!isLatestOperation()) return;
        currentOperation = await getOperation(
          currentOperation.id,
          controller.signal,
        );
        if (!isLatestOperation()) return;
        latestOperation.value = currentOperation;
        recoveryAttempt = recovering ? recoveryAttempt + 1 : 0;
      }
      if (
        isLatestOperation() &&
        currentOperation.state === "succeeded" &&
        selectReads.revision === selectSequenceAtStart &&
        selected.value.id === node.id
      ) {
        await select(node.id);
      }
    } catch (error) {
      if (!isLatestOperation() || controller.signal.aborted) return;
      operationError.value =
        error instanceof Error ? error.message : "Operation failed";
    } finally {
      if (operationController === controller) {
        operationController = undefined;
        operationTracking.value = false;
      }
    }
  }

  function detachOperation(): void {
    operationController?.abort();
    operationController = undefined;
    operationTracking.value = false;
    operationSequence += 1;
  }

  function pauseRefresh(): void {
    connectSequence += 1;
    source?.close();
    source = undefined;
    streamConnected.value = false;
    // Suspend snapshot reads only; accepted operations keep their own tracking.
    rebuildReads.cancel();
    selectReads.cancel();
    // Cancellation fences old finalizers; the read owner settles its UI state.
    loading.value = false;
    selecting.value = false;
  }

  const foregroundRefresh = createForegroundRefresh(
    async () => {
      const sequence = connectSequence;
      if (!loading.value || !rebuildReads.pending) await rebuild();
      if (sequence === connectSequence && isPageActive() && !source)
        await connect();
    },
    () =>
      streamConnected.value &&
      !unavailable.value &&
      selectionError.value !== "unavailable"
        ? 60_000
        : 15_000,
    pauseRefresh,
  );

  // Leaving a view stops its reads, but does not cancel accepted mutations.
  function stop(): void {
    foregroundRefresh.stop();
    selectReads.cancel();
    selectionTarget = undefined;
    selected.value = undefined;
    sessions.value = [];
    ipBans.value = [];
    userGroupState.value = [];
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
    let context: WorkspaceContext;
    try {
      context = await currentContext();
      const path = await eventStreamPath(eventCursor);
      if (
        sequence !== connectSequence ||
        !isCurrent(context) ||
        !isPageActive()
      )
        return;
      const stream = new EventSource(path);
      if (sequence !== connectSequence || !isCurrent(context)) {
        stream.close();
        return;
      }
      source = stream;
      stream.onopen = () => {
        if (
          sequence !== connectSequence ||
          !isCurrent(context) ||
          source !== stream
        )
          return;
        streamConnected.value = true;
        if (unavailable.value) foregroundRefresh.request();
        else foregroundRefresh.reschedule();
      };
      stream.addEventListener("platform", (message) => {
        if (
          sequence !== connectSequence ||
          !isCurrent(context) ||
          source !== stream
        ) {
          stream.close();
          return;
        }
        if (message instanceof MessageEvent && message.lastEventId)
          eventCursor = message.lastEventId;
        foregroundRefresh.request(150);
        if (typeof window !== "undefined")
          window.dispatchEvent(new Event(platformEventsEvent));
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
        stream.close();
        source = undefined;
        streamConnected.value = false;
        unavailable.value = true;
        foregroundRefresh.reschedule();
        void probeAuthentication().catch(() => undefined);
      };
    } catch {
      if (sequence === connectSequence) unavailable.value = true;
    }
  }

  function disconnect(): void {
    stop();
    eventCursor = undefined;
    detachOperation();
  }
  onScopeDispose(disconnect);
  return {
    nodes,
    selected,
    sessions,
    ipBans,
    userGroupState,
    latestOperation,
    operationTracking,
    operationError,
    loading,
    initialized,
    selecting,
    selectionError,
    unavailable,
    snapshotAt,
    online,
    offline,
    relay,
    direct,
    sessionCount,
    agentCurrent,
    agentUpdateAvailable,
    agentAhead,
    agentUnknown,
    rebuild,
    start: () => {
      foregroundRefresh.start();
    },
    stop,
    streamConnected,
    select,
    connect,
    disconnect,
    disconnectSession: (sessionId: string, reason: string) =>
      runOperation((node, signal) =>
        disconnectSession(node, sessionId, reason, signal),
      ),
    terminateSession: (sessionId: string, reason: string) =>
      runOperation((node, signal) =>
        terminateSession(node, sessionId, reason, signal),
      ),
    removeIpBan: (ip: string, reason: string) =>
      runOperation((node, signal) => removeIpBan(node, ip, reason, signal)),
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
      runOperation((node, signal) =>
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
      runOperation((node, signal) =>
        disableUser(node.id, username, version, reason, signal),
      ),
    enableUser: (username: string, version: number, reason: string) =>
      runOperation((node, signal) =>
        enableUser(node.id, username, version, reason, signal),
      ),
    rotateUserPassword: (
      username: string,
      version: number,
      sealedPassword: string,
      secretKeyId: string,
      reason: string,
    ) =>
      runOperation((node, signal) =>
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
      runOperation((node, signal) =>
        applyGroup(node.id, groupName, version, members, reason, signal),
      ),
    trackOperation: (operationId: string) =>
      runOperation((_node, signal) => getOperation(operationId, signal)),
    detachOperation,
  };
});
