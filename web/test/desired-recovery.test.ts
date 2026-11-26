import type { UserGroupResourceState } from "@/api/models";
import { describe, expect, it } from "vitest";

import {
  recoveryDialogKind,
  resourceStatusKey,
} from "../src/shared/desired-recovery";

function resource(
  recoveryMutationKind?: UserGroupResourceState["recovery_mutation_kind"],
): UserGroupResourceState {
  return {
    kind: recoveryMutationKind === "group_apply" ? "group" : "user",
    name: "alice",
    convergence: "drifted",
    recovery_required: true,
    ...(recoveryMutationKind
      ? { recovery_mutation_kind: recoveryMutationKind }
      : {}),
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

describe("resource management status", () => {
  it("distinguishes an observed existing account from managed drift", () => {
    expect(
      resourceStatusKey({
        ...resource(),
        observed_revision: 1,
        observed_at: "2026-09-30T00:00:00Z",
      }),
    ).toBe("resourceUnmanaged");
    expect(resourceStatusKey({ ...resource(), desired_version: 1 })).toBe(
      "resourceNotObserved",
    );
    expect(
      resourceStatusKey({
        ...resource(),
        desired_version: 1,
        observed_revision: 0,
        observed_at: "2026-09-30T00:00:00Z",
      }),
    ).toBe("convergence_drifted");
  });
  it.each(["pending", "offline_pending"] as const)(
    "retains %s without an initial observation",
    (convergence) => {
      expect(
        resourceStatusKey({ ...resource(), desired_version: 1, convergence }),
      ).toBe("convergence_" + convergence);
    },
  );
});
