import type { UserGroupResourceState } from "@ocservia/api-client";

export type DesiredDialogKind =
  "create" | "disable" | "enable" | "rotate" | "group";

export function recoveryDialogKind(
  resource: UserGroupResourceState,
): DesiredDialogKind | undefined {
  switch (resource.recoveryMutationKind) {
    case "user_create":
      return "create";
    case "user_disable":
      return "disable";
    case "user_enable":
      return "enable";
    case "user_password_rotate":
      return "rotate";
    case "group_apply":
      return "group";
    default:
      return undefined;
  }
}

export function resourceStatusKey(resource: UserGroupResourceState): string {
  if (resource.desiredVersion === undefined) return "resourceUnmanaged";
  if (
    resource.convergence === "pending" ||
    resource.convergence === "offline_pending"
  )
    return "convergence_" + resource.convergence;
  if (resource.observedRevision === undefined || !resource.observedAt)
    return "resourceNotObserved";
  return "convergence_" + resource.convergence;
}
