import type { NodeObservedState } from "@/api/models";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { requestOrigin, stubRequestOrigin } from "./request-origin";

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
async function loadApi() {
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
  stubRequestOrigin();
  vi.stubGlobal("fetch", async (input: Request) =>
    fetchMock(input.url.slice(requestOrigin.length), {
      method: input.method,
      credentials: input.credentials,
      headers: input.headers,
      signal: input.signal,
      body: input.body ? await input.text() : null,
    }),
  );
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
    const api = await loadApi();
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
    expect(api.consumeLoginReturnPath()).toBe("/nodes?sort=name#detail");
    expect(storage.has("ocservia.login.return-to")).toBe(false);
  });

  it("preserves a valid earlier return path across repeated 401 redirects", async () => {
    storage.set("ocservia.login.return-to", "/operations?state=failed");
    fetchMock.mockResolvedValue(json({}, 401));
    const api = await loadApi();
    await expect(api.getVersion()).rejects.toMatchObject({
      response: { status: 401 },
    });
    expect(assign).toHaveBeenCalledExactlyOnceWith("/login");
    expect(api.consumeLoginReturnPath()).toBe("/operations?state=failed");
  });

  it("does not redirect from login or on non-401 errors, and never retries", async () => {
    const api = await loadApi();
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

  it("returns a login rejection to the form without using development credentials", async () => {
    vi.stubEnv("VITE_DEV_AUTH_TOKEN", "fictional-dev-token");
    const api = await loadApi();
    fetchMock.mockResolvedValueOnce(json({}, 401));
    expect((await api.loginLocal("alice", "test-password")).status).toBe(401);
    expect(request().path).toBe("/api/v1/auth/login");
    expect(request().method).toBe("POST");
    expect(request().credentials).toBe("same-origin");
    expect(request().headers.has("Authorization")).toBe(false);
    expect(request().body).toEqual({
      username: "alice",
      password: "test-password",
    });
    expect(assign).not.toHaveBeenCalled();
  });

  it("keeps artifact grant authorization separate from Workspace headers", async () => {
    vi.stubEnv("VITE_DEV_AUTH_TOKEN", "fictional-dev-token");
    const api = await loadApi();
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
    const api = await loadApi();
    await api.getNode("node-a");
    expect(request().headers.has("Authorization")).toBe(false);
    expect(request().credentials).toBe("same-origin");
    await api.downloadCertificateArtifact("artifact-a", "grant-token");
    expect(request().headers.has("Authorization")).toBe(false);
  });
});

describe("Workspace authority and events", () => {
  it("rejects an empty authorized Workspace list", async () => {
    const api = await loadApi();
    fetchMock.mockResolvedValueOnce(json({ items: [] }));
    await expect(api.getWorkspace()).rejects.toThrow(
      "No authorized workspace is available",
    );
    expect(api.cachedWorkspaceId()).toBeUndefined();
  });

  it("updates the cached workspace after explicit discovery", async () => {
    const api = await loadApi();
    await api.getWorkspace();
    expect(api.cachedWorkspaceId()).toBe(alpha.id);
    fetchMock.mockResolvedValueOnce(json({ items: [beta] }));
    await expect(api.getWorkspace(true)).resolves.toEqual(beta);
    expect(api.cachedWorkspaceId()).toBe(beta.id);
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it("coalesces bootstrap discovery and retains the selected workspace", async () => {
    const api = await loadApi();
    expect(api.cachedWorkspaceId()).toBeUndefined();
    await Promise.all([
      api.getWorkspace(),
      api.getWorkspace(),
      api.getWorkspaceId(),
    ]);
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(api.cachedWorkspaceId()).toBe(alpha.id);
    await api.getWorkspace(true);
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(api.cachedWorkspaceId()).toBe(alpha.id);
  });

  it("keeps session probes independent of cached Workspace authority and coalesces only in flight", async () => {
    const api = await loadApi();
    await api.getWorkspace();
    await Promise.all([api.probeAuthentication(), api.probeAuthentication()]);
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(api.cachedWorkspaceId()).toBe(alpha.id);
    await api.probeAuthentication();
    expect(fetchMock).toHaveBeenCalledTimes(3);
    fetchMock.mockResolvedValueOnce(json({}, 503));
    await expect(api.getWorkspace(true)).rejects.toMatchObject({
      response: { status: 503 },
    });
    expect(api.cachedWorkspaceId()).toBe(alpha.id);
    await api.getWorkspace(true);
    expect(fetchMock).toHaveBeenCalledTimes(5);
  });

  it("scopes list requests and stream URLs without placing credentials in the URL", async () => {
    vi.stubEnv("VITE_DEV_AUTH_TOKEN", "fictional-dev-token");
    const api = await loadApi();
    const controller = new AbortController();
    const { signal } = controller;
    await api.listNodes("cursor/a", signal);
    expect(request().path).toBe(
      "/api/v1/nodes?page_size=200&cursor=cursor%2Fa",
    );
    expect(request().headers.get("X-Workspace-ID")).toBe(alpha.id);
    expect(request().headers.get("Authorization")).toBe(
      "Bearer fictional-dev-token",
    );
    expect(request().signal).toBeInstanceOf(AbortSignal);
    await api.listOperations(undefined, signal);
    expect(request().path).toBe("/api/v1/operations?page_size=200");
    expect(request().headers.get("X-Workspace-ID")).toBe(alpha.id);
    await api.listEvents("event/a", signal, "desc");
    expect(request().headers.get("X-Workspace-ID")).toBe(alpha.id);
    expect(request().signal).toBeInstanceOf(AbortSignal);
    const eventUrl = new URL(request().path, "https://console.invalid");
    expect(Object.fromEntries(eventUrl.searchParams)).toEqual({
      page_size: "200",
      after: "event/a",
      order: "desc",
    });
    controller.abort();
    expect(request().signal?.aborted).toBe(true);
    expect(await api.eventStreamPath("event/a")).toBe(
      "/api/v1/events/stream?after=event%2Fa&workspace_id=workspace-a",
    );
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
      const api = await loadApi();
      if (developmentToken)
        expect(await api.eventStreamPath("cursor")).toBe(
          "/api/v1/events/stream?after=cursor",
        );
      else
        await expect(api.eventStreamPath()).rejects.toMatchObject({
          response: { status: 503 },
        });
      expect(api.cachedWorkspaceId()).toBeUndefined();
    },
  );
});

// Storage can be disabled, full, or throw from the global getter itself.
function breakStorage(mode: "getter" | "getItem" | "setItem" | "removeItem") {
  if (mode === "getter") {
    Object.defineProperty(globalThis, "sessionStorage", {
      configurable: true,
      get() {
        throw new DOMException("denied", "SecurityError");
      },
    });
    return;
  }
  const fail = () => {
    throw new DOMException("denied", "SecurityError");
  };
  vi.stubGlobal("sessionStorage", {
    getItem: (key: string) => storage.get(key) ?? null,
    setItem: (key: string, value: string) => storage.set(key, value),
    removeItem: (key: string) => storage.delete(key),
    [mode]: fail,
  });
}

describe("optional browser storage failures", () => {
  it.each(["getter", "getItem", "setItem", "removeItem"] as const)(
    "still redirects once and returns the original 401 when %s throws",
    async (mode) => {
      breakStorage(mode);
      const api = await loadApi();
      fetchMock.mockImplementation(() => Promise.resolve(json({}, 401)));
      await expect(api.getNode("node-a")).rejects.toMatchObject({
        response: { status: 401 },
      });
      await expect(api.getVersion()).rejects.toMatchObject({
        response: { status: 401 },
      });
      expect(assign).toHaveBeenCalledTimes(1);
      expect(fetchMock).toHaveBeenCalledTimes(2);
      expect(() => api.consumeLoginReturnPath()).not.toThrow();
    },
  );
});

describe("domain request contracts", () => {
  it("preserves desired mutation fences, unique idempotency keys, body and AbortSignal", async () => {
    const api = await loadApi();
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
    expect(request().signal).toBeInstanceOf(AbortSignal);
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

  it("preserves controlled operation boot binding and TTL without approval headers", async () => {
    const api = await loadApi();
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
    expect(request().signal).toBeInstanceOf(AbortSignal);
    await api.reloadService(node, "requested", signal);
    expect(request().headers.get("X-Approval-ID")).toBeNull();
    expect(request().body).toEqual({
      reason: "requested",
      expected_version: 7,
      ttl_seconds: 60,
    });
  });

  it("preserves configuration and certificate mutation policies without automatic retries", async () => {
    const api = await loadApi();
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
    expect(request().signal).toBeInstanceOf(AbortSignal);
    await api.issueCertificate(
      "certificate-a",
      { reason: "requested" },
      signal,
    );
    expect(request().headers.has("Idempotency-Key")).toBe(false);
    expect(request().body).toEqual({
      reason: "requested",
    });
    expect(request().signal).toBeInstanceOf(AbortSignal);
    fetchMock.mockResolvedValueOnce(json({}, 409));
    await expect(
      api.applyConfigPlan("plan-a", { reason: "requested" }, signal),
    ).rejects.toMatchObject({ response: { status: 409 } });
    expect(request().headers.get("Idempotency-Key")).toBeTruthy();
    expect(request().signal).toBeInstanceOf(AbortSignal);
    expect(fetchMock).toHaveBeenCalledTimes(3);
  });

  it("keeps rollout scope and trusted upgrade selection in their existing requests", async () => {
    const api = await loadApi();
    const signal = new AbortController().signal;
    await api.upgradeNodeAgent(node, "1.2.3", "requested", signal);
    expect(request().headers.get("If-Match")).toBe('"revision-7"');
    expect(request().body).toEqual({
      target_version: "1.2.3",
      reason: "requested",
    });
    await api.createAgentRollout("1.2.3", [node.id], 1, "requested", signal);
    expect(request().headers.get("X-Workspace-ID")).toBe(alpha.id);
    expect(request().body).toEqual({
      target_version: "1.2.3",
      node_ids: [node.id],
      batch_size: 1,
      reason: "requested",
    });
    expect(request().signal).toBeInstanceOf(AbortSignal);
    expect(request().headers.get("Idempotency-Key")).toBeTruthy();
  });
});

describe("audit and rollout wire decoding", () => {
  it("decodes real audit fields without closing historical result codes", async () => {
    const api = await loadApi();
    await api.getWorkspace();
    fetchMock.mockResolvedValueOnce(
      json({
        items: [
          {
            id: "audit-a",
            occurred_at: "2026-09-10T04:31:00Z",
            actor_type: "user",
            actor_id: "admin",
            action: "node.approve",
            resource_type: "node",
            resource_id: null,
            node_id: null,
            request_id: "request-a",
            trace_id: null,
            command_id: null,
            approval_id: null,
            result: "historical-result",
            reason: null,
            error_type: null,
            previous_event_hash: "",
            event_hash: "abc",
          },
        ],
      }),
    );
    const page = await api.listAuditEvents();
    expect(page.items[0]).toMatchObject({
      actorId: "admin",
      requestId: "request-a",
      resourceId: null,
      previousEventHash: "",
      eventHash: "abc",
      result: "historical-result",
      occurredAt: "2026-09-10T04:31:00Z",
    });
  });

  it.each([200, 409])(
    "decodes audit verification HTTP %s exactly once",
    async (status) => {
      const api = await loadApi();
      await api.getWorkspace();
      fetchMock.mockClear();
      fetchMock.mockResolvedValueOnce(
        json(
          {
            workspace_id: alpha.id,
            events: 3,
            compacted_events: 1,
            valid: status === 200,
            checkpoint_valid: status === 200,
          },
          status,
        ),
      );
      expect(await api.verifyAuditChain()).toEqual({
        workspaceId: alpha.id,
        events: 3,
        compactedEvents: 1,
        valid: status === 200,
        checkpointValid: status === 200,
      });
      expect(fetchMock).toHaveBeenCalledTimes(1);
      expect(request().method).toBe("POST");
    },
  );

  it.each([
    { type: "about:blank", title: "Conflict", status: 409 },
    {
      workspace_id: beta.id,
      events: 0,
      compacted_events: 0,
      valid: false,
      checkpoint_valid: false,
    },
    {
      workspace_id: alpha.id,
      events: -1,
      compacted_events: 0,
      valid: false,
      checkpoint_valid: false,
    },
    {
      workspace_id: alpha.id,
      events: 0,
      compacted_events: 0,
      valid: "false",
      checkpoint_valid: false,
    },
  ])(
    "rejects malformed or foreign audit verification without resubmitting",
    async (body) => {
      const api = await loadApi();
      await api.getWorkspace();
      fetchMock.mockClear();
      fetchMock.mockResolvedValueOnce(json(body, 409));
      await expect(api.verifyAuditChain()).rejects.toThrow(
        "Invalid audit verification response",
      );
      expect(fetchMock).toHaveBeenCalledTimes(1);
    },
  );

  it("decodes known exclusions and preserves unfamiliar stored JSON on reads and resume", async () => {
    const api = await loadApi();
    await api.getWorkspace();
    const wire = {
      id: "rollout-a",
      workspace_id: alpha.id,
      created_at: "2026-09-10T04:34:00Z",
      updated_at: "2026-09-10T04:34:56.123456Z",
      excluded: [
        { node_id: "node-a", reason: "future_reason" },
        null,
        ["legacy"],
        { extra: true },
      ],
    };
    for (const load of [
      () => api.getAgentRollout("rollout-a"),
      () => api.resumeAgentRollout("rollout-a"),
      async () => (await api.listAgentRollouts()).rollouts[0],
    ]) {
      fetchMock.mockResolvedValueOnce(json({ ...wire, rollouts: [wire] }));
      const value = await load();
      expect(value).toBeDefined();
      if (!value) throw new Error("Missing rollout");
      expect(value.excluded).toEqual([
        { nodeId: "node-a", reason: "future_reason", raw: "" },
        { nodeId: "", reason: "", raw: "null" },
        { nodeId: "", reason: "", raw: '["legacy"]' },
        { nodeId: "", reason: "", raw: '{"extra":true}' },
      ]);
      expect(value.createdAt).toBe("2026-09-10T04:34:00Z");
      expect(value.updatedAt).toBe("2026-09-10T04:34:56.123456Z");
    }
  });
});
