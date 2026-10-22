import { EnrollmentApi } from "@ocservia/api-client";
import { configuration } from "./transport";

const enrollment = new EnrollmentApi(configuration);

// Capture the workspace at the UI boundary; never resolve a different selection
// while this non-idempotent request is in flight. No retries on unknown outcomes.
export function createNodeBootstrapToken(
  workspaceId: string,
  environment: string,
  expectedNodeName: string,
  signal: AbortSignal,
) {
  return enrollment.createNodeBootstrapToken(
    {
      nodeBootstrapTokenRequest: {
        workspaceId,
        environment,
        expectedNodeName,
        ttlSeconds: 900,
        reason: "Bootstrap managed node",
      },
    },
    ({ init }) => {
      const headers = new Headers(init.headers);
      headers.set("X-Workspace-ID", workspaceId);
      return Promise.resolve({ ...init, headers, signal, cache: "no-store" });
    },
  );
}
