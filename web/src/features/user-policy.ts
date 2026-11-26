import { ResponseError } from "@/api/transport";
import type { UserPolicy, UserPolicyRequest } from "@/api/models";

import { getUserPolicy, setUserPolicy } from "../api/users";

type QuotaUnit = "MiB" | "GiB";

export interface UserPolicyForm {
  period: "none" | "monthly" | "lifetime";
  direction: "rx" | "tx" | "rxtx";
  quotaValue: number;
  quotaUnit: QuotaUnit;
  // UTC wall-clock text for the input; the control itself carries no timezone.
  expiresAtUtcInput: string;
  version: number;
}

const unitBytes: Record<QuotaUnit, number> = {
  MiB: 1024 * 1024,
  GiB: 1024 * 1024 * 1024,
};

export function policyToForm(policy?: UserPolicy): UserPolicyForm {
  const unit: QuotaUnit =
    policy && policy.quota_bytes % unitBytes.GiB === 0 ? "GiB" : "MiB";
  return {
    period: policy?.quota_period ?? "none",
    direction: policy?.quota_direction ?? "rxtx",
    quotaValue: policy ? policy.quota_bytes / unitBytes[unit] : 0,
    quotaUnit: unit,
    expiresAtUtcInput:
      policy?.expires_at?.replace(/:00Z$/, "").replace(/Z$/, "") ?? "",
    version: policy?.version ?? 0,
  };
}

export function expiryInputType(expiry: string): "datetime-local" | "text" {
  return !expiry ||
    /^(?!0000)\d{4}-\d{2}-\d{2}T\d{2}:\d{2}(?::\d{2})?$/.test(expiry)
    ? "datetime-local"
    : "text";
}

export function formToRequest(
  form: UserPolicyForm,
  reason: string,
): UserPolicyRequest {
  const quotaBytes =
    form.period === "none"
      ? 0
      : Math.round(form.quotaValue * unitBytes[form.quotaUnit]);
  if (!Number.isSafeInteger(quotaBytes) || quotaBytes < 0)
    throw new Error("Quota is outside the supported byte range");
  if (expiryInputType(form.expiresAtUtcInput) === "text")
    throw new Error("Expiry must be a finite UTC date with whole seconds");
  const expiry =
    form.expiresAtUtcInput.length === 16
      ? `${form.expiresAtUtcInput}:00`
      : form.expiresAtUtcInput;
  const expiresAt = expiry ? new Date(`${expiry}Z`) : undefined;
  if (
    expiresAt &&
    (Number.isNaN(expiresAt.valueOf()) ||
      expiresAt.toISOString() !== `${expiry}.000Z`)
  )
    throw new Error("Expiry is invalid");
  return {
    quota_period: form.period,
    quota_direction: form.direction,
    quota_bytes: quotaBytes,
    ...(expiresAt ? { expires_at: `${expiry}Z` } : {}),
    expected_version: form.version,
    reason,
  };
}

export async function loadUserPolicy(
  nodeId: string,
  username: string,
  signal?: AbortSignal,
): Promise<UserPolicyForm> {
  try {
    return policyToForm(await getUserPolicy(nodeId, username, signal));
  } catch (error) {
    if (error instanceof ResponseError && error.response.status === 404)
      return policyToForm();
    throw error;
  }
}

export async function saveUserPolicy(
  nodeId: string,
  username: string,
  form: UserPolicyForm,
  reason: string,
  signal?: AbortSignal,
): Promise<UserPolicyForm> {
  const policy = await setUserPolicy(
    nodeId,
    username,
    formToRequest(form, reason),
    signal,
  );
  return policyToForm(policy);
}
