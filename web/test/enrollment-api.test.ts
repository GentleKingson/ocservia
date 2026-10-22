import { afterEach, expect, it, vi } from "vitest";
import { createNodeBootstrapToken } from "../src/api/enrollment";

afterEach(() => vi.unstubAllGlobals());
it("sends exactly one scoped POST with the generated JSON content type and body", async () => {
  const fetch = vi.fn<typeof globalThis.fetch>().mockResolvedValue(
    new Response(
      JSON.stringify({
        id: "token-id",
        token: "obt1_" + "a".repeat(43),
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
  const call = fetch.mock.calls[0];
  if (!call?.[1]) throw new Error("Missing request");
  const [url, init] = call;
  if (typeof init.body !== "string") throw new Error("Missing JSON request");
  expect(url).toBe("/api/v1/node-bootstrap-tokens");
  expect(init.method).toBe("POST");
  expect(new Headers(init.headers).get("X-Workspace-ID")).toBe("workspace-b");
  expect(new Headers(init.headers).get("Content-Type")).toBe(
    "application/json",
  );
  expect(init.credentials).toBe("same-origin");
  expect(init.cache).toBe("no-store");
  expect(init.signal).toBe(controller.signal);
  expect(JSON.parse(init.body)).toEqual({
    workspace_id: "workspace-b",
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
