import type { Workspace } from "./models";
import { client, read } from "./transport";

let managementWorkspace: Workspace | undefined;
let workspaceRequest: Promise<Workspace> | undefined;

export function getWorkspace(refresh = false): Promise<Workspace> {
  if (managementWorkspace && !refresh)
    return Promise.resolve(managementWorkspace);
  workspaceRequest ??= read(client.GET("/workspaces"))
    .then(({ items }) => {
      managementWorkspace = items[0];
      if (!managementWorkspace)
        throw new Error("No authorized workspace is available");
      return managementWorkspace;
    })
    .finally(() => {
      workspaceRequest = undefined;
    });
  return workspaceRequest;
}

export async function getWorkspaceId(): Promise<string> {
  return (await getWorkspace()).id;
}

export function cachedWorkspaceId(): string | undefined {
  return managementWorkspace?.id;
}
