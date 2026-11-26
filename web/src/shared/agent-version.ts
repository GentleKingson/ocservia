import type { NodeObservedState } from "@/api/models";

export function agentVersionLabel(node: NodeObservedState): string {
  if (!node.agent_version) return "versionNotObserved";
  if (!node.recommended_agent_version) return "recommendationNotConfigured";
  if (!node.agent_version_state || node.agent_version_state === "unknown")
    return "versionNotComparable";
  return node.agent_version_state;
}
