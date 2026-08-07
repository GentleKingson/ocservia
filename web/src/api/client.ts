import {
  Configuration,
  DevelopmentApi,
  EventsApi,
  NodesApi,
  OperationsApi,
  PlatformApi,
  type Operation,
  type OperationPage,
  type PlatformEventPage,
  type Readiness,
  type NodeObservedState,
  type NodeIpBanPage,
  type NodePage,
  type NodeSessionPage,
  type UserGroupStatePage,
  type UserPolicy,
  type UserPolicyRequest,
  type SimulationScenario,
  type Workspace,
} from "@ocservia/api-client";

const devAuthToken = import.meta.env.VITE_DEV_AUTH_TOKEN;
const loginReturnKey = "ocservia.login.return-to";
const loginStartedKey = "ocservia.login.started-at";
const workspaceKey = "ocservia.workspace-id";
export const workspaceChangedEvent = "ocservia:workspace-changed";

async function authenticatedFetch(
  input: RequestInfo | URL,
  init?: RequestInit,
): Promise<Response> {
  const response = await fetch(input, init);
  if (response.status === 401 && typeof window !== "undefined") {
    const started = Number(sessionStorage.getItem(loginStartedKey) ?? "0");
    if (!started || Date.now() - started > 60_000) {
      const returnTo = `${window.location.pathname}${window.location.search}${window.location.hash}`;
      if (returnTo.startsWith("/") && !returnTo.startsWith("//")) {
        sessionStorage.setItem(loginReturnKey, returnTo);
      }
      sessionStorage.setItem(loginStartedKey, String(Date.now()));
      window.location.assign("/api/v1/auth/login");
    }
  }
  return response;
}

const configuration = new Configuration({
  basePath: "/api/v1",
  fetchApi: authenticatedFetch,
  credentials: "same-origin",
  ...(devAuthToken ? { accessToken: devAuthToken } : {}),
});
const operations = new OperationsApi(configuration);
const platform = new PlatformApi(configuration);
const development = new DevelopmentApi(configuration);
const events = new EventsApi(configuration);
const nodes = new NodesApi(configuration);
let selectedWorkspace: Workspace | undefined;
let authorizedWorkspaces: Workspace[] | undefined;
let workspaceRequest: Promise<Workspace[]> | undefined;
let workspaceGeneration = 0;
let authenticationProbe: Promise<void> | undefined;

export interface WorkspaceContext {
  id: string | undefined;
  generation: number;
}

export function workspaceContext(): WorkspaceContext {
  return { id: selectedWorkspace?.id, generation: workspaceGeneration };
}

function setSelectedWorkspace(workspace: Workspace | undefined): void {
  if (selectedWorkspace?.id !== workspace?.id) workspaceGeneration += 1;
  selectedWorkspace = workspace;
}

export async function listAuthorizedWorkspaces(
  refresh = false,
): Promise<Workspace[]> {
  if (authorizedWorkspaces && !refresh) return authorizedWorkspaces;
  workspaceRequest ??= platform
    .listAuthorizedWorkspaces()
    .then((page) => {
      authorizedWorkspaces = page.items;
      const remembered = sessionStorage.getItem(workspaceKey);
      setSelectedWorkspace(
        page.items.find((workspace) => workspace.id === remembered) ??
          page.items[0],
      );
      if (selectedWorkspace)
        sessionStorage.setItem(workspaceKey, selectedWorkspace.id);
      else sessionStorage.removeItem(workspaceKey);
      return page.items;
    })
    .finally(() => {
      workspaceRequest = undefined;
    });
  return workspaceRequest;
}

export async function getWorkspace(): Promise<Workspace> {
  if (selectedWorkspace) return selectedWorkspace;
  const page = await listAuthorizedWorkspaces();
  const remembered = sessionStorage.getItem(workspaceKey);
  const workspace =
    page.find((candidate) => candidate.id === remembered) ?? page[0];
  if (!workspace) throw new Error("No authorized workspace is available");
  setSelectedWorkspace(workspace);
  return workspace;
}

export async function selectWorkspace(workspaceId: string): Promise<Workspace> {
  const workspaces = await listAuthorizedWorkspaces();
  const workspace = workspaces.find(
    (candidate) => candidate.id === workspaceId,
  );
  if (!workspace) throw new Error("Workspace is not authorized");
  if (selectedWorkspace?.id === workspace.id) return workspace;
  setSelectedWorkspace(workspace);
  sessionStorage.setItem(workspaceKey, workspace.id);
  window.dispatchEvent(
    new CustomEvent(workspaceChangedEvent, { detail: workspace.id }),
  );
  return workspace;
}

export function consumeLoginReturnPath(): string | undefined {
  const value = sessionStorage.getItem(loginReturnKey) ?? undefined;
  sessionStorage.removeItem(loginReturnKey);
  sessionStorage.removeItem(loginStartedKey);
  return value?.startsWith("/") && !value.startsWith("//") ? value : undefined;
}

export async function probeAuthentication(): Promise<void> {
  authenticationProbe ??= platform
    .listAuthorizedWorkspaces()
    .then(() => undefined)
    .finally(() => {
      authenticationProbe = undefined;
    });
  return authenticationProbe;
}

function requestInit(signal?: AbortSignal): RequestInit | undefined {
  return signal ? { signal } : undefined;
}

export async function eventStreamPath(after?: string): Promise<string> {
  const query = new URLSearchParams();
  if (after) query.set("after", after);
  try {
    const workspace = await getWorkspace();
    query.set("workspace_id", workspace.id);
  } catch (error) {
    if (!devAuthToken) throw error;
  }
  const encoded = query.toString();
  return `/api/v1/events/stream${encoded ? `?${encoded}` : ""}`;
}

async function workspaceID(): Promise<string> {
  return (await getWorkspace()).id;
}

export async function listOperations(
  cursor?: string,
  signal?: AbortSignal,
): Promise<OperationPage> {
  const xWorkspaceID = await workspaceID();
  return operations.listOperations(
    cursor
      ? { xWorkspaceID, cursor, pageSize: 200 }
      : { xWorkspaceID, pageSize: 200 },
    requestInit(signal),
  );
}

export async function getReadiness(): Promise<Readiness> {
  return platform.getReadiness();
}

export async function createLocalSimulation(
  simulationScenario: SimulationScenario,
  signal?: AbortSignal,
): Promise<Operation> {
  return development.createLocalSimulation(
    { simulationScenario },
    requestInit(signal),
  );
}

export async function getOperation(
  operationId: string,
  signal?: AbortSignal,
): Promise<Operation> {
  return operations.getOperation({ operationId }, requestInit(signal));
}

export async function listEvents(
  after?: string,
  signal?: AbortSignal,
): Promise<PlatformEventPage> {
  const xWorkspaceID = await workspaceID();
  return events.listEvents(
    after
      ? { xWorkspaceID, after, pageSize: 200 }
      : { xWorkspaceID, pageSize: 200 },
    requestInit(signal),
  );
}

export async function listNodes(
  cursor?: string,
  signal?: AbortSignal,
): Promise<NodePage> {
  const xWorkspaceID = await workspaceID();
  return nodes.listNodes(
    cursor
      ? { xWorkspaceID, cursor, pageSize: 200 }
      : { xWorkspaceID, pageSize: 200 },
    requestInit(signal),
  );
}

export async function getNode(
  nodeId: string,
  signal?: AbortSignal,
): Promise<NodeObservedState> {
  return nodes.getNode({ nodeId }, requestInit(signal));
}

export async function listNodeSessions(
  nodeId: string,
  cursor?: string,
  signal?: AbortSignal,
): Promise<NodeSessionPage> {
  return nodes.listNodeSessions(
    cursor ? { nodeId, cursor, pageSize: 200 } : { nodeId, pageSize: 200 },
    requestInit(signal),
  );
}

export async function listNodeIpBans(
  nodeId: string,
  signal?: AbortSignal,
): Promise<NodeIpBanPage> {
  return nodes.listNodeIpBans({ nodeId }, requestInit(signal));
}

export async function listNodeUserGroupState(
  nodeId: string,
  signal?: AbortSignal,
): Promise<UserGroupStatePage> {
  return nodes.listNodeUserGroupState({ nodeId }, requestInit(signal));
}

function desiredRequest(version: number, reason: string) {
  return {
    idempotencyKey: crypto.randomUUID(),
    ifMatch: `"revision-${String(version)}"`,
    reason,
    expectedVersion: version,
    ttlSeconds: 86400,
  };
}

export async function createUser(
  nodeId: string,
  name: string,
  version: number,
  sealedPassword: string,
  secretKeyId: string,
  reason: string,
  signal?: AbortSignal,
): Promise<Operation> {
  const request = desiredRequest(version, reason);
  return operations.createNodeUser(
    {
      nodeId,
      idempotencyKey: request.idempotencyKey,
      ifMatch: request.ifMatch,
      userCreateRequest: {
        name,
        sealedPassword,
        secretKeyId,
        reason: request.reason,
        expectedVersion: request.expectedVersion,
        ttlSeconds: request.ttlSeconds,
      },
    },
    requestInit(signal),
  );
}
export async function disableUser(
  nodeId: string,
  username: string,
  version: number,
  reason: string,
  signal?: AbortSignal,
): Promise<Operation> {
  const request = desiredRequest(version, reason);
  return operations.disableNodeUser(
    {
      nodeId,
      username,
      idempotencyKey: request.idempotencyKey,
      ifMatch: request.ifMatch,
      desiredMutationRequest: {
        reason: request.reason,
        expectedVersion: version,
        ttlSeconds: request.ttlSeconds,
      },
    },
    requestInit(signal),
  );
}
export async function enableUser(
  nodeId: string,
  username: string,
  version: number,
  reason: string,
  signal?: AbortSignal,
): Promise<Operation> {
  const request = desiredRequest(version, reason);
  return operations.enableNodeUser(
    {
      nodeId,
      username,
      idempotencyKey: request.idempotencyKey,
      ifMatch: request.ifMatch,
      desiredMutationRequest: {
        reason: request.reason,
        expectedVersion: version,
        ttlSeconds: request.ttlSeconds,
      },
    },
    requestInit(signal),
  );
}
export async function rotateUserPassword(
  nodeId: string,
  username: string,
  version: number,
  sealedPassword: string,
  secretKeyId: string,
  reason: string,
  signal?: AbortSignal,
): Promise<Operation> {
  const request = desiredRequest(version, reason);
  return operations.rotateNodeUserPassword(
    {
      nodeId,
      username,
      idempotencyKey: request.idempotencyKey,
      ifMatch: request.ifMatch,
      passwordRotateRequest: {
        sealedPassword,
        secretKeyId,
        reason: request.reason,
        expectedVersion: version,
        ttlSeconds: request.ttlSeconds,
      },
    },
    requestInit(signal),
  );
}
export async function applyGroup(
  nodeId: string,
  groupName: string,
  version: number,
  members: string[],
  reason: string,
  signal?: AbortSignal,
): Promise<Operation> {
  const request = desiredRequest(version, reason);
  return operations.applyNodeGroup(
    {
      nodeId,
      groupName,
      idempotencyKey: request.idempotencyKey,
      ifMatch: request.ifMatch,
      groupApplyRequest: {
        members: new Set(members),
        reason: request.reason,
        expectedVersion: version,
        ttlSeconds: request.ttlSeconds,
      },
    },
    requestInit(signal),
  );
}

export async function getUserPolicy(
  nodeId: string,
  username: string,
  signal?: AbortSignal,
): Promise<UserPolicy> {
  return nodes.getNodeUserPolicy({ nodeId, username }, requestInit(signal));
}

export async function setUserPolicy(
  nodeId: string,
  username: string,
  policy: UserPolicyRequest,
  signal?: AbortSignal,
): Promise<UserPolicy> {
  return operations.setNodeUserPolicy(
    {
      nodeId,
      username,
      idempotencyKey: crypto.randomUUID(),
      userPolicyRequest: policy,
    },
    requestInit(signal),
  );
}

function controlledRequest(node: NodeObservedState, reason: string) {
  return {
    idempotencyKey: crypto.randomUUID(),
    ifMatch: `"revision-${String(node.version)}"`,
    controlledOperationRequest: {
      reason,
      expectedVersion: node.version,
      ttlSeconds: 60,
    },
  };
}

export async function disconnectSession(
  node: NodeObservedState,
  sessionId: string,
  reason: string,
  signal?: AbortSignal,
): Promise<Operation> {
  if (!node.bootId) throw new Error("Node boot identity is unavailable");
  const request = controlledRequest(node, reason);
  return operations.disconnectNodeSession(
    {
      nodeId: node.id,
      sessionId,
      ...request,
      controlledOperationRequest: {
        ...request.controlledOperationRequest,
        bootId: node.bootId,
      },
    },
    requestInit(signal),
  );
}

export async function terminateSession(
  node: NodeObservedState,
  sessionId: string,
  reason: string,
  signal?: AbortSignal,
): Promise<Operation> {
  if (!node.bootId) throw new Error("Node boot identity is unavailable");
  const request = controlledRequest(node, reason);
  return operations.terminateNodeSession(
    {
      nodeId: node.id,
      sessionId,
      ...request,
      controlledOperationRequest: {
        ...request.controlledOperationRequest,
        bootId: node.bootId,
      },
    },
    requestInit(signal),
  );
}

export async function removeIpBan(
  node: NodeObservedState,
  ip: string,
  reason: string,
  signal?: AbortSignal,
): Promise<Operation> {
  return operations.removeNodeIpBan(
    {
      nodeId: node.id,
      ip,
      ...controlledRequest(node, reason),
    },
    requestInit(signal),
  );
}

export async function reloadService(
  node: NodeObservedState,
  reason: string,
  approvalId: string,
  signal?: AbortSignal,
): Promise<Operation> {
  return operations.reloadNodeService(
    {
      nodeId: node.id,
      xApprovalID: approvalId,
      ...controlledRequest(node, reason),
    },
    requestInit(signal),
  );
}
