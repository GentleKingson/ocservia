import { client, read } from "./transport";
import { getWorkspaceId } from "./workspace";

export async function listNodes(cursor?: string, signal?: AbortSignal) {
  return read(
    client.GET("/nodes", {
      params: {
        header: { "X-Workspace-ID": await getWorkspaceId() },
        query: { page_size: 200, ...(cursor ? { cursor } : {}) },
      },
      signal: signal ?? null,
    }),
  );
}

export function getNode(nodeId: string, signal?: AbortSignal) {
  return read(
    client.GET("/nodes/{node_id}", {
      params: { path: { node_id: nodeId } },
      signal: signal ?? null,
    }),
  );
}

export function listNodeSessions(
  nodeId: string,
  cursor?: string,
  signal?: AbortSignal,
) {
  return read(
    client.GET("/nodes/{node_id}/sessions", {
      params: {
        path: { node_id: nodeId },
        query: { page_size: 200, ...(cursor ? { cursor } : {}) },
      },
      signal: signal ?? null,
    }),
  );
}

export function listNodeIpBans(nodeId: string, signal?: AbortSignal) {
  return read(
    client.GET("/nodes/{node_id}/ip-bans", {
      params: { path: { node_id: nodeId } },
      signal: signal ?? null,
    }),
  );
}

export function listNodeUserGroupState(nodeId: string, signal?: AbortSignal) {
  return read(
    client.GET("/nodes/{node_id}/user-group-state", {
      params: { path: { node_id: nodeId } },
      signal: signal ?? null,
    }),
  );
}
