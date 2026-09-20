import {
  OperationsApi,
  DevelopmentApi,
  type NodeObservedState,
  type Operation,
  type OperationPage,
  type OperationSummary,
  type SimulationScenario,
} from "@ocservia/api-client";
import { configuration, newIdempotencyKey, requestInit } from "./transport";
import { workspaceID } from "./workspace";

const operations = new OperationsApi(configuration);
const development = new DevelopmentApi(configuration);

function controlledRequest(node: NodeObservedState, reason: string) {
  return {
    idempotencyKey: newIdempotencyKey(),
    ifMatch: `"revision-${String(node.version)}"`,
    controlledOperationRequest: {
      reason,
      expectedVersion: node.version,
      ttlSeconds: 60,
    },
  };
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

export async function operationSummary(
  signal?: AbortSignal,
): Promise<OperationSummary> {
  const xWorkspaceID = await workspaceID();
  return operations.getOperationSummary({ xWorkspaceID }, requestInit(signal));
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
