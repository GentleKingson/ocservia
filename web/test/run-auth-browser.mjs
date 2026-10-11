import assert from "node:assert/strict";
import console from "node:console";
import process from "node:process";
import { spawn } from "node:child_process";
import { mkdtemp, readFile, rm } from "node:fs/promises";
import { openSync, closeSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { preview } from "vite";

// Usage: node test/run-auth-browser.mjs [--project=NAME]... [SPEC]...
// Projects default to desktop; extra specs run beside the authentication specs.
// Every required test must pass in every selected project; a missing browser
// fails the run instead of being reported as passed.
const args = process.argv.slice(2);
const projects = args
  .filter((arg) => arg.startsWith("--project="))
  .map((arg) => arg.slice("--project=".length));
if (projects.length === 0) projects.push("desktop");
const extraSpecs = args.filter((arg) => !arg.startsWith("--project="));
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
        ...extraSpecs,
        ...projects.map((project) => `--project=${project}`),
        "--reporter=json",
        `--global-timeout=${String(180_000 * projects.length * (1 + extraSpecs.length))}`,
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
  let passed = 0;
  const declaredSkips = [];
  const requiredProjects = new Map();
  for (const spec of specs(result.suites)) {
    assert.ok(spec.tests.length > 0, `not run: ${spec.title}`);
    for (const test of spec.tests) {
      const name = `[${test.projectName}] ${spec.title}`;
      assert.equal(test.results.length, 1, `not run once: ${name}`);
      // Only a spec's own conditional test.skip (e.g. mobile-only navigation)
      // may skip, and never an authentication regression.
      if (
        test.expectedStatus === "skipped" &&
        test.annotations.some(({ type }) => type === "skip") &&
        !required.has(spec.title)
      ) {
        assert.equal(test.status, "skipped", name);
        declaredSkips.push(name);
        continue;
      }
      assert.equal(test.results[0].status, "passed", name);
      assert.equal(test.status, "expected", name);
      passed += 1;
    }
    if (required.has(spec.title)) {
      const ran = requiredProjects.get(spec.title) ?? [];
      ran.push(...spec.tests.map((test) => test.projectName));
      requiredProjects.set(spec.title, ran);
    }
  }
  for (const title of required) {
    assert.deepEqual(
      (requiredProjects.get(title) ?? []).sort(),
      [...projects].sort(),
      `authentication browser test not run once per project: ${title}`,
    );
  }
  assert.equal(status, 0, JSON.stringify(result.errors));
  console.log(
    `Browser regressions (${projects.join(", ")}): ${String(passed)} passed, ` +
      `${String(declaredSkips.length)} declared skips${declaredSkips.length ? `: ${declaredSkips.join("; ")}` : ""}`,
  );
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
