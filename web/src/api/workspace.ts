import { PlatformApi, type Workspace } from "@ocservia/api-client";
import { configuration } from "./transport";

const platform = new PlatformApi(configuration);
let managementWorkspace: Workspace | undefined;
let workspaceRequest: Promise<Workspace> | undefined;
let workspaceGeneration = 0;

export interface WorkspaceContext {
  id: string | undefined;
  generation: number;
}

export function sameWorkspaceContext(
  a: WorkspaceContext,
  b: WorkspaceContext,
): boolean {
  return a.id === b.id && a.generation === b.generation;
}

export function workspaceContext(): WorkspaceContext {
  return { id: managementWorkspace?.id, generation: workspaceGeneration };
}

export async function getWorkspace(refresh = false): Promise<Workspace> {
  if (managementWorkspace && !refresh) return managementWorkspace;
  workspaceRequest ??= platform
    .listAuthorizedWorkspaces()
    .then((page) => {
      const workspace = page.items[0];
      if (!workspace) {
        managementWorkspace = undefined;
        throw new Error("No authorized workspace is available");
      }
      if (managementWorkspace?.id !== workspace.id) workspaceGeneration += 1;
      managementWorkspace = workspace;
      return workspace;
    })
    .finally(() => {
      workspaceRequest = undefined;
    });
  return workspaceRequest;
}

export async function getWorkspaceId(): Promise<string> {
  return (await getWorkspace()).id;
}
