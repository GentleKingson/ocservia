import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { requestOrigin, stubRequestOrigin } from "./request-origin";

let createNodeBootstrapToken: typeof import("../src/api/enrollment").createNodeBootstrapToken;

beforeEach(async () => {
  vi.resetModules();
  stubRequestOrigin();
  ({ createNodeBootstrapToken } = await import("../src/api/enrollment"));
});
afterEach(() => vi.unstubAllGlobals());
it("sends exactly one scoped POST with the generated JSON content type and body", async () => {
  const fetch = vi.fn<typeof globalThis.fetch>().mockResolvedValue(
    new Response(
      JSON.stringify({
        id: "token-id",
        token:
          "oit1_aHR0cHM6Ly9jb250cm9sbGVyLmV4YW1wbGUuY29t." + "a".repeat(43),
        expires_at: "2026-01-01T00:15:00Z",
      }),
      { status: 201 },
    ),
  );
  vi.stubGlobal("fetch", fetch);
  const controller = new AbortController();
  await createNodeBootstrapToken(
    "workspace-b",
    "production",
    "edge-1",
    controller.signal,
  );
  expect(fetch).toHaveBeenCalledTimes(1);
  const request = fetch.mock.calls[0]?.[0];
  if (!(request instanceof Request)) throw new Error("Missing request");
  expect(request.url).toBe(requestOrigin + "/api/v1/node-bootstrap-tokens");
  expect(request.method).toBe("POST");
  expect(request.headers.get("X-Workspace-ID")).toBe("workspace-b");
  expect(request.headers.get("Content-Type")).toBe("application/json");
  expect(request.credentials).toBe("same-origin");
  expect(request.cache).toBe("no-store");
  controller.abort();
  expect(request.signal.aborted).toBe(true);
  expect(await request.json()).toEqual({
    workspace_id: "workspace-b",
    installation: true,
    environment: "production",
    expected_node_name: "edge-1",
    ttl_seconds: 900,
    reason: "Bootstrap managed node",
  });
});
it.each([400, 403, 503, "network"])("never retries %s", async (status) => {
  const fetch = vi.fn<typeof globalThis.fetch>();
  if (status === "network") fetch.mockRejectedValue(new TypeError("network"));
  else fetch.mockResolvedValue(new Response("{}", { status: Number(status) }));
  vi.stubGlobal("fetch", fetch);
  await expect(
    createNodeBootstrapToken(
      "workspace-b",
      "production",
      "edge-1",
      new AbortController().signal,
    ),
  ).rejects.toThrow();
  expect(fetch).toHaveBeenCalledTimes(1);
});
