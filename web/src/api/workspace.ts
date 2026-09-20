import { PlatformApi, type Workspace } from "@ocservia/api-client";
import { configuration } from "./transport";

const platform = new PlatformApi(configuration);
const workspaceKey = "ocservia.workspace-id";
export const workspaceChangedEvent = "ocservia:workspace-changed";
let selectedWorkspace: Workspace | undefined;
let authorizedWorkspaces: Workspace[] | undefined;
let workspaceRequest: Promise<Workspace[]> | undefined;
let workspaceGeneration = 0;

export interface WorkspaceContext {
  id: string | undefined;
  generation: number;
}

function setSelectedWorkspace(workspace: Workspace | undefined): void {
  if (selectedWorkspace?.id !== workspace?.id) workspaceGeneration += 1;
  selectedWorkspace = workspace;
}

export function workspaceContext(): WorkspaceContext {
  return { id: selectedWorkspace?.id, generation: workspaceGeneration };
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

export async function workspaceID(): Promise<string> {
  return (await getWorkspace()).id;
}
