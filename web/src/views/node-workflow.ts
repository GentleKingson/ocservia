import type { ArtifactGrant } from "@ocservia/api-client";
import { shallowReactive } from "vue";
import { workspaceContext, type WorkspaceContext } from "../api/workspace";

export interface NodeWorkflowContext {
  nodeId: string;
  workspace: WorkspaceContext;
  signal: AbortSignal;
}

export function createNodeWorkflow(
  nodeId: () => string | undefined,
  isOpen: () => boolean,
) {
  let current: NodeWorkflowContext | undefined;
  let controller: AbortController | undefined;
  function cancel(): void {
    controller?.abort();
    controller = undefined;
    current = undefined;
  }
  function begin(id: string): NodeWorkflowContext {
    cancel();
    controller = new AbortController();
    current = {
      nodeId: id,
      workspace: workspaceContext(),
      signal: controller.signal,
    };
    return current;
  }
  function isCurrent(context: NodeWorkflowContext): boolean {
    const workspace = workspaceContext();
    return (
      current === context &&
      !context.signal.aborted &&
      isOpen() &&
      nodeId() === context.nodeId &&
      workspace.id === context.workspace.id &&
      workspace.generation === context.workspace.generation
    );
  }
  return { begin, cancel, isCurrent };
}

export function waitForNodePoll(signal: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(finish, 500);
    function finish(): void {
      signal.removeEventListener("abort", abort);
      resolve();
    }
    function abort(): void {
      clearTimeout(timer);
      signal.removeEventListener("abort", abort);
      reject(new DOMException("Node workflow polling aborted", "AbortError"));
    }
    if (signal.aborted) abort();
    else signal.addEventListener("abort", abort, { once: true });
  });
}

interface Receipt {
  resourceId?: string;
  operationId?: string;
}

function receiptKey(
  context: NodeWorkflowContext,
  kind: "config" | "certificate",
): string {
  return `ocservia.node-workflow.${JSON.stringify([context.workspace.id, context.nodeId, kind])}`;
}

// Persistent receipts contain only identifiers. Reopening must read the server;
// a receipt is not cached authorization, a Plan result or an artifact secret.
export function readNodeReceipt(
  context: NodeWorkflowContext,
  kind: "config" | "certificate",
): Receipt {
  try {
    const value: unknown = JSON.parse(
      sessionStorage.getItem(receiptKey(context, kind)) ?? "{}",
    );
    const receipt: Receipt = {};
    if (typeof value !== "object" || value === null) return receipt;
    if ("resourceId" in value && typeof value.resourceId === "string")
      receipt.resourceId = value.resourceId;
    if ("operationId" in value && typeof value.operationId === "string")
      receipt.operationId = value.operationId;
    return receipt;
  } catch {
    return {};
  }
}

interface NodeMutation {
  key: string;
  settled: Promise<void>;
  resolve(): void;
}

// Ownership outlives the dialog, but only until the request acknowledges or
// fails. A reopened view waits for it instead of sending a new idempotency key.
const pendingMutations = new Map<string, NodeMutation>();

export function beginNodeMutation(
  context: NodeWorkflowContext,
  kind: "config" | "certificate",
): NodeMutation | undefined {
  const key = receiptKey(context, kind);
  if (pendingMutations.has(key)) return undefined;
  let resolve!: () => void;
  const settled = new Promise<void>((done) => {
    resolve = done;
  });
  const ticket = { key, settled, resolve };
  pendingMutations.set(key, ticket);
  return ticket;
}

export function finishNodeMutation(ticket: NodeMutation): void {
  if (pendingMutations.get(ticket.key) !== ticket) return;
  pendingMutations.delete(ticket.key);
  ticket.resolve();
}

export function waitForNodeMutation(
  context: NodeWorkflowContext,
  kind: "config" | "certificate",
): Promise<void> | undefined {
  const ticket = pendingMutations.get(receiptKey(context, kind));
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

export function rememberNodeReceipt(
  ticket: NodeMutation,
  receipt: Receipt,
): void {
  if (pendingMutations.get(ticket.key) !== ticket) return;
  try {
    sessionStorage.setItem(ticket.key, JSON.stringify(receipt));
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
  return JSON.stringify([context.workspace.id, context.nodeId, certificateId]);
}

function expireGrant(key: string): void {
  const cached = certificateGrants.get(key);
  if (!cached) return;
  clearTimeout(cached.timer);
  delete cached.grant.downloadToken;
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
  const ttl = Math.min(Date.parse(grant.expiresAt) - Date.now(), 10 * 60_000);
  if (
    !Number.isFinite(ttl) ||
    ttl <= 0 ||
    grant.operation.nodeId !== context.nodeId
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
  if (grant?.artifactId === artifactId) {
    delete grant.downloadToken;
    // Retain the password until expiry so a download completed after closing
    // the dialog is still usable when the user reopens it.
  }
}
