import type { AgentRollout, AgentRolloutNode, Operation } from "@/api/models";

// Presentation only: the tone never decides polling or terminal state. A
// plain operation's unknown is still being recovered (warning), while the
// reconciled upgrade family ends on unknown (danger); keep them apart.
export type StateTone = "success" | "danger" | "warning" | "neutral";

const operationTones: Record<string, StateTone> = {
  succeeded: "success",
  failed: "danger",
  expired: "danger",
  drifted: "danger",
  rolled_back: "warning",
  unknown: "warning",
};

export function operationTone(operation: Operation): StateTone {
  if (operation.agent_upgrade_state && operation.state === "unknown")
    return "danger";
  return operationTones[operation.state] ?? "neutral";
}

const rolloutTones: Record<string, StateTone> = {
  succeeded: "success",
  failed: "danger",
  paused: "warning",
};

export function rolloutTone(state: AgentRollout["state"]): StateTone {
  return rolloutTones[state] ?? "neutral";
}

// Rollout nodes are agent upgrades, so their unknown is terminal.
const rolloutNodeTones: Record<string, StateTone> = {
  succeeded: "success",
  failed: "danger",
  rolled_back: "danger",
  unknown: "danger",
};

export function rolloutNodeTone(state: AgentRolloutNode["state"]): StateTone {
  return rolloutNodeTones[state] ?? "neutral";
}

// Audit results are free-form strings; intent records an attempt before
// its outcome.
const auditResultTones: Record<string, StateTone> = {
  succeeded: "success",
  failed: "danger",
};

export function auditResultTone(result: string): StateTone {
  return auditResultTones[result] ?? "neutral";
}
