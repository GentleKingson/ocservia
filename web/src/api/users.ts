import {
  OperationsApi,
  NodesApi,
  type Operation,
  type UserPolicy,
  type UserPolicyRequest,
} from "@ocservia/api-client";
import { configuration, newIdempotencyKey, requestInit } from "./transport";

const operations = new OperationsApi(configuration);
const nodes = new NodesApi(configuration);

function desiredRequest(version: number, reason: string) {
  return {
    idempotencyKey: newIdempotencyKey(),
    ifMatch: `"revision-${String(version)}"`,
    reason,
    expectedVersion: version,
    ttlSeconds: 86400,
  };
}

export async function createUser(
  nodeId: string,
  name: string,
  version: number,
  sealedPassword: string,
  secretKeyId: string,
  reason: string,
  signal?: AbortSignal,
): Promise<Operation> {
  const request = desiredRequest(version, reason);
  return operations.createNodeUser(
    {
      nodeId,
      idempotencyKey: request.idempotencyKey,
      ifMatch: request.ifMatch,
      userCreateRequest: {
        name,
        sealedPassword: {
          version: 1,
          purpose: "user_password",
          keyId: secretKeyId,
          ciphertext: sealedPassword,
        },
        reason: request.reason,
        expectedVersion: request.expectedVersion,
        ttlSeconds: request.ttlSeconds,
      },
    },
    requestInit(signal),
  );
}

export async function disableUser(
  nodeId: string,
  username: string,
  version: number,
  reason: string,
  signal?: AbortSignal,
): Promise<Operation> {
  const request = desiredRequest(version, reason);
  return operations.disableNodeUser(
    {
      nodeId,
      username,
      idempotencyKey: request.idempotencyKey,
      ifMatch: request.ifMatch,
      desiredMutationRequest: {
        reason: request.reason,
        expectedVersion: version,
        ttlSeconds: request.ttlSeconds,
      },
    },
    requestInit(signal),
  );
}

export async function enableUser(
  nodeId: string,
  username: string,
  version: number,
  reason: string,
  signal?: AbortSignal,
): Promise<Operation> {
  const request = desiredRequest(version, reason);
  return operations.enableNodeUser(
    {
      nodeId,
      username,
      idempotencyKey: request.idempotencyKey,
      ifMatch: request.ifMatch,
      desiredMutationRequest: {
        reason: request.reason,
        expectedVersion: version,
        ttlSeconds: request.ttlSeconds,
      },
    },
    requestInit(signal),
  );
}

export async function rotateUserPassword(
  nodeId: string,
  username: string,
  version: number,
  sealedPassword: string,
  secretKeyId: string,
  reason: string,
  signal?: AbortSignal,
): Promise<Operation> {
  const request = desiredRequest(version, reason);
  return operations.rotateNodeUserPassword(
    {
      nodeId,
      username,
      idempotencyKey: request.idempotencyKey,
      ifMatch: request.ifMatch,
      passwordRotateRequest: {
        sealedPassword: {
          version: 1,
          purpose: "user_password",
          keyId: secretKeyId,
          ciphertext: sealedPassword,
        },
        reason: request.reason,
        expectedVersion: version,
        ttlSeconds: request.ttlSeconds,
      },
    },
    requestInit(signal),
  );
}

export async function applyGroup(
  nodeId: string,
  groupName: string,
  version: number,
  members: string[],
  reason: string,
  signal?: AbortSignal,
): Promise<Operation> {
  const request = desiredRequest(version, reason);
  return operations.applyNodeGroup(
    {
      nodeId,
      groupName,
      idempotencyKey: request.idempotencyKey,
      ifMatch: request.ifMatch,
      groupApplyRequest: {
        members: new Set(members),
        reason: request.reason,
        expectedVersion: version,
        ttlSeconds: request.ttlSeconds,
      },
    },
    requestInit(signal),
  );
}

export async function getUserPolicy(
  nodeId: string,
  username: string,
  signal?: AbortSignal,
): Promise<UserPolicy> {
  return nodes.getNodeUserPolicy({ nodeId, username }, requestInit(signal));
}

export async function setUserPolicy(
  nodeId: string,
  username: string,
  policy: UserPolicyRequest,
  signal?: AbortSignal,
): Promise<UserPolicy> {
  return operations.setNodeUserPolicy(
    {
      nodeId,
      username,
      idempotencyKey: newIdempotencyKey(),
      userPolicyRequest: policy,
    },
    requestInit(signal),
  );
}
