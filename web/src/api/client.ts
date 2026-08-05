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
  type SimulationScenario,
  type Workspace,
} from "@ocservia/api-client";

const devAuthToken = import.meta.env.VITE_DEV_AUTH_TOKEN;
const configuration = new Configuration({
  basePath: "/api/v1",
  ...(devAuthToken ? { accessToken: devAuthToken } : {}),
});
const operations = new OperationsApi(configuration);
const platform = new PlatformApi(configuration);
const development = new DevelopmentApi(configuration);
const events = new EventsApi(configuration);
const nodes = new NodesApi(configuration);
let selectedWorkspace: Workspace | undefined;

export async function getWorkspace(): Promise<Workspace> {
  if (selectedWorkspace) return selectedWorkspace;
  const page = await platform.listAuthorizedWorkspaces();
  const workspace = page.items[0];
  if (!workspace) throw new Error("No authorized workspace is available");
  selectedWorkspace = workspace;
  return workspace;
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

export async function listOperations(cursor?: string): Promise<OperationPage> {
  const xWorkspaceID = await workspaceID();
  return operations.listOperations(
    cursor
      ? { xWorkspaceID, cursor, pageSize: 200 }
      : { xWorkspaceID, pageSize: 200 },
  );
}

export async function getReadiness(): Promise<Readiness> {
  return platform.getReadiness();
}

export async function createLocalSimulation(
  simulationScenario: SimulationScenario,
): Promise<Operation> {
  return development.createLocalSimulation({ simulationScenario });
}

export async function getOperation(operationId: string): Promise<Operation> {
  return operations.getOperation({ operationId });
}

export async function listEvents(after?: string): Promise<PlatformEventPage> {
  const xWorkspaceID = await workspaceID();
  return events.listEvents(
    after
      ? { xWorkspaceID, after, pageSize: 200 }
      : { xWorkspaceID, pageSize: 200 },
  );
}

export async function listNodes(cursor?: string): Promise<NodePage> {
  const xWorkspaceID = await workspaceID();
  return nodes.listNodes(
    cursor
      ? { xWorkspaceID, cursor, pageSize: 200 }
      : { xWorkspaceID, pageSize: 200 },
  );
}

export async function getNode(nodeId: string): Promise<NodeObservedState> {
  return nodes.getNode({ nodeId });
}

export async function listNodeSessions(
  nodeId: string,
  cursor?: string,
): Promise<NodeSessionPage> {
  return nodes.listNodeSessions(
    cursor ? { nodeId, cursor, pageSize: 200 } : { nodeId, pageSize: 200 },
  );
}

export async function listNodeIpBans(nodeId: string): Promise<NodeIpBanPage> {
  return nodes.listNodeIpBans({ nodeId });
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
): Promise<Operation> {
  if (!node.bootId) throw new Error("Node boot identity is unavailable");
  const request = controlledRequest(node, reason);
  return operations.disconnectNodeSession({
    nodeId: node.id,
    sessionId,
    ...request,
    controlledOperationRequest: {
      ...request.controlledOperationRequest,
      bootId: node.bootId,
    },
  });
}

export async function terminateSession(
  node: NodeObservedState,
  sessionId: string,
  reason: string,
): Promise<Operation> {
  if (!node.bootId) throw new Error("Node boot identity is unavailable");
  const request = controlledRequest(node, reason);
  return operations.terminateNodeSession({
    nodeId: node.id,
    sessionId,
    ...request,
    controlledOperationRequest: {
      ...request.controlledOperationRequest,
      bootId: node.bootId,
    },
  });
}

export async function removeIpBan(
  node: NodeObservedState,
  ip: string,
  reason: string,
): Promise<Operation> {
  return operations.removeNodeIpBan({
    nodeId: node.id,
    ip,
    ...controlledRequest(node, reason),
  });
}

export async function reloadService(
  node: NodeObservedState,
  reason: string,
  approvalId: string,
): Promise<Operation> {
  return operations.reloadNodeService({
    nodeId: node.id,
    xApprovalID: approvalId,
    ...controlledRequest(node, reason),
  });
}
