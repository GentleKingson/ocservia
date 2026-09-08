import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const { Headers, Response } = globalThis;
const {
  Configuration,
  EventsApi,
  OperationsApi,
  PlatformApi,
} = require("../src/api/generated/dist/index.js");

const requests = [];
const configuration = new Configuration({
  accessToken: "test-token",
  fetchApi: async (input, init) => {
    const path = String(input);
    requests.push({
      path,
      authorization: new Headers(init?.headers).get("Authorization"),
      method: init?.method,
      body: init?.body,
      credentials: init?.credentials,
    });
    if (path.endsWith("/auth/methods")) {
      return new Response(JSON.stringify({ local: true, oidc: false }), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      });
    }
    if (path.endsWith("/auth/login")) {
      return new Response(null, { status: 204 });
    }
    if (path.endsWith("/events/stream")) {
      return new Response("", {
        status: 200,
        headers: { "Content-Type": "text/event-stream" },
      });
    }
    return new Response(
      JSON.stringify({ items: [], page: { has_more: false } }),
      { status: 200, headers: { "Content-Type": "application/json" } },
    );
  },
});
const operations = new OperationsApi(configuration);
const events = new EventsApi(configuration);
const platform = new PlatformApi(
  new Configuration({
    accessToken: "test-token",
    credentials: "include",
    fetchApi: configuration.fetchApi,
  }),
);

assert.deepEqual(await platform.getAuthMethods(), { local: true, oidc: false });
await platform.loginLocal({
  localLoginRequest: { username: "alice", password: "test-password" },
});
const localLogin = requests.find(({ path }) => path.endsWith("/auth/login"));
assert.equal(localLogin.method, "POST");
assert.equal(localLogin.authorization, null);
assert.equal(localLogin.credentials, "include");
assert.deepEqual(JSON.parse(localLogin.body), {
  username: "alice",
  password: "test-password",
});
assert.equal(
  requests.find(({ path }) => path.endsWith("/auth/methods")).authorization,
  null,
);

await operations.listOperations({
  xWorkspaceID: "0198f20e-0882-7000-8000-000000000001",
});
await events.listEvents({
  xWorkspaceID: "0198f20e-0882-7000-8000-000000000001",
});
await events.watchEvents({
  xWorkspaceID: "0198f20e-0882-7000-8000-000000000001",
});

for (const suffix of ["/operations", "/events", "/events/stream"]) {
  const request = requests.find(({ path }) => path.endsWith(suffix));
  assert.ok(request, `missing generated request for ${suffix}`);
  assert.equal(request.authorization, "Bearer test-token");
}
