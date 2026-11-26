import type { ArtifactGrant } from "@/api/models";
import { shallowReactive, type Ref } from "vue";
import { abortableDelay } from "../shared/abortable-delay";

export interface NodeWorkflowContext {
  nodeId: string;
  workspaceId: string | undefined;
  signal: AbortSignal;
}

export function createNodeWorkflow(
  nodeId: () => string | undefined,
  isOpen: () => boolean,
  workspaceId: () => string | undefined,
) {
  let current:
    { context: NodeWorkflowContext; controller: AbortController } | undefined;
  function cancel(): void {
    current?.controller.abort();
    current = undefined;
  }
  function begin(id: string): NodeWorkflowContext {
    cancel();
    const controller = new AbortController();
    const context = {
      nodeId: id,
      workspaceId: workspaceId(),
      signal: controller.signal,
    };
    current = { context, controller };
    return context;
  }
  function isCurrent(context: NodeWorkflowContext): boolean {
    return (
      current?.context === context && isOpen() && nodeId() === context.nodeId
    );
  }
  return { begin, cancel, isCurrent };
}

export function waitForNodePoll(signal: AbortSignal): Promise<void> {
  return abortableDelay(500, signal, "Node workflow polling aborted");
}

interface NodeWorkflowReference {
  resourceId?: string;
  operationId?: string;
}

function workflowReferenceKey(
  context: NodeWorkflowContext,
  kind: "config" | "certificate",
): string {
  return `ocservia.node-workflow.${JSON.stringify([context.workspaceId, context.nodeId, kind])}`;
}

// Persistent references contain only identifiers. Reopening must read the server;
// a reference is not cached authorization, a Plan result or an artifact secret.
export function readNodeWorkflowReference(
  context: NodeWorkflowContext,
  kind: "config" | "certificate",
): NodeWorkflowReference {
  try {
    const value: unknown = JSON.parse(
      sessionStorage.getItem(workflowReferenceKey(context, kind)) ?? "{}",
    );
    const reference: NodeWorkflowReference = {};
    if (typeof value !== "object" || value === null) return reference;
    if ("resourceId" in value && typeof value.resourceId === "string")
      reference.resourceId = value.resourceId;
    if ("operationId" in value && typeof value.operationId === "string")
      reference.operationId = value.operationId;
    return reference;
  } catch {
    return {};
  }
}

export interface NodeMutationTicket {
  key: string;
  settled: Promise<void>;
  resolve(): void;
}

// Ownership outlives the dialog, but only until the request acknowledges or
// fails. A reopened view waits for it instead of sending a new idempotency key.
const pendingMutations = new Map<string, NodeMutationTicket>();

export function beginNodeMutation(
  context: NodeWorkflowContext,
  kind: "config" | "certificate",
): NodeMutationTicket | undefined {
  const key = workflowReferenceKey(context, kind);
  if (pendingMutations.has(key)) return undefined;
  let resolve!: () => void;
  const settled = new Promise<void>((done) => {
    resolve = done;
  });
  const ticket = { key, settled, resolve };
  pendingMutations.set(key, ticket);
  return ticket;
}

export function finishNodeMutation(ticket: NodeMutationTicket): void {
  if (pendingMutations.get(ticket.key) !== ticket) return;
  pendingMutations.delete(ticket.key);
  ticket.resolve();
}

export function waitForNodeMutation(
  context: NodeWorkflowContext,
  kind: "config" | "certificate",
): Promise<void> | undefined {
  const ticket = pendingMutations.get(workflowReferenceKey(context, kind));
  if (!ticket) return undefined;
  return new Promise((resolve, reject) => {
    function finish(): void {
      context.signal.removeEventListener("abort", abort);
      resolve();
    }
    function abort(): void {
      context.signal.removeEventListener("abort", abort);
      reject(new DOMException("Node workflow detached", "AbortError"));
    }
    if (context.signal.aborted) abort();
    else context.signal.addEventListener("abort", abort, { once: true });
    void ticket.settled.then(finish);
  });
}

export function rememberNodeWorkflowReference(
  ticket: NodeMutationTicket,
  reference: NodeWorkflowReference,
): void {
  if (pendingMutations.get(ticket.key) !== ticket) return;
  try {
    sessionStorage.setItem(ticket.key, JSON.stringify(reference));
  } catch {
    // Storage can be disabled; the accepted operation still exists server-side.
  } finally {
    finishNodeMutation(ticket);
  }
}

interface CachedGrant {
  grant: ArtifactGrant;
  timer: ReturnType<typeof setTimeout>;
  expiresAt: number;
}
const certificateGrants = new Map<string, CachedGrant>();

function grantKey(context: NodeWorkflowContext, certificateId: string): string {
  return JSON.stringify([context.workspaceId, context.nodeId, certificateId]);
}

function expireGrant(key: string): void {
  const cached = certificateGrants.get(key);
  if (!cached) return;
  clearTimeout(cached.timer);
  delete cached.grant.download_token;
  delete cached.grant.password;
  certificateGrants.delete(key);
}

// These one-time credentials cannot be replayed. Keep them only in this SPA's
// memory until the server deadline (at most the Controller's ten-minute TTL).
export function rememberCertificateGrant(
  context: NodeWorkflowContext,
  certificateId: string,
  grant: ArtifactGrant,
): void {
  const ttl = Math.min(Date.parse(grant.expires_at) - Date.now(), 10 * 60_000);
  if (
    !Number.isFinite(ttl) ||
    ttl <= 0 ||
    grant.operation.node_id !== context.nodeId
  )
    return;
  const key = grantKey(context, certificateId);
  expireGrant(key);
  certificateGrants.set(key, {
    grant: shallowReactive(grant),
    timer: setTimeout(() => {
      expireGrant(key);
    }, ttl),
    expiresAt: Date.now() + ttl,
  });
}

export function readCertificateGrant(
  context: NodeWorkflowContext,
  certificateId: string,
): ArtifactGrant | undefined {
  const key = grantKey(context, certificateId);
  const cached = certificateGrants.get(key);
  if (cached && cached.expiresAt <= Date.now()) {
    expireGrant(key);
    return undefined;
  }
  return cached?.grant;
}

export function consumeCertificateGrant(
  context: NodeWorkflowContext,
  certificateId: string,
  artifactId: string,
): void {
  const grant = readCertificateGrant(context, certificateId);
  if (grant?.artifact_id === artifactId) {
    delete grant.download_token;
    // Retain the password until expiry so a download completed after closing
    // the dialog is still usable when the user reopens it.
  }
}

export interface NodeMutationOptions {
  workflow: ReturnType<typeof createNodeWorkflow>;
  context: NodeWorkflowContext | undefined;
  kind: "config" | "certificate";
  loading: Ref<boolean>;
  error: Ref<string>;
  fallbackError: () => string;
  run: (
    ticket: NodeMutationTicket,
    context: NodeWorkflowContext,
  ) => Promise<void>;
  onError?: (cause: unknown) => void;
}

export async function runNodeMutation({
  workflow,
  context,
  kind,
  loading,
  error,
  fallbackError,
  run,
  onError,
}: NodeMutationOptions): Promise<void> {
  if (!context || !workflow.isCurrent(context) || loading.value) return;
  const ticket = beginNodeMutation(context, kind);
  if (!ticket) return;
  loading.value = true;
  error.value = "";
  try {
    await run(ticket, context);
  } catch (cause) {
    onError?.(cause);
    if (!workflow.isCurrent(context)) return;
    error.value = (cause instanceof Error && cause.message) || fallbackError();
  } finally {
    finishNodeMutation(ticket);
    if (workflow.isCurrent(context)) loading.value = false;
  }
}

export async function pollNodeResource<T>(
  workflow: ReturnType<typeof createNodeWorkflow>,
  context: NodeWorkflowContext,
  initial: T,
  shouldContinue: (value: T) => boolean,
  fetchNext: (value: T, signal: AbortSignal) => Promise<T>,
  onUpdate: (value: T) => void,
  maxAttempts = 30,
): Promise<void> {
  if (!workflow.isCurrent(context)) return;
  let value = initial;
  onUpdate(value);
  for (
    let attempt = 0;
    attempt < maxAttempts && shouldContinue(value);
    attempt += 1
  ) {
    await waitForNodePoll(context.signal);
    if (!workflow.isCurrent(context)) return;
    value = await fetchNext(value, context.signal);
    if (!workflow.isCurrent(context)) return;
    onUpdate(value);
  }
}
