import {
  EventsApi,
  ResponseError,
  AuditVerificationFromJSON,
  type AuditVerification,
  type AuditEventPage,
  type ListVpnAuditRecordsRequest,
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

// 409 is a completed verification with negative evidence, not a Problem.
// Other failures remain errors, and this POST is never automatically retried.
export async function verifyAuditChain(
  signal?: AbortSignal,
): Promise<AuditVerification> {
  const xWorkspaceID = await getWorkspaceId();
  let value: unknown;
  try {
    value = await events.verifyAuditChain(
      { xWorkspaceID },
      requestInit(signal),
    );
  } catch (cause) {
    if (!(cause instanceof ResponseError) || cause.response.status !== 409)
      throw cause;
    const wire: unknown = await cause.response.json();
    if (!wire || typeof wire !== "object") throw cause;
    value = AuditVerificationFromJSON(wire);
  }
  if (
    !value ||
    typeof value !== "object" ||
    !("workspaceId" in value) ||
    value.workspaceId !== xWorkspaceID ||
    !("events" in value) ||
    !Number.isSafeInteger(value.events) ||
    Number(value.events) < 0 ||
    !("valid" in value) ||
    typeof value.valid !== "boolean" ||
    !("checkpointValid" in value) ||
    typeof value.checkpointValid !== "boolean" ||
    !("compactedEvents" in value) ||
    !Number.isSafeInteger(value.compactedEvents) ||
    Number(value.compactedEvents) < 0
  ) {
    throw new Error("Invalid audit verification response");
  }
  return value as AuditVerification;
}

export function listVpnAuditRecords(
  query: ListVpnAuditRecordsRequest,
  signal: AbortSignal,
) {
  return events.listVpnAuditRecords(query, requestInit(signal));
}
