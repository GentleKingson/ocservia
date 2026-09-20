import {
  ConfigurationApi,
  type ConfigPlan,
  type ConfigPlanRequest,
  type ConfigApplyRequest,
  type Operation,
} from "@ocservia/api-client";
import { configuration, newIdempotencyKey, requestInit } from "./transport";

const configPlans = new ConfigurationApi(configuration);

export async function createConfigPlan(
  nodeId: string,
  request: ConfigPlanRequest,
  signal?: AbortSignal,
): Promise<ConfigPlan> {
  return configPlans.createConfigPlan(
    { nodeId, idempotencyKey: newIdempotencyKey(), configPlanRequest: request },
    requestInit(signal),
  );
}

export async function getConfigPlan(
  planId: string,
  signal?: AbortSignal,
): Promise<ConfigPlan> {
  return configPlans.getConfigPlan({ planId }, requestInit(signal));
}

export async function applyConfigPlan(
  planId: string,
  request: ConfigApplyRequest,
  signal?: AbortSignal,
): Promise<Operation> {
  return configPlans.applyConfigPlan(
    {
      planId,
      idempotencyKey: newIdempotencyKey(),
      configApplyRequest: request,
    },
    requestInit(signal),
  );
}
