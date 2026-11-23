import type { NodeObservedState } from "./models";
import { client, newIdempotencyKey, read } from "./transport";
import { getWorkspaceId } from "./workspace";

function controlledRequest(node: NodeObservedState, reason: string) {
  return {
    header: {
      "Idempotency-Key": newIdempotencyKey(),
      "If-Match": `"revision-${String(node.version)}"`,
    },
    body: { reason, expected_version: node.version, ttl_seconds: 60 },
  };
}

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

export async function disconnectSession(
  node: NodeObservedState,
  sessionId: string,
  reason: string,
  signal?: AbortSignal,
) {
  if (!node.bootId) throw new Error("Node boot identity is unavailable");
  const { header, body } = controlledRequest(node, reason);
  return read(
    client.POST("/nodes/{node_id}/sessions/{session_id}:disconnect", {
      params: { path: { node_id: node.id, session_id: sessionId }, header },
      body: { ...body, boot_id: node.bootId },
      signal: signal ?? null,
    }),
  );
}

export async function terminateSession(
  node: NodeObservedState,
  sessionId: string,
  reason: string,
  signal?: AbortSignal,
) {
  if (!node.bootId) throw new Error("Node boot identity is unavailable");
  const { header, body } = controlledRequest(node, reason);
  return read(
    client.POST("/nodes/{node_id}/sessions/{session_id}:terminate", {
      params: { path: { node_id: node.id, session_id: sessionId }, header },
      body: { ...body, boot_id: node.bootId },
      signal: signal ?? null,
    }),
  );
}

export function removeIpBan(
  node: NodeObservedState,
  ip: string,
  reason: string,
  signal?: AbortSignal,
) {
  const { header, body } = controlledRequest(node, reason);
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
  const { header, body } = controlledRequest(node, reason);
  return read(
    client.POST("/nodes/{node_id}/service:reload", {
      params: { path: { node_id: node.id }, header },
      body,
      signal: signal ?? null,
    }),
  );
}
