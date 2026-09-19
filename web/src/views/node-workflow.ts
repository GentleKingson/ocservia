import { workspaceContext, type WorkspaceContext } from "../api/client";

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

// Only identifiers survive a dialog/unmount. Reopening must read the server;
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

export function rememberNodeReceipt(
  context: NodeWorkflowContext,
  kind: "config" | "certificate",
  receipt: Receipt,
): void {
  try {
    sessionStorage.setItem(receiptKey(context, kind), JSON.stringify(receipt));
  } catch {
    // Storage can be disabled; the accepted operation still exists server-side.
  }
}
