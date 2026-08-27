import type { Operation } from "@ocservia/api-client";

// The reconciled upgrade lifecycle gets its own progress words: an
// acknowledged schedule is only the restart window, never success, and
// unknown is the conservative terminal outcome of that family.
const upgradeStateLabels: Record<string, string> = {
  queued: "upgradeStateQueued",
  dispatched: "upgradeStateScheduling",
  accepted: "upgradeStateWaitingRestart",
  running: "upgradeStateVerifying",
  succeeded: "upgradeStateSucceeded",
  failed: "upgradeStateFailed",
  rolled_back: "upgradeStateRolledBack",
  unknown: "upgradeStateUnknown",
};

export function operationStatusKey(operation: Operation): string {
  if (operation.agentUpgradeState)
    return (
      upgradeStateLabels[operation.state] ?? "operation_" + operation.state
    );
  return "operation_" + operation.state;
}
