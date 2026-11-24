import type { UserPolicyRequest } from "./models";
import { client, newIdempotencyKey, read } from "./transport";

export function getUserPasswordSealingKey(
  nodeId: string,
  signal?: AbortSignal,
) {
  return read(
    client.GET("/nodes/{node_id}/user-password-sealing-key", {
      params: { path: { node_id: nodeId } },
      signal: signal ?? null,
    }),
  );
}

function desiredRequest(version: number, reason: string) {
  return {
    header: {
      "Idempotency-Key": newIdempotencyKey(),
      "If-Match": `"revision-${String(version)}"`,
    },
    body: { reason, expected_version: version, ttl_seconds: 86400 },
  };
}

function passwordEnvelope(ciphertext: string, keyId: string) {
  return {
    version: 1,
    purpose: "user_password",
    key_id: keyId,
    ciphertext,
  } as const;
}

export function createUser(
  nodeId: string,
  name: string,
  version: number,
  sealedPassword: string,
  secretKeyId: string,
  reason: string,
  signal?: AbortSignal,
) {
  const { header, body } = desiredRequest(version, reason);
  return read(
    client.POST("/nodes/{node_id}/users", {
      params: { path: { node_id: nodeId }, header },
      body: {
        ...body,
        name,
        sealed_password: passwordEnvelope(sealedPassword, secretKeyId),
      },
      signal: signal ?? null,
    }),
  );
}

export function disableUser(
  nodeId: string,
  username: string,
  version: number,
  reason: string,
  signal?: AbortSignal,
) {
  const { header, body } = desiredRequest(version, reason);
  return read(
    client.POST("/nodes/{node_id}/users/{username}:disable", {
      params: { path: { node_id: nodeId, username }, header },
      body,
      signal: signal ?? null,
    }),
  );
}

export function enableUser(
  nodeId: string,
  username: string,
  version: number,
  reason: string,
  signal?: AbortSignal,
) {
  const { header, body } = desiredRequest(version, reason);
  return read(
    client.POST("/nodes/{node_id}/users/{username}:enable", {
      params: { path: { node_id: nodeId, username }, header },
      body,
      signal: signal ?? null,
    }),
  );
}

export function rotateUserPassword(
  nodeId: string,
  username: string,
  version: number,
  sealedPassword: string,
  secretKeyId: string,
  reason: string,
  signal?: AbortSignal,
) {
  const { header, body } = desiredRequest(version, reason);
  return read(
    client.POST("/nodes/{node_id}/users/{username}:rotate-password", {
      params: { path: { node_id: nodeId, username }, header },
      body: {
        ...body,
        sealed_password: passwordEnvelope(sealedPassword, secretKeyId),
      },
      signal: signal ?? null,
    }),
  );
}

export function applyGroup(
  nodeId: string,
  groupName: string,
  version: number,
  members: string[],
  reason: string,
  signal?: AbortSignal,
) {
  const { header, body } = desiredRequest(version, reason);
  return read(
    client.PUT("/nodes/{node_id}/groups/{group_name}", {
      params: { path: { node_id: nodeId, group_name: groupName }, header },
      body: { ...body, members: [...new Set(members)] },
      signal: signal ?? null,
    }),
  );
}

export function getUserPolicy(
  nodeId: string,
  username: string,
  signal?: AbortSignal,
) {
  return read(
    client.GET("/nodes/{node_id}/users/{username}/policy", {
      params: { path: { node_id: nodeId, username } },
      signal: signal ?? null,
    }),
  );
}

export function setUserPolicy(
  nodeId: string,
  username: string,
  policy: UserPolicyRequest,
  signal?: AbortSignal,
) {
  return read(
    client.PUT("/nodes/{node_id}/users/{username}/policy", {
      params: {
        path: { node_id: nodeId, username },
        header: { "Idempotency-Key": newIdempotencyKey() },
      },
      body: {
        quota_period: policy.quotaPeriod,
        quota_direction: policy.quotaDirection,
        quota_bytes: policy.quotaBytes,
        expected_version: policy.expectedVersion,
        reason: policy.reason,
        ...(policy.expiresAt ? { expires_at: policy.expiresAt } : {}),
      },
      signal: signal ?? null,
    }),
  );
}
