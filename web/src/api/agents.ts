import {
  fromWire,
  type AgentRollout,
  type NodeObservedState,
  type Schemas,
} from "./models";
import { client, newIdempotencyKey, read, ResponseError } from "./transport";
import { getWorkspaceId } from "./workspace";

export type RolloutDetail = Omit<AgentRollout, "excluded"> & {
  excluded: Array<{ nodeId: string; reason: string; raw: string }>;
};

function rolloutDetail({
  excluded,
  ...rollout
}: Schemas["AgentRollout"]): RolloutDetail {
  return {
    ...fromWire(rollout),
    excluded: (excluded ?? []).map((entry) => {
      if (
        entry !== null &&
        typeof entry === "object" &&
        "node_id" in entry &&
        typeof entry.node_id === "string" &&
        "reason" in entry &&
        typeof entry.reason === "string"
      ) {
        return { nodeId: entry.node_id, reason: entry.reason, raw: "" };
      }
      return { nodeId: "", reason: "", raw: JSON.stringify(entry) };
    }),
  };
}

async function readRollout(
  request: Promise<{ data?: Schemas["AgentRollout"]; response: Response }>,
) {
  const { data, response } = await request;
  if (data === undefined) throw new ResponseError(response);
  return rolloutDetail(data);
}

export function upgradeNodeAgent(
  node: NodeObservedState,
  targetVersion: string,
  reason: string,
  signal?: AbortSignal,
) {
  return read(
    client.POST("/nodes/{node_id}/agent-upgrade", {
      params: {
        path: { node_id: node.id },
        header: {
          "Idempotency-Key": newIdempotencyKey(),
          "If-Match": `"revision-${String(node.version)}"`,
        },
      },
      body: { target_version: targetVersion, reason },
      signal: signal ?? null,
    }),
  );
}

export async function createAgentRollout(
  targetVersion: string,
  nodeIds: string[],
  batchSize: number,
  reason: string,
  signal?: AbortSignal,
) {
  return readRollout(
    client.POST("/agent-rollouts", {
      params: {
        header: {
          "X-Workspace-ID": await getWorkspaceId(),
          "Idempotency-Key": newIdempotencyKey(),
        },
      },
      body: {
        target_version: targetVersion,
        node_ids: nodeIds,
        batch_size: batchSize,
        reason,
      },
      signal: signal ?? null,
    }),
  );
}

export async function listAgentRollouts(limit?: number, signal?: AbortSignal) {
  const { data, response } = await client.GET("/agent-rollouts", {
    params: {
      header: { "X-Workspace-ID": await getWorkspaceId() },
      ...(limit === undefined ? {} : { query: { limit } }),
    },
    signal: signal ?? null,
  });
  if (data === undefined) throw new ResponseError(response);
  return { rollouts: data.rollouts.map(rolloutDetail) };
}

export async function getAgentRollout(rolloutId: string, signal?: AbortSignal) {
  return readRollout(
    client.GET("/agent-rollouts/{rollout_id}", {
      params: {
        path: { rollout_id: rolloutId },
        header: { "X-Workspace-ID": await getWorkspaceId() },
      },
      signal: signal ?? null,
    }),
  );
}

export async function resumeAgentRollout(
  rolloutId: string,
  signal?: AbortSignal,
) {
  return readRollout(
    client.POST("/agent-rollouts/{rollout_id}/resume", {
      params: {
        path: { rollout_id: rolloutId },
        header: {
          "X-Workspace-ID": await getWorkspaceId(),
          "Idempotency-Key": newIdempotencyKey(),
        },
      },
      signal: signal ?? null,
    }),
  );
}
