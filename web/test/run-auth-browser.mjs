import assert from "node:assert/strict";
import console from "node:console";
import process from "node:process";
import { spawn } from "node:child_process";
import { mkdtemp, readFile, rm } from "node:fs/promises";
import { openSync, closeSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { preview } from "vite";

// These existing browser regressions are not selected by `vitest run test`.
const required = new Set([
  "local-only login restores return path and initializes the shell without storing passwords",
  "combined login shows generic credential and rate-limit errors, and SSO uses GET",
  "OIDC-only redirects without a click",
  "unavailable methods fail closed and can be retried",
  "OIDC return followed by 401 stops automatic login and permits manual retry",
  "fixed failure marker prevents auto SSO without accepting external returnTo",
  "callback rejection is terminal and revisiting login offers manual retry",
  "fresh OIDC login returns to the requested console route",
  "switching workspace clears fleet state and reconnects its stream",
  "an expired SSE session starts OIDC login and restores the console",
  "a late response from the previous workspace cannot overwrite the current view",
  "rapid workspace changes leave only the final workspace stream active",
]);
const directory = await mkdtemp(join(tmpdir(), "ocservia-auth-browser-"));
let server;
let child;
let interrupted = false;
const stop = () => {
  interrupted = true;
  if (child?.pid) {
    try {
      process.kill(-child.pid, "SIGTERM");
    } catch (error) {
      if (error.code !== "ESRCH") throw error;
    }
  }
};
process.on("SIGINT", stop);
process.on("SIGTERM", stop);
try {
  server = await preview({
    configFile: false,
    preview: { host: "127.0.0.1", port: 0 },
  });
  assert.ok(!interrupted, "browser acceptance interrupted");
  const report = join(directory, "results.json");
  const fd = openSync(report, "w");
  try {
    child = spawn(
      process.execPath,
      [
        "node_modules/@playwright/test/cli.js",
        "test",
        "login.spec.ts",
        "auth-workspace.spec.ts",
        "--project=desktop",
        "--reporter=json",
        "--global-timeout=180000",
      ],
      {
        detached: true,
        stdio: ["ignore", fd, "inherit"],
        env: {
          ...process.env,
          PLAYWRIGHT_BASE_URL: `http://127.0.0.1:${server.httpServer.address().port}`,
          PLAYWRIGHT_OUTPUT_DIR: join(directory, "test-results"),
        },
      },
    );
  } finally {
    closeSync(fd);
  }
  const status = await new Promise((resolve, reject) => {
    child.on("error", reject);
    child.on("exit", (code) => resolve(code));
  });
  child = undefined;
  assert.ok(!interrupted, "browser acceptance interrupted");
  const result = JSON.parse(await readFile(report, "utf8"));
  const specs = (suites) =>
    suites.flatMap((suite) => [...suite.specs, ...specs(suite.suites ?? [])]);
  for (const spec of specs(result.suites)) {
    assert.ok(spec.tests.length > 0, `not run: ${spec.title}`);
    for (const test of spec.tests) {
      assert.equal(test.results.length, 1, `not run once: ${spec.title}`);
      assert.equal(test.results[0].status, "passed", spec.title);
      assert.equal(test.status, "expected", spec.title);
    }
    required.delete(spec.title);
  }
  assert.deepEqual([...required], [], "missing authentication browser tests");
  assert.equal(status, 0, JSON.stringify(result.errors));
  console.log("Authentication browser regressions: 12 passed, 0 skipped");
} finally {
  if (child?.pid) {
    const exited = new Promise((resolve) => child.once("close", resolve));
    stop();
    await exited;
  }
  if (server) {
    server.httpServer.closeAllConnections();
    await new Promise((resolve) => server.httpServer.close(resolve));
  }
  await rm(directory, { recursive: true, force: true });
  process.off("SIGINT", stop);
  process.off("SIGTERM", stop);
}
