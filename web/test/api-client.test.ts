import type { NodeObservedState } from "@ocservia/api-client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const alpha = { id: "workspace-a", name: "Alpha", slug: "alpha", version: 1 };
const beta = { id: "workspace-b", name: "Beta", slug: "beta", version: 2 };
const node = {
  id: "node/a",
  version: 7,
  bootId: "boot-a",
} as NodeObservedState;
const fetchMock = vi.fn<typeof fetch>();
const assign = vi.fn();
let storage: Map<string, string>;
let browser: EventTarget & {
  location: {
    pathname: string;
    search: string;
    hash: string;
    assign: typeof assign;
  };
};

// Exercise each public owner directly; production has no aggregate API barrel.
async function loadAPI() {
  return {
    ...(await import("../src/shared/session")),
    ...(await import("../src/api/workspace")),
    ...(await import("../src/api/configuration")),
    ...(await import("../src/api/certificates")),
    ...(await import("../src/api/platform")),
    ...(await import("../src/api/events")),
    ...(await import("../src/api/nodes")),
    ...(await import("../src/api/users")),
    ...(await import("../src/api/operations")),
    ...(await import("../src/api/agents")),
  };
}

function json(value: unknown, status = 200): Response {
  return new Response(JSON.stringify(value), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function request(index = -1) {
  const call = fetchMock.mock.calls.at(index);
  if (!call) throw new Error("Expected an API request");
  const [input, init] = call;
  return {
    path: requestPath(input),
    method: init?.method,
    credentials: init?.credentials,
    headers: new Headers(init?.headers),
    signal: init?.signal,
    body:
      typeof init?.body === "string"
        ? (JSON.parse(init.body) as Record<string, unknown>)
        : undefined,
  };
}

function requestPath(input: RequestInfo | URL): string {
  return typeof input === "string"
    ? input
    : input instanceof URL
      ? input.href
      : input.url;
}

beforeEach(() => {
  vi.resetModules();
  vi.stubEnv("DEV", true);
  vi.stubEnv("VITE_DEV_AUTH_TOKEN", undefined);
  fetchMock.mockReset();
  assign.mockReset();
  storage = new Map();
  browser = Object.assign(new EventTarget(), {
    location: {
      pathname: "/nodes",
      search: "?sort=name",
      hash: "#detail",
      assign,
    },
  });
  vi.stubGlobal("window", browser);
  vi.stubGlobal("sessionStorage", {
    getItem: (key: string) => storage.get(key) ?? null,
    setItem: (key: string, value: string) => storage.set(key, value),
    removeItem: (key: string) => storage.delete(key),
  });
  vi.stubGlobal("fetch", fetchMock);
  fetchMock.mockImplementation((input) =>
    Promise.resolve(
      json(
        requestPath(input).endsWith("/workspaces")
          ? { items: [alpha, beta] }
          : { items: [], page: { has_more: false } },
      ),
    ),
  );
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.unstubAllEnvs();
});

describe("API transport and session contracts", () => {
  it("keeps one 401 redirect and return path across generated and artifact requests", async () => {
    const api = await loadAPI();
    fetchMock.mockImplementation(() => Promise.resolve(json({}, 401)));
    await expect(api.getNode("node-a")).rejects.toMatchObject({
      response: { status: 401 },
    });
    await expect(
      api.downloadCertificateArtifact("artifact-a", "grant"),
    ).rejects.toThrow("Certificate artifact download failed");
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(assign).toHaveBeenCalledExactlyOnceWith("/login");
    expect(storage.get("ocservia.login.return-to")).toBe(
      "/nodes?sort=name#detail",
    );
    storage.set("ocservia.login.oidc-attempt", "started");
    expect(api.consumeLoginReturnPath()).toBe("/nodes?sort=name#detail");
    expect(storage.has("ocservia.login.return-to")).toBe(false);
    expect(storage.has("ocservia.login.oidc-attempt")).toBe(false);
  });

  it("preserves a valid earlier return path and stops an attempted OIDC loop", async () => {
    storage.set("ocservia.login.return-to", "/operations?state=failed");
    storage.set("ocservia.login.oidc-attempt", "started");
    fetchMock.mockResolvedValue(json({}, 401));
    const api = await loadAPI();
    await expect(api.getVersion()).rejects.toMatchObject({
      response: { status: 401 },
    });
    expect(assign).toHaveBeenCalledExactlyOnceWith("/login?auth=failed");
    expect(api.consumeLoginReturnPath()).toBe("/operations?state=failed");
  });

  it("does not redirect from login or on non-401 errors, and never retries", async () => {
    const api = await loadAPI();
    fetchMock.mockResolvedValueOnce(json({}, 403));
    await expect(api.getNode("node-a")).rejects.toMatchObject({
      response: { status: 403 },
    });
    browser.location.pathname = "/login";
    fetchMock.mockResolvedValueOnce(json({}, 401));
    await expect(api.getNode("node-a")).rejects.toMatchObject({
      response: { status: 401 },
    });
    expect(assign).not.toHaveBeenCalled();
    expect(storage.size).toBe(0);
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it("keeps artifact grant authorization separate from Workspace headers", async () => {
    vi.stubEnv("VITE_DEV_AUTH_TOKEN", "fictional-dev-token");
    const api = await loadAPI();
    fetchMock.mockResolvedValue(new Response("certificate-bytes"));
    expect(
      await (
        await api.downloadCertificateArtifact("artifact/a?b", "grant-token")
      ).text(),
    ).toBe("certificate-bytes");
    expect(request().path).toBe("/api/v1/artifacts/artifact%2Fa%3Fb");
    expect(request().credentials).toBe("same-origin");
    expect(Object.fromEntries(request().headers)).toEqual({
      authorization: "Bearer fictional-dev-token",
      "x-artifact-token": "grant-token",
    });
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it("does not include development credentials in production", async () => {
    vi.stubEnv("DEV", false);
    vi.stubEnv("VITE_DEV_AUTH_TOKEN", "fictional-dev-token");
    const api = await loadAPI();
    await api.getNode("node-a");
    expect(request().headers.has("Authorization")).toBe(false);
    expect(request().credentials).toBe("same-origin");
    await api.downloadCertificateArtifact("artifact-a", "grant-token");
    expect(request().headers.has("Authorization")).toBe(false);
  });
});

describe("Workspace authority and events", () => {
  it("rejects an empty authorized Workspace list", async () => {
    const api = await loadAPI();
    fetchMock.mockResolvedValueOnce(json({ items: [] }));
    await expect(api.getWorkspace()).rejects.toThrow(
      "No authorized workspace is available",
    );
    expect(api.workspaceContext()).toEqual({ id: undefined, generation: 0 });
  });

  it("coalesces discovery, remembers selection and advances generation only on changes", async () => {
    storage.set("ocservia.workspace-id", beta.id);
    const api = await loadAPI();
    expect(api.workspaceContext()).toEqual({ id: undefined, generation: 0 });
    const changed = vi.fn<(event: Event) => void>();
    browser.addEventListener(api.workspaceChangedEvent, changed);
    await Promise.all([
      api.getWorkspace(),
      api.listAuthorizedWorkspaces(),
      api.getWorkspace(),
    ]);
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(api.workspaceContext()).toEqual({ id: beta.id, generation: 1 });
    await api.selectWorkspace(beta.id);
    expect(changed).not.toHaveBeenCalled();
    await expect(api.selectWorkspace("unauthorized")).rejects.toThrow(
      "Workspace is not authorized",
    );
    await api.selectWorkspace(alpha.id);
    expect(api.workspaceContext()).toEqual({ id: alpha.id, generation: 2 });
    expect(storage.get("ocservia.workspace-id")).toBe(alpha.id);
    expect(changed).toHaveBeenCalledTimes(1);
    expect(changed.mock.calls[0]?.[0]).toMatchObject({ detail: alpha.id });
  });

  it("keeps session probes independent of cached Workspace authority and coalesces only in flight", async () => {
    const api = await loadAPI();
    await api.selectWorkspace(beta.id);
    await Promise.all([api.probeAuthentication(), api.probeAuthentication()]);
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(api.workspaceContext()).toEqual({ id: beta.id, generation: 2 });
    await api.probeAuthentication();
    expect(fetchMock).toHaveBeenCalledTimes(3);
    fetchMock.mockResolvedValueOnce(json({}, 503));
    await expect(api.listAuthorizedWorkspaces(true)).rejects.toMatchObject({
      response: { status: 503 },
    });
    expect(api.workspaceContext()).toEqual({ id: beta.id, generation: 2 });
    await api.listAuthorizedWorkspaces(true);
    expect(fetchMock).toHaveBeenCalledTimes(5);
  });

  it("scopes list requests and stream URLs without placing credentials in the URL", async () => {
    vi.stubEnv("VITE_DEV_AUTH_TOKEN", "fictional-dev-token");
    const api = await loadAPI();
    const signal = new AbortController().signal;
    await api.selectWorkspace(beta.id);
    await api.listNodes("cursor/a", signal);
    expect(request().path).toBe(
      "/api/v1/nodes?cursor=cursor%2Fa&page_size=200",
    );
    expect(request().headers.get("X-Workspace-ID")).toBe(beta.id);
    expect(request().headers.get("Authorization")).toBe(
      "Bearer fictional-dev-token",
    );
    expect(request().signal).toBe(signal);
    await api.listOperations(undefined, signal);
    expect(request().path).toBe("/api/v1/operations?page_size=200");
    expect(request().headers.get("X-Workspace-ID")).toBe(beta.id);
    await api.listEvents("event/a", signal, "desc");
    expect(request().headers.get("X-Workspace-ID")).toBe(beta.id);
    expect(request().signal).toBe(signal);
    const eventURL = new URL(request().path, "https://console.invalid");
    expect(Object.fromEntries(eventURL.searchParams)).toEqual({
      page_size: "200",
      after: "event/a",
      order: "desc",
    });
    expect(await api.eventStreamPath("event/a")).toBe(
      "/api/v1/events/stream?after=event%2Fa&workspace_id=workspace-b",
    );
    await api.selectWorkspace(alpha.id);
    expect(await api.eventStreamPath()).toBe(
      "/api/v1/events/stream?workspace_id=workspace-a",
    );
  });

  it.each([false, true])(
    "preserves the stream's development-only fallback (token=%s)",
    async (developmentToken) => {
      if (developmentToken)
        vi.stubEnv("VITE_DEV_AUTH_TOKEN", "fictional-dev-token");
      fetchMock.mockResolvedValue(json({}, 503));
      const api = await loadAPI();
      if (developmentToken)
        expect(await api.eventStreamPath("cursor")).toBe(
          "/api/v1/events/stream?after=cursor",
        );
      else
        await expect(api.eventStreamPath()).rejects.toMatchObject({
          response: { status: 503 },
        });
      expect(api.workspaceContext()).toEqual({ id: undefined, generation: 0 });
    },
  );
});

describe("domain request contracts", () => {
  it("preserves desired mutation fences, unique idempotency keys, body and AbortSignal", async () => {
    const api = await loadAPI();
    const signal = new AbortController().signal;
    await api.createUser(
      node.id,
      "alice",
      7,
      "sealed",
      "key-a",
      "requested",
      signal,
    );
    const firstKey = request().headers.get("Idempotency-Key");
    expect(firstKey).toMatch(/^[0-9a-f-]{36}$/);
    expect(request().path).toBe("/api/v1/nodes/node%2Fa/users");
    expect(request().method).toBe("POST");
    expect(request().headers.get("If-Match")).toBe('"revision-7"');
    expect(request().signal).toBe(signal);
    expect(request().body).toEqual({
      name: "alice",
      sealed_password: {
        version: 1,
        purpose: "user_password",
        key_id: "key-a",
        ciphertext: "sealed",
      },
      expected_version: 7,
      reason: "requested",
      ttl_seconds: 86400,
    });
    await api.applyGroup(
      node.id,
      "staff",
      8,
      ["alice", "alice", "bob"],
      "requested",
      signal,
    );
    expect(request().headers.get("Idempotency-Key")).not.toBe(firstKey);
    expect(request().headers.get("If-Match")).toBe('"revision-8"');
    expect(request().body).toEqual({
      members: ["alice", "bob"],
      reason: "requested",
      expected_version: 8,
      ttl_seconds: 86400,
    });
  });

  it("preserves controlled operation boot binding, TTL and approval headers", async () => {
    const api = await loadAPI();
    const signal = new AbortController().signal;
    const withoutBoot = { ...node };
    delete withoutBoot.bootId;
    await expect(
      api.disconnectSession(withoutBoot, "42", "requested", signal),
    ).rejects.toThrow("Node boot identity is unavailable");
    expect(fetchMock).not.toHaveBeenCalled();
    await api.disconnectSession(node, "42", "requested", signal);
    expect(request().headers.get("If-Match")).toBe('"revision-7"');
    expect(request().body).toEqual({
      reason: "requested",
      expected_version: 7,
      ttl_seconds: 60,
      boot_id: "boot-a",
    });
    expect(request().signal).toBe(signal);
    await api.reloadService(node, "requested", "approval-a", signal);
    expect(request().headers.get("X-Approval-ID")).toBe("approval-a");
    expect(request().body).toEqual({
      reason: "requested",
      expected_version: 7,
      ttl_seconds: 60,
    });
  });

  it("preserves configuration and certificate mutation policies without automatic retries", async () => {
    const api = await loadAPI();
    const signal = new AbortController().signal;
    await api.createConfigPlan(
      node.id,
      {
        expectedRevision: 7,
        template: { name: "test", directives: [] },
        ttlSeconds: 300,
        reason: "requested",
      },
      signal,
    );
    expect(request().headers.get("Idempotency-Key")).toBeTruthy();
    expect(request().body).toEqual({
      expected_revision: 7,
      template: { name: "test", directives: [] },
      ttl_seconds: 300,
      reason: "requested",
    });
    expect(request().signal).toBe(signal);
    await api.issueCertificate(
      "certificate-a",
      { approvalId: "approval-a", reason: "requested" },
      signal,
    );
    expect(request().headers.has("Idempotency-Key")).toBe(false);
    expect(request().body).toEqual({
      approval_id: "approval-a",
      reason: "requested",
    });
    expect(request().signal).toBe(signal);
    fetchMock.mockResolvedValueOnce(json({}, 409));
    await expect(
      api.applyConfigPlan(
        "plan-a",
        { approvalId: "approval-a", reason: "requested" },
        signal,
      ),
    ).rejects.toMatchObject({ response: { status: 409 } });
    expect(request().headers.get("Idempotency-Key")).toBeTruthy();
    expect(request().signal).toBe(signal);
    expect(fetchMock).toHaveBeenCalledTimes(3);
  });

  it("keeps rollout scope and trusted upgrade selection in their existing requests", async () => {
    const api = await loadAPI();
    const signal = new AbortController().signal;
    await api.upgradeNodeAgent(
      node,
      "1.2.3",
      "requested",
      "approval-a",
      signal,
    );
    expect(request().headers.get("If-Match")).toBe('"revision-7"');
    expect(request().body).toEqual({
      target_version: "1.2.3",
      approval_id: "approval-a",
      reason: "requested",
    });
    await api.selectWorkspace(beta.id);
    await api.createAgentRollout(
      "1.2.3",
      [node.id],
      1,
      "requested",
      "approval-a",
      signal,
    );
    expect(request().headers.get("X-Workspace-ID")).toBe(beta.id);
    expect(request().body).toEqual({
      target_version: "1.2.3",
      node_ids: [node.id],
      batch_size: 1,
      reason: "requested",
      approval_id: "approval-a",
    });
    expect(request().signal).toBe(signal);
    expect(request().headers.get("Idempotency-Key")).toBeTruthy();
  });
});
