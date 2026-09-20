import {
  NodesApi,
  type NodeObservedState,
  type NodePage,
  type NodeSessionPage,
  type NodeIpBanPage,
  type UserGroupStatePage,
} from "@ocservia/api-client";
import { configuration, requestInit } from "./transport";
import { workspaceID } from "./workspace";

const nodes = new NodesApi(configuration);

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
