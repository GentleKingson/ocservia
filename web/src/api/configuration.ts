import type { ConfigPlanRequest, ConfigApplyRequest } from "./models";
import { client, newIdempotencyKey, read } from "./transport";

export function createConfigPlan(
  nodeId: string,
  request: ConfigPlanRequest,
  signal?: AbortSignal,
) {
  const { expectedRevision, template, ttlSeconds, reason } = request;
  return read(
    client.POST("/nodes/{node_id}/config-plans", {
      params: {
        path: { node_id: nodeId },
        header: { "Idempotency-Key": newIdempotencyKey() },
      },
      body: {
        expected_revision: expectedRevision,
        template: {
          name: template.name,
          directives: template.directives.map(({ name, value, secretRef }) => ({
            name,
            ...(value === undefined ? {} : { value }),
            ...(secretRef
              ? { secret_ref: { secret_ref_id: secretRef.secretRefId } }
              : {}),
          })),
        },
        ttl_seconds: ttlSeconds,
        reason,
      },
      signal: signal ?? null,
    }),
  );
}

export function getConfigPlan(planId: string, signal?: AbortSignal) {
  return read(
    client.GET("/config-plans/{plan_id}", {
      params: { path: { plan_id: planId } },
      signal: signal ?? null,
    }),
  );
}

export function applyConfigPlan(
  planId: string,
  request: ConfigApplyRequest,
  signal?: AbortSignal,
) {
  return read(
    client.POST("/config-plans/{plan_id}/apply", {
      params: {
        path: { plan_id: planId },
        header: { "Idempotency-Key": newIdempotencyKey() },
      },
      body: request,
      signal: signal ?? null,
    }),
  );
}
