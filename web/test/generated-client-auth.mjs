import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const { Headers, Response } = globalThis;
const {
  Configuration,
  OperationsApi,
} = require("../src/api/generated/dist/index.js");

let authorization = null;
const api = new OperationsApi(
  new Configuration({
    accessToken: "test-token",
    fetchApi: async (_input, init) => {
      authorization = new Headers(init?.headers).get("Authorization");
      return new Response(
        JSON.stringify({ items: [], page: { has_more: false } }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      );
    },
  }),
);

await api.listOperations({
  xWorkspaceID: "0198f20e-0882-7000-8000-000000000001",
});
assert.equal(authorization, "Bearer test-token");
