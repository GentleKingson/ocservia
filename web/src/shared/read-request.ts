import {
  sameWorkspaceContext,
  workspaceContext,
  type WorkspaceContext,
} from "../api/workspace";

// One owner per independent read channel. Data, lifecycle and retries belong
// to the caller. Invalidation also fences transports that ignore cancellation.
export function createReadRequest() {
  let controller: AbortController | undefined;
  let revision = 0;
  function cancel(): void {
    revision += 1;
    controller?.abort();
    controller = undefined;
  }
  return {
    cancel,
    get revision() {
      return revision;
    },
    get pending() {
      return controller !== undefined;
    },
    begin() {
      cancel();
      const request = new AbortController();
      controller = request;
      const ticket = revision;
      const current = (context?: WorkspaceContext): boolean =>
        ticket === revision &&
        !request.signal.aborted &&
        (!context || sameWorkspaceContext(workspaceContext(), context));
      return {
        signal: request.signal,
        current,
        finish(): boolean {
          if (!current()) return false;
          controller = undefined;
          return true;
        },
      };
    },
  };
}
