import {
  OperationsApi,
  AgentRolloutExclusionFromJSON,
  AgentRolloutFromJSON,
  type NodeObservedState,
  type Operation,
  type AgentRollout,
  type AgentRolloutPage,
} from "@ocservia/api-client";
import { configuration, newIdempotencyKey, requestInit } from "./transport";
import { getWorkspaceId } from "./workspace";

export type RolloutDetail = Omit<AgentRollout, "excluded"> & {
  excluded: Array<{ nodeId: string; reason: string; raw: string }>;
};

// Stored exclusions may predate the current protocol. Decode known shapes
// once here and keep unfamiliar JSON visible rather than dropping history.
function rolloutDetail(wire: unknown): RolloutDetail {
  if (!wire || typeof wire !== "object" || Array.isArray(wire)) {
    throw new Error("Invalid rollout response");
  }
  const excluded = "excluded" in wire ? wire.excluded : undefined;
  if (excluded != null && !Array.isArray(excluded)) {
    throw new Error("Invalid rollout exclusions");
  }
  return {
    // Generated converters implement the current model. Read stored exclusions
    // from the raw response before conversion so historical values survive.
    ...AgentRolloutFromJSON({ ...wire, excluded: undefined }),
    excluded: (excluded ?? []).map((entry: unknown) => {
      if (
        entry !== null &&
        typeof entry === "object" &&
        "node_id" in entry &&
        typeof entry.node_id === "string" &&
        "reason" in entry &&
        typeof entry.reason === "string"
      ) {
        return { ...AgentRolloutExclusionFromJSON(entry), raw: "" };
      }
      return {
        nodeId: "",
        reason: "",
        raw: JSON.stringify(entry) ?? String(entry),
      };
    }),
  };
}

async function rolloutResponse(
  response: Promise<{ raw: Response }>,
): Promise<RolloutDetail> {
  const wire: unknown = await (await response).raw.json();
  return rolloutDetail(wire);
}

const operations = new OperationsApi(configuration);

// The browser only names the trusted release; the Controller resolves the
// package digest from its operator-provisioned catalog, and the reconciled
// terminal outcome (including the conservative unknown) arrives through the
// operation being tracked.
export async function upgradeNodeAgent(
  node: NodeObservedState,
  targetVersion: string,
  reason: string,
  signal?: AbortSignal,
): Promise<Operation> {
  return operations.upgradeNodeAgent(
    {
      nodeId: node.id,
      idempotencyKey: newIdempotencyKey(),
      ifMatch: `"revision-${String(node.version)}"`,
      agentUpgradeRequest: { targetVersion, reason },
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
  signal?: AbortSignal,
): Promise<AgentRollout> {
  return rolloutResponse(
    operations.createAgentRolloutRaw(
      {
        idempotencyKey: newIdempotencyKey(),
        xWorkspaceID: await getWorkspaceId(),
        agentRolloutCreateRequest: {
          targetVersion,
          nodeIds,
          batchSize,
          reason,
        },
      },
      requestInit(signal),
    ),
  );
}

export async function listAgentRollouts(
  limit?: number,
  signal?: AbortSignal,
): Promise<AgentRolloutPage> {
  const xWorkspaceID = await getWorkspaceId();
  const response = await operations.listAgentRolloutsRaw(
    limit === undefined ? { xWorkspaceID } : { xWorkspaceID, limit },
    requestInit(signal),
  );
  const wire: unknown = await response.raw.json();
  if (
    !wire ||
    typeof wire !== "object" ||
    !("rollouts" in wire) ||
    !Array.isArray(wire.rollouts)
  ) {
    throw new Error("Invalid rollout page");
  }
  return { rollouts: wire.rollouts.map(rolloutDetail) };
}

export async function getAgentRollout(
  rolloutId: string,
  signal?: AbortSignal,
): Promise<RolloutDetail> {
  return rolloutResponse(
    operations.getAgentRolloutRaw(
      {
        xWorkspaceID: await getWorkspaceId(),
        rolloutId,
      },
      requestInit(signal),
    ),
  );
}

export async function resumeAgentRollout(
  rolloutId: string,
  signal?: AbortSignal,
): Promise<RolloutDetail> {
  return rolloutResponse(
    operations.resumeAgentRolloutRaw(
      {
        idempotencyKey: newIdempotencyKey(),
        xWorkspaceID: await getWorkspaceId(),
        rolloutId,
      },
      requestInit(signal),
    ),
  );
}
