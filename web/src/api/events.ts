import type { operations } from "./generated/schema";
import type { AuditVerification, Schemas } from "./models";
import { client, devAuthToken, read } from "./transport";
import { getWorkspaceId } from "./workspace";

export const platformEventsEvent = "ocservia:platform-events";

export async function eventStreamPath(after?: string): Promise<string> {
  const query = new URLSearchParams();
  if (after) query.set("after", after);
  try {
    query.set("workspace_id", await getWorkspaceId());
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
  pageSize = 200,
) {
  return read(
    client.GET("/events", {
      params: {
        header: { "X-Workspace-ID": await getWorkspaceId() },
        query: {
          page_size: pageSize,
          ...(after ? { after } : {}),
          ...(order ? { order } : {}),
        },
      },
      signal: signal ?? null,
    }),
  );
}

export async function listAuditEvents(signal?: AbortSignal) {
  return read(
    client.GET("/audit/events", {
      params: {
        header: { "X-Workspace-ID": await getWorkspaceId() },
        query: { page_size: 50 },
      },
      signal: signal ?? null,
    }),
  );
}

export async function verifyAuditChain(
  signal?: AbortSignal,
): Promise<AuditVerification> {
  const workspaceId = await getWorkspaceId();
  const result = await client.POST("/audit:verify", {
    params: { header: { "X-Workspace-ID": workspaceId } },
    signal: signal ?? null,
  });
  const verification = (result.data ?? result.error) as Partial<
    Schemas["AuditVerification"]
  > | null;
  if (
    !verification ||
    verification.workspace_id !== workspaceId ||
    typeof verification.valid !== "boolean" ||
    typeof verification.checkpoint_valid !== "boolean" ||
    ![verification.events, verification.compacted_events].every(
      (count) => Number.isSafeInteger(count) && Number(count) >= 0,
    )
  )
    throw new Error("Invalid audit verification response");
  return verification as Schemas["AuditVerification"];
}

export type ListVpnAuditRecordsRequest = NonNullable<
  operations["listVpnAuditRecords"]["parameters"]["query"]
>;

export async function listVpnAuditRecords(
  query: ListVpnAuditRecordsRequest,
  signal?: AbortSignal,
) {
  return read(
    client.GET("/audit/vpn", {
      params: { header: { "X-Workspace-ID": await getWorkspaceId() }, query },
      signal: signal ?? null,
    }),
  );
}
