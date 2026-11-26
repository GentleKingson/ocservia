import type { NodeObservedState } from "./models";
import { client, fencedRequest, read } from "./transport";
import { getWorkspaceId } from "./workspace";

export async function listOperations(
  cursor?: string,
  signal?: AbortSignal,
  pageSize = 200,
) {
  return read(
    client.GET("/operations", {
      params: {
        header: { "X-Workspace-ID": await getWorkspaceId() },
        query: { page_size: pageSize, ...(cursor ? { cursor } : {}) },
      },
      signal: signal ?? null,
    }),
  );
}

export async function operationSummary(signal?: AbortSignal) {
  return read(
    client.GET("/operations/summary", {
      params: { header: { "X-Workspace-ID": await getWorkspaceId() } },
      signal: signal ?? null,
    }),
  );
}

export function getOperation(operationId: string, signal?: AbortSignal) {
  return read(
    client.GET("/operations/{operation_id}", {
      params: { path: { operation_id: operationId } },
      signal: signal ?? null,
    }),
  );
}

async function sessionAction(
  action: "disconnect" | "terminate",
  node: NodeObservedState,
  sessionId: string,
  reason: string,
  signal?: AbortSignal,
) {
  if (!node.boot_id) throw new Error("Node boot identity is unavailable");
  const { header, body } = fencedRequest(node.version, reason, 60);
  const options = {
    params: { path: { node_id: node.id, session_id: sessionId }, header },
    body: { ...body, boot_id: node.boot_id },
    signal: signal ?? null,
  };
  return read(
    action === "disconnect"
      ? client.POST(
          "/nodes/{node_id}/sessions/{session_id}:disconnect",
          options,
        )
      : client.POST(
          "/nodes/{node_id}/sessions/{session_id}:terminate",
          options,
        ),
  );
}

export function disconnectSession(
  node: NodeObservedState,
  sessionId: string,
  reason: string,
  signal?: AbortSignal,
) {
  return sessionAction("disconnect", node, sessionId, reason, signal);
}

export function terminateSession(
  node: NodeObservedState,
  sessionId: string,
  reason: string,
  signal?: AbortSignal,
) {
  return sessionAction("terminate", node, sessionId, reason, signal);
}

export function removeIpBan(
  node: NodeObservedState,
  ip: string,
  reason: string,
  signal?: AbortSignal,
) {
  const { header, body } = fencedRequest(node.version, reason, 60);
  return read(
    client.POST("/nodes/{node_id}/ip-bans/{ip}:remove", {
      params: { path: { node_id: node.id, ip }, header },
      body,
      signal: signal ?? null,
    }),
  );
}

export function reloadService(
  node: NodeObservedState,
  reason: string,
  signal?: AbortSignal,
) {
  const { header, body } = fencedRequest(node.version, reason, 60);
  return read(
    client.POST("/nodes/{node_id}/service:reload", {
      params: { path: { node_id: node.id }, header },
      body,
      signal: signal ?? null,
    }),
  );
}
