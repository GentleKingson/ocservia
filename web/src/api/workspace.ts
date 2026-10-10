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

// The remembered ID is only a preference among server-authorized Workspaces;
// storage failures mean "no preference" and never block selection or events.
function rememberedWorkspaceID(): string | null {
  try {
    return sessionStorage.getItem(workspaceKey);
  } catch {
    return null;
  }
}

function rememberWorkspace(workspaceId: string | undefined): void {
  try {
    if (workspaceId) sessionStorage.setItem(workspaceKey, workspaceId);
    else sessionStorage.removeItem(workspaceKey);
  } catch {
    // The in-memory selection stays authoritative for this page.
  }
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
      const remembered = rememberedWorkspaceID();
      setSelectedWorkspace(
        page.items.find((workspace) => workspace.id === remembered) ??
          page.items[0],
      );
      rememberWorkspace(selectedWorkspace?.id);
      return page.items;
    })
    .finally(() => {
      workspaceRequest = undefined;
    });
  return workspaceRequest;
}

export async function getWorkspace(): Promise<Workspace> {
  if (!selectedWorkspace) await listAuthorizedWorkspaces();
  if (!selectedWorkspace)
    throw new Error("No authorized workspace is available");
  return selectedWorkspace;
}

export async function selectWorkspace(workspaceId: string): Promise<Workspace> {
  const workspaces = await listAuthorizedWorkspaces();
  const workspace = workspaces.find(
    (candidate) => candidate.id === workspaceId,
  );
  if (!workspace) throw new Error("Workspace is not authorized");
  if (selectedWorkspace?.id === workspace.id) return workspace;
  setSelectedWorkspace(workspace);
  rememberWorkspace(workspace.id);
  window.dispatchEvent(
    new CustomEvent(workspaceChangedEvent, { detail: workspace.id }),
  );
  return workspace;
}

export async function workspaceID(): Promise<string> {
  return (await getWorkspace()).id;
}
