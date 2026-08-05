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

export async function listOperations(cursor?: string): Promise<OperationPage> {
  return operations.listOperations(
    cursor ? { cursor, pageSize: 200 } : { pageSize: 200 },
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
  return events.listEvents(
    after ? { after, pageSize: 200 } : { pageSize: 200 },
  );
}

export async function listNodes(cursor?: string): Promise<NodePage> {
  return nodes.listNodes(
    cursor ? { cursor, pageSize: 200 } : { pageSize: 200 },
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
): Promise<Operation> {
  return operations.reloadNodeService({
    nodeId: node.id,
    ...controlledRequest(node, reason),
  });
}
