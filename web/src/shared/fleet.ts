import type {
  NodeIpBan,
  NodeObservedState,
  NodeSession,
  Operation,
  UserGroupResourceState,
} from "@ocservia/api-client";
import { defineStore } from "pinia";
import { computed, onScopeDispose, ref } from "vue";

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

function responseStatus(error: unknown): number | undefined {
  if (typeof error !== "object" || error === null || !("response" in error))
    return undefined;
  const response = (error as { response?: unknown }).response;
  if (typeof response !== "object" || response === null) return undefined;
  const status = (response as { status?: unknown }).status;
  return typeof status === "number" ? status : undefined;
}

export const useFleetStore = defineStore("fleet", () => {
  const nodes = ref<NodeObservedState[]>([]);
  const selected = ref<NodeObservedState>();
  const sessions = ref<NodeSession[]>([]);
  const ipBans = ref<NodeIpBan[]>([]);
  const userGroupState = ref<UserGroupResourceState[]>([]);
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
  const controllers = new Set<AbortController>();
  const rebuildReads = createReadRequest();
  const selectReads = createReadRequest();
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
    rebuildReads.cancel();
    selectReads.cancel();
    operationController = undefined;
    selecting.value = false;
    operationTracking.value = false;
    operationSequence += 1;
  }

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

  const online = computed(
    () =>
      nodes.value.filter((node) => node.connectionState === "online").length,
  );
  const offline = computed(
    () =>
      nodes.value.filter((node) => node.connectionState !== "online").length,
  );
  const relay = computed(
    () => nodes.value.filter((node) => node.path?.mode === "relay").length,
  );
  const direct = computed(
    () => nodes.value.filter((node) => node.path?.mode === "direct").length,
  );
  const sessionCount = computed(() =>
    nodes.value.reduce((total, node) => total + node.sessionCount, 0),
  );
  const agentCurrent = computed(
    () =>
      nodes.value.filter((node) => node.agentVersionState === "current").length,
  );
  const agentUpdateAvailable = computed(
    () =>
      nodes.value.filter(
        (node) => node.agentVersionState === "upgrade_available",
      ).length,
  );
  const agentAhead = computed(
    () =>
      nodes.value.filter((node) => node.agentVersionState === "ahead").length,
  );
  // agentVersionState is optional on the wire, so anything missing or not yet
  // classified (unknown, unsupported) rolls up as unknown; current, update,
  // ahead, and unknown therefore always sum to the fleet node count.
  const agentUnknown = computed(
    () =>
      nodes.value.filter(
        (node) =>
          node.agentVersionState !== "current" &&
          node.agentVersionState !== "upgrade_available" &&
          node.agentVersionState !== "ahead",
      ).length,
  );

  async function rebuild(): Promise<void> {
    if (!isPageActive()) return;
    const request = rebuildReads.begin();
    const selectSequenceAtStart = selectReads.revision;
    let context: WorkspaceContext | undefined;
    loading.value = true;
    try {
      context = await currentContext();
      const isLatestRebuild = () =>
        Boolean(context && request.current(context));
      if (!isLatestRebuild()) return;
      // Read the cursor before the nodes so events racing the snapshot are
      // still delivered. An open stream keeps the cursor current itself.
      // A failed lookup falls back to a full replay rather than failing.
      const snapshotCursor = source
        ? undefined
        : await listEvents(undefined, request.signal, "desc").then(
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
      if (!context) return;
      const isLatestRebuild = request.current(context);
      if (!isLatestRebuild) return;
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
      cancelRequest(operationController);
      operationController = undefined;
      operationTracking.value = false;
      operationSequence += 1;
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
      const isLatestSelect = () => Boolean(context && request.current(context));
      if (!isLatestSelect()) return;
      const node = await getNode(nodeId, request.signal);
      if (!isLatestSelect()) return;
      const rebuiltSessions: NodeSession[] = [];
      let cursor: string | undefined;
      do {
        const page = await listNodeSessions(nodeId, cursor, request.signal);
        if (!isLatestSelect()) return;
        rebuiltSessions.push(...page.items);
        cursor = page.page.hasMore ? page.page.nextCursor : undefined;
      } while (cursor);
      if (!isLatestSelect()) return;
      const rebuiltIpBans = (await listNodeIpBans(nodeId, request.signal))
        .items;
      const rebuiltUserGroupState = (
        await listNodeUserGroupState(nodeId, request.signal)
      ).items;
      if (!isLatestSelect()) return;
      selected.value = node;
      sessions.value = rebuiltSessions;
      ipBans.value = rebuiltIpBans;
      userGroupState.value = rebuiltUserGroupState;
      selectionError.value = "";
      unavailable.value = false;
    } catch (error) {
      if (!context) return;
      const isLatestSelect = request.current(context);
      if (!isLatestSelect) return;
      if (responseStatus(error) === 404) {
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
    cancelRequest(operationController);
    const context = workspaceContext();
    const controller = trackRequest();
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
      releaseRequest(controller);
      if (operationController === controller) {
        operationController = undefined;
        operationTracking.value = false;
      }
    }
  }

  function detachOperation(): void {
    if (!operationController) return;
    cancelRequest(operationController);
    operationController = undefined;
    operationTracking.value = false;
    operationSequence += 1;
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

  async function reload(reason: string): Promise<void> {
    await runOperation((node, signal) => reloadService(node, reason, signal));
  }

  async function upgradeAgent(
    targetVersion: string,
    reason: string,
  ): Promise<void> {
    await runOperation(
      (node, signal) => upgradeNodeAgent(node, targetVersion, reason, signal),
      upgradeTerminalStates,
    );
  }

  async function trackOperation(operationId: string): Promise<void> {
    await runOperation((_node, signal) => getOperation(operationId, signal));
  }

  async function createUserAction(
    name: string,
    version: number,
    sealedPassword: string,
    secretKeyId: string,
    reason: string,
  ): Promise<void> {
    await runOperation((node, signal) =>
      createUser(
        node.id,
        name,
        version,
        sealedPassword,
        secretKeyId,
        reason,
        signal,
      ),
    );
  }
  async function disableUserAction(
    username: string,
    version: number,
    reason: string,
  ): Promise<void> {
    await runOperation((node, signal) =>
      disableUser(node.id, username, version, reason, signal),
    );
  }
  async function enableUserAction(
    username: string,
    version: number,
    reason: string,
  ): Promise<void> {
    await runOperation((node, signal) =>
      enableUser(node.id, username, version, reason, signal),
    );
  }
  async function rotatePasswordAction(
    username: string,
    version: number,
    sealedPassword: string,
    secretKeyId: string,
    reason: string,
  ): Promise<void> {
    await runOperation((node, signal) =>
      rotateUserPassword(
        node.id,
        username,
        version,
        sealedPassword,
        secretKeyId,
        reason,
        signal,
      ),
    );
  }
  async function applyGroupAction(
    groupName: string,
    version: number,
    members: string[],
    reason: string,
  ): Promise<void> {
    await runOperation((node, signal) =>
      applyGroup(node.id, groupName, version, members, reason, signal),
    );
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

  function start(): void {
    foregroundRefresh.start();
  }

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
    connectSequence += 1;
    source?.close();
    source = undefined;
    eventCursor = undefined;
    streamConnected.value = false;
    abortRequests();
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
    start,
    stop,
    streamConnected,
    select,
    connect,
    disconnect,
    disconnectSession: disconnectSessionAction,
    terminateSession: terminate,
    removeIpBan: unban,
    reloadService: reload,
    upgradeAgent,
    createUser: createUserAction,
    disableUser: disableUserAction,
    enableUser: enableUserAction,
    rotateUserPassword: rotatePasswordAction,
    applyGroup: applyGroupAction,
    trackOperation,
    detachOperation,
  };
});
