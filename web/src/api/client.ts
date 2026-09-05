import {
  Configuration,
  ConfigurationApi,
  CertificatesApi,
  DevelopmentApi,
  EventsApi,
  NodesApi,
  OperationsApi,
  PlatformApi,
  type Operation,
  type OperationPage,
  type OperationSummary,
  type PlatformEventPage,
  type Readiness,
  type BuildInfo,
  type NodeObservedState,
  type NodeIpBanPage,
  type NodePage,
  type NodeSessionPage,
  type UserGroupStatePage,
  type UserPolicy,
  type UserPolicyRequest,
  type SimulationScenario,
  type Workspace,
  type ConfigPlan,
  type ConfigPlanRequest,
  type ConfigApplyRequest,
  type ArtifactGrant,
  type Certificate,
  type CertificateIssueRequest,
  type CertificateP12Request,
  type CertificateRequest,
  type CertificateRevokeRequest,
  type AgentRollout,
  type AgentRolloutPage,
} from "@ocservia/api-client";

const devAuthToken = import.meta.env.DEV
  ? import.meta.env.VITE_DEV_AUTH_TOKEN
  : undefined;
const loginReturnKey = "ocservia.login.return-to";
const loginStartedKey = "ocservia.login.started-at";
const workspaceKey = "ocservia.workspace-id";
export const workspaceChangedEvent = "ocservia:workspace-changed";
export const platformEventsEvent = "ocservia:platform-events";

function newIdempotencyKey(): string {
  if (typeof globalThis.crypto.randomUUID === "function")
    return globalThis.crypto.randomUUID();
  const bytes = globalThis.crypto.getRandomValues(new Uint8Array(16));
  bytes[6] = ((bytes[6] ?? 0) & 0x0f) | 0x40;
  bytes[8] = ((bytes[8] ?? 0) & 0x3f) | 0x80;
  const hex = Array.from(bytes, (value) => value.toString(16).padStart(2, "0"));
  return `${hex.slice(0, 4).join("")}-${hex.slice(4, 6).join("")}-${hex.slice(6, 8).join("")}-${hex.slice(8, 10).join("")}-${hex.slice(10).join("")}`;
}

async function authenticatedFetch(
  input: RequestInfo | URL,
  init?: RequestInit,
): Promise<Response> {
  const response = await fetch(input, init);
  if (response.status === 401 && typeof window !== "undefined") {
    const started = Number(sessionStorage.getItem(loginStartedKey) ?? "0");
    if (!started || Date.now() - started > 60_000) {
      const returnTo = `${window.location.pathname}${window.location.search}${window.location.hash}`;
      if (returnTo.startsWith("/") && !returnTo.startsWith("//")) {
        sessionStorage.setItem(loginReturnKey, returnTo);
      }
      sessionStorage.setItem(loginStartedKey, String(Date.now()));
      window.location.assign("/api/v1/auth/login");
    }
  }
  return response;
}

const configuration = new Configuration({
  basePath: "/api/v1",
  fetchApi: authenticatedFetch,
  credentials: "same-origin",
  ...(devAuthToken ? { accessToken: devAuthToken } : {}),
});
const operations = new OperationsApi(configuration);
const platform = new PlatformApi(configuration);
const development = new DevelopmentApi(configuration);
const events = new EventsApi(configuration);
const nodes = new NodesApi(configuration);
const configPlans = new ConfigurationApi(configuration);
const certificates = new CertificatesApi(configuration);
let selectedWorkspace: Workspace | undefined;
let authorizedWorkspaces: Workspace[] | undefined;
let workspaceRequest: Promise<Workspace[]> | undefined;
let workspaceGeneration = 0;
let authenticationProbe: Promise<void> | undefined;

export interface WorkspaceContext {
  id: string | undefined;
  generation: number;
}

export function workspaceContext(): WorkspaceContext {
  return { id: selectedWorkspace?.id, generation: workspaceGeneration };
}

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

export async function createCertificate(
  nodeId: string,
  request: CertificateRequest,
  signal?: AbortSignal,
): Promise<Certificate> {
  return certificates.createCertificate(
    {
      nodeId,
      idempotencyKey: newIdempotencyKey(),
      certificateRequest: request,
    },
    requestInit(signal),
  );
}

export async function getCertificate(
  certificateId: string,
  signal?: AbortSignal,
): Promise<Certificate> {
  return certificates.getCertificate({ certificateId }, requestInit(signal));
}

export async function listNodeCertificates(
  nodeId: string,
  signal?: AbortSignal,
): Promise<Certificate[]> {
  const response = await certificates.listNodeCertificates(
    { nodeId },
    requestInit(signal),
  );
  return response.items;
}

export async function issueCertificate(
  certificateId: string,
  request: CertificateIssueRequest,
  signal?: AbortSignal,
): Promise<Certificate> {
  return certificates.issueCertificate(
    { certificateId, certificateIssueRequest: request },
    requestInit(signal),
  );
}

export async function createCertificateP12(
  certificateId: string,
  request: CertificateP12Request,
  signal?: AbortSignal,
): Promise<ArtifactGrant> {
  return certificates.createCertificateP12(
    {
      certificateId,
      idempotencyKey: newIdempotencyKey(),
      certificateP12Request: request,
    },
    requestInit(signal),
  );
}

export async function revokeCertificate(
  certificateId: string,
  request: CertificateRevokeRequest,
  signal?: AbortSignal,
): Promise<Operation> {
  return certificates.revokeCertificate(
    {
      certificateId,
      idempotencyKey: newIdempotencyKey(),
      certificateRevokeRequest: request,
    },
    requestInit(signal),
  );
}

export async function downloadCertificateArtifact(
  artifactId: string,
  token: string,
): Promise<Blob> {
  const response = await authenticatedFetch(
    `/api/v1/artifacts/${encodeURIComponent(artifactId)}`,
    {
      credentials: "same-origin",
      headers: {
        "X-Artifact-Token": token,
        ...(devAuthToken ? { Authorization: `Bearer ${devAuthToken}` } : {}),
      },
    },
  );
  if (!response.ok) throw new Error("Certificate artifact download failed");
  return response.blob();
}

function setSelectedWorkspace(workspace: Workspace | undefined): void {
  if (selectedWorkspace?.id !== workspace?.id) workspaceGeneration += 1;
  selectedWorkspace = workspace;
}

export async function listAuthorizedWorkspaces(
  refresh = false,
): Promise<Workspace[]> {
  if (authorizedWorkspaces && !refresh) return authorizedWorkspaces;
  workspaceRequest ??= platform
    .listAuthorizedWorkspaces()
    .then((page) => {
      authorizedWorkspaces = page.items;
      const remembered = sessionStorage.getItem(workspaceKey);
      setSelectedWorkspace(
        page.items.find((workspace) => workspace.id === remembered) ??
          page.items[0],
      );
      if (selectedWorkspace)
        sessionStorage.setItem(workspaceKey, selectedWorkspace.id);
      else sessionStorage.removeItem(workspaceKey);
      return page.items;
    })
    .finally(() => {
      workspaceRequest = undefined;
    });
  return workspaceRequest;
}

export async function getWorkspace(): Promise<Workspace> {
  if (selectedWorkspace) return selectedWorkspace;
  const page = await listAuthorizedWorkspaces();
  const remembered = sessionStorage.getItem(workspaceKey);
  const workspace =
    page.find((candidate) => candidate.id === remembered) ?? page[0];
  if (!workspace) throw new Error("No authorized workspace is available");
  setSelectedWorkspace(workspace);
  return workspace;
}

export async function selectWorkspace(workspaceId: string): Promise<Workspace> {
  const workspaces = await listAuthorizedWorkspaces();
  const workspace = workspaces.find(
    (candidate) => candidate.id === workspaceId,
  );
  if (!workspace) throw new Error("Workspace is not authorized");
  if (selectedWorkspace?.id === workspace.id) return workspace;
  setSelectedWorkspace(workspace);
  sessionStorage.setItem(workspaceKey, workspace.id);
  window.dispatchEvent(
    new CustomEvent(workspaceChangedEvent, { detail: workspace.id }),
  );
  return workspace;
}

export function consumeLoginReturnPath(): string | undefined {
  const value = sessionStorage.getItem(loginReturnKey) ?? undefined;
  sessionStorage.removeItem(loginReturnKey);
  sessionStorage.removeItem(loginStartedKey);
  return value?.startsWith("/") && !value.startsWith("//") ? value : undefined;
}

export async function probeAuthentication(): Promise<void> {
  authenticationProbe ??= platform
    .listAuthorizedWorkspaces()
    .then(() => undefined)
    .finally(() => {
      authenticationProbe = undefined;
    });
  return authenticationProbe;
}

function requestInit(signal?: AbortSignal): RequestInit | undefined {
  return signal ? { signal } : undefined;
}

export async function eventStreamPath(after?: string): Promise<string> {
  const query = new URLSearchParams();
  if (after) query.set("after", after);
  try {
    const workspace = await getWorkspace();
    query.set("workspace_id", workspace.id);
  } catch (error) {
    if (!devAuthToken) throw error;
  }
  const encoded = query.toString();
  return `/api/v1/events/stream${encoded ? `?${encoded}` : ""}`;
}

async function workspaceID(): Promise<string> {
  return (await getWorkspace()).id;
}

export async function listOperations(
  cursor?: string,
  signal?: AbortSignal,
): Promise<OperationPage> {
  const xWorkspaceID = await workspaceID();
  return operations.listOperations(
    cursor
      ? { xWorkspaceID, cursor, pageSize: 200 }
      : { xWorkspaceID, pageSize: 200 },
    requestInit(signal),
  );
}

export async function operationSummary(
  signal?: AbortSignal,
): Promise<OperationSummary> {
  const xWorkspaceID = await workspaceID();
  return operations.getOperationSummary({ xWorkspaceID }, requestInit(signal));
}

export async function getReadiness(): Promise<Readiness> {
  return platform.getReadiness();
}

export async function getVersion(): Promise<BuildInfo> {
  return platform.getVersion();
}

export async function createLocalSimulation(
  simulationScenario: SimulationScenario,
  signal?: AbortSignal,
): Promise<Operation> {
  return development.createLocalSimulation(
    { simulationScenario },
    requestInit(signal),
  );
}

export async function getOperation(
  operationId: string,
  signal?: AbortSignal,
): Promise<Operation> {
  return operations.getOperation({ operationId }, requestInit(signal));
}

export async function listEvents(
  after?: string,
  signal?: AbortSignal,
  order?: "asc" | "desc",
): Promise<PlatformEventPage> {
  const xWorkspaceID = await workspaceID();
  return events.listEvents(
    {
      xWorkspaceID,
      pageSize: 200,
      ...(after ? { after } : {}),
      ...(order ? { order } : {}),
    },
    requestInit(signal),
  );
}

export async function listNodes(
  cursor?: string,
  signal?: AbortSignal,
): Promise<NodePage> {
  const xWorkspaceID = await workspaceID();
  return nodes.listNodes(
    cursor
      ? { xWorkspaceID, cursor, pageSize: 200 }
      : { xWorkspaceID, pageSize: 200 },
    requestInit(signal),
  );
}

export async function getNode(
  nodeId: string,
  signal?: AbortSignal,
): Promise<NodeObservedState> {
  return nodes.getNode({ nodeId }, requestInit(signal));
}

export async function listNodeSessions(
  nodeId: string,
  cursor?: string,
  signal?: AbortSignal,
): Promise<NodeSessionPage> {
  return nodes.listNodeSessions(
    cursor ? { nodeId, cursor, pageSize: 200 } : { nodeId, pageSize: 200 },
    requestInit(signal),
  );
}

export async function listNodeIpBans(
  nodeId: string,
  signal?: AbortSignal,
): Promise<NodeIpBanPage> {
  return nodes.listNodeIpBans({ nodeId }, requestInit(signal));
}

export async function listNodeUserGroupState(
  nodeId: string,
  signal?: AbortSignal,
): Promise<UserGroupStatePage> {
  return nodes.listNodeUserGroupState({ nodeId }, requestInit(signal));
}

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

function controlledRequest(node: NodeObservedState, reason: string) {
  return {
    idempotencyKey: newIdempotencyKey(),
    ifMatch: `"revision-${String(node.version)}"`,
    controlledOperationRequest: {
      reason,
      expectedVersion: node.version,
      ttlSeconds: 60,
    },
  };
}

export async function disconnectSession(
  node: NodeObservedState,
  sessionId: string,
  reason: string,
  signal?: AbortSignal,
): Promise<Operation> {
  if (!node.bootId) throw new Error("Node boot identity is unavailable");
  const request = controlledRequest(node, reason);
  return operations.disconnectNodeSession(
    {
      nodeId: node.id,
      sessionId,
      ...request,
      controlledOperationRequest: {
        ...request.controlledOperationRequest,
        bootId: node.bootId,
      },
    },
    requestInit(signal),
  );
}

export async function terminateSession(
  node: NodeObservedState,
  sessionId: string,
  reason: string,
  signal?: AbortSignal,
): Promise<Operation> {
  if (!node.bootId) throw new Error("Node boot identity is unavailable");
  const request = controlledRequest(node, reason);
  return operations.terminateNodeSession(
    {
      nodeId: node.id,
      sessionId,
      ...request,
      controlledOperationRequest: {
        ...request.controlledOperationRequest,
        bootId: node.bootId,
      },
    },
    requestInit(signal),
  );
}

export async function removeIpBan(
  node: NodeObservedState,
  ip: string,
  reason: string,
  signal?: AbortSignal,
): Promise<Operation> {
  return operations.removeNodeIpBan(
    {
      nodeId: node.id,
      ip,
      ...controlledRequest(node, reason),
    },
    requestInit(signal),
  );
}

export async function reloadService(
  node: NodeObservedState,
  reason: string,
  approvalId: string,
  signal?: AbortSignal,
): Promise<Operation> {
  return operations.reloadNodeService(
    {
      nodeId: node.id,
      xApprovalID: approvalId,
      ...controlledRequest(node, reason),
    },
    requestInit(signal),
  );
}

// The browser only names the trusted release; the Controller resolves the
// package digest from its operator-provisioned catalog, and the reconciled
// terminal outcome (including the conservative unknown) arrives through the
// operation being tracked.
export async function upgradeNodeAgent(
  node: NodeObservedState,
  targetVersion: string,
  reason: string,
  approvalId: string,
  signal?: AbortSignal,
): Promise<Operation> {
  return operations.upgradeNodeAgent(
    {
      nodeId: node.id,
      idempotencyKey: newIdempotencyKey(),
      ifMatch: `"revision-${String(node.version)}"`,
      agentUpgradeRequest: { targetVersion, approvalId, reason },
    },
    requestInit(signal),
  );
}

// Fleet rollouts are orchestrated server-side: the browser only selects the
// target version, candidate nodes, and batch size, then tracks durable
// rollout state that survives browser closure and Controller restart.
export async function createAgentRollout(
  targetVersion: string,
  nodeIds: string[],
  batchSize: number,
  reason: string,
  approvalId: string,
  signal?: AbortSignal,
): Promise<AgentRollout> {
  return operations.createAgentRollout(
    {
      idempotencyKey: newIdempotencyKey(),
      xWorkspaceID: await workspaceID(),
      agentRolloutCreateRequest: {
        targetVersion,
        nodeIds,
        batchSize,
        reason,
        approvalId,
      },
    },
    requestInit(signal),
  );
}

export async function listAgentRollouts(
  limit?: number,
  signal?: AbortSignal,
): Promise<AgentRolloutPage> {
  const xWorkspaceID = await workspaceID();
  return operations.listAgentRollouts(
    limit === undefined ? { xWorkspaceID } : { xWorkspaceID, limit },
    requestInit(signal),
  );
}

export async function getAgentRollout(
  rolloutId: string,
  signal?: AbortSignal,
): Promise<AgentRollout> {
  return operations.getAgentRollout(
    {
      xWorkspaceID: await workspaceID(),
      rolloutId,
    },
    requestInit(signal),
  );
}

export async function resumeAgentRollout(
  rolloutId: string,
  signal?: AbortSignal,
): Promise<AgentRollout> {
  return operations.resumeAgentRollout(
    {
      idempotencyKey: newIdempotencyKey(),
      xWorkspaceID: await workspaceID(),
      rolloutId,
    },
    requestInit(signal),
  );
}
