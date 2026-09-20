import {
  OperationsApi,
  type NodeObservedState,
  type Operation,
  type AgentRollout,
  type AgentRolloutPage,
} from "@ocservia/api-client";
import { configuration, newIdempotencyKey, requestInit } from "./transport";
import { workspaceID } from "./workspace";

const operations = new OperationsApi(configuration);

// The browser only names the trusted release; the Controller resolves the
// package digest from its operator-provisioned catalog, and the reconciled
// terminal outcome (including the conservative unknown) arrives through the
// operation being tracked.
export async function upgradeNodeAgent(
  node: NodeObservedState,
  targetVersion: string,
  reason: string,
  approvalId: string,
  signal?: AbortSignal,
): Promise<Operation> {
  return operations.upgradeNodeAgent(
    {
      nodeId: node.id,
      idempotencyKey: newIdempotencyKey(),
      ifMatch: `"revision-${String(node.version)}"`,
      agentUpgradeRequest: { targetVersion, approvalId, reason },
    },
    requestInit(signal),
  );
}

// Fleet rollouts are orchestrated server-side: the browser only selects the
// target version, candidate nodes, and batch size, then tracks durable
// rollout state that survives browser closure and Controller restart.
export async function createAgentRollout(
  targetVersion: string,
  nodeIds: string[],
  batchSize: number,
  reason: string,
  approvalId: string,
  signal?: AbortSignal,
): Promise<AgentRollout> {
  return operations.createAgentRollout(
    {
      idempotencyKey: newIdempotencyKey(),
      xWorkspaceID: await workspaceID(),
      agentRolloutCreateRequest: {
        targetVersion,
        nodeIds,
        batchSize,
        reason,
        approvalId,
      },
    },
    requestInit(signal),
  );
}

export async function listAgentRollouts(
  limit?: number,
  signal?: AbortSignal,
): Promise<AgentRolloutPage> {
  const xWorkspaceID = await workspaceID();
  return operations.listAgentRollouts(
    limit === undefined ? { xWorkspaceID } : { xWorkspaceID, limit },
    requestInit(signal),
  );
}

export async function getAgentRollout(
  rolloutId: string,
  signal?: AbortSignal,
): Promise<AgentRollout> {
  return operations.getAgentRollout(
    {
      xWorkspaceID: await workspaceID(),
      rolloutId,
    },
    requestInit(signal),
  );
}

export async function resumeAgentRollout(
  rolloutId: string,
  signal?: AbortSignal,
): Promise<AgentRollout> {
  return operations.resumeAgentRollout(
    {
      idempotencyKey: newIdempotencyKey(),
      xWorkspaceID: await workspaceID(),
      rolloutId,
    },
    requestInit(signal),
  );
}
