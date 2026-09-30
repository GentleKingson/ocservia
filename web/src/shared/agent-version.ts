import type { NodeObservedState } from "@ocservia/api-client";

export function agentVersionLabel(node: NodeObservedState): string {
  if (!node.agentVersion) return "versionNotObserved";
  if (!node.recommendedAgentVersion) return "recommendationNotConfigured";
  if (!node.agentVersionState || node.agentVersionState === "unknown")
    return "versionNotComparable";
  return node.agentVersionState;
}
