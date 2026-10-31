import {
  EventsApi,
  type AuditEventPage,
  type PlatformEventPage,
} from "@ocservia/api-client";
import { configuration, devAuthToken, requestInit } from "./transport";
import { getWorkspace, getWorkspaceId } from "./workspace";

const events = new EventsApi(configuration);
export const platformEventsEvent = "ocservia:platform-events";

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

export async function listEvents(
  after?: string,
  signal?: AbortSignal,
  order?: "asc" | "desc",
): Promise<PlatformEventPage> {
  const xWorkspaceID = await getWorkspaceId();
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

export async function listAuditEvents(
  signal?: AbortSignal,
): Promise<AuditEventPage> {
  const xWorkspaceID = await getWorkspaceId();
  return events.listAuditEvents(
    { xWorkspaceID, pageSize: 50 },
    requestInit(signal),
  );
}
