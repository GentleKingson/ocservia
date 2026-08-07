import type { UserGroupResourceState } from "@ocservia/api-client";
import { describe, expect, it } from "vitest";

import { recoveryDialogKind } from "../src/shared/desired-recovery";

function resource(
  recoveryMutationKind?: UserGroupResourceState["recoveryMutationKind"],
): UserGroupResourceState {
  return {
    kind: recoveryMutationKind === "group_apply" ? "group" : "user",
    name: "alice",
    convergence: "drifted",
    recoveryRequired: true,
    ...(recoveryMutationKind ? { recoveryMutationKind } : {}),
  };
}

describe("desired state recovery actions", () => {
  it.each([
    ["user_create", "create"],
    ["user_disable", "disable"],
    ["user_enable", "enable"],
    ["user_password_rotate", "rotate"],
    ["group_apply", "group"],
  ] as const)("maps %s only to its same-kind retry", (mutation, dialog) => {
    expect(recoveryDialogKind(resource(mutation))).toBe(dialog);
  });

  it("does not masquerade manual reconciliation as another mutation", () => {
    expect(recoveryDialogKind(resource())).toBeUndefined();
  });
});
