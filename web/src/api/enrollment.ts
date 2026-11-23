import { client, read } from "./transport";

export function createNodeBootstrapToken(
  workspaceId: string,
  environment: string,
  expectedNodeName: string,
  signal: AbortSignal,
) {
  return read(
    client.POST("/node-bootstrap-tokens", {
      params: { header: { "X-Workspace-ID": workspaceId } },
      body: {
        workspace_id: workspaceId,
        installation: true,
        environment,
        expected_node_name: expectedNodeName,
        ttl_seconds: 900,
        reason: "Bootstrap managed node",
      },
      signal,
      cache: "no-store",
    }),
  );
}
