import assert from "node:assert/strict";
import console from "node:console";
import process from "node:process";
import { setTimeout as delay } from "node:timers/promises";
import { execFileSync, spawn, spawnSync } from "node:child_process";
import { chmod, mkdtemp, readFile, rm } from "node:fs/promises";
import { openSync, closeSync } from "node:fs";
import { request } from "node:https";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { preview } from "vite";

// Usage: node test/run-auth-browser.mjs [--gateway[=standalone|integrated]]
//   [--project=NAME]... [SPEC]...
// Projects default to desktop; extra specs run beside the authentication specs.
// --gateway serves the build (dist/) through the pinned production Caddy and
// the repository Caddyfile over HTTPS instead of Vite preview, so its headers
// (including the enforced CSP) apply; it needs Docker.
// --gateway=integrated instead puts the Integrated Edge (NGINX SNI routing with
// PROXY protocol) in front of the Integrated Caddyfile, published on
// 127.0.0.1:443 as https://controller.localhost, so that port must be free.
// Every required test must pass in every selected project; a missing browser
// fails the run instead of being reported as passed.
const args = process.argv.slice(2);
const projects = args
  .filter((arg) => arg.startsWith("--project="))
  .map((arg) => arg.slice("--project=".length));
if (projects.length === 0) projects.push("desktop");
const gatewayArg = args.find(
  (arg) => arg === "--gateway" || arg.startsWith("--gateway="),
);
const gateway = gatewayArg?.split("=")[1] ?? (gatewayArg ? "standalone" : "");
assert.ok(["", "standalone", "integrated"].includes(gateway), gatewayArg);
const extraSpecs = args.filter(
  (arg) => !arg.startsWith("--project=") && arg !== gatewayArg,
);
if (gateway) extraSpecs.push("csp-control.spec.ts");
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
if (gateway)
  required.add("gateway CSP blocks violations observably in the page");
const directory = await mkdtemp(join(tmpdir(), "ocservia-auth-browser-"));
let server;
let child;
const gatewayName = `ocservia-browser-gateway-${String(process.pid)}`;
const docker = (...command) =>
  execFileSync("docker", command, { encoding: "utf8" }).trim();
// Every gateway origin is loopback; resolving it here does not depend on the
// host resolving *.localhost the way the browsers do.
const loopback = (_hostname, options, callback) =>
  options.all
    ? callback(null, [{ address: "127.0.0.1", family: 4 }])
    : callback(null, "127.0.0.1", 4);
const get = (url) =>
  new Promise((done, fail) => {
    request(
      url,
      { rejectUnauthorized: false, lookup: loopback },
      (response) => {
        response.resume();
        done(response.statusCode);
      },
    )
      .on("error", fail)
      .end();
  });

// Production gateway image and Caddyfile; /api/* reaches a stub that answers
// 404 like Vite preview, so only the specs' own route mocks serve the API.
// Integrated adds the Edge image's NGINX with the repository template and
// entrypoint; Caddy accepts PROXY protocol only from Edge's fixed address.
async function startGateway() {
  const root = resolve("..");
  const image = async (dockerfile, name) =>
    (await readFile(join(root, "deploy/production", dockerfile), "utf8")).match(
      new RegExp(`^FROM (${name}:\\S+)(?: AS \\S+)?$`, "m"),
    )[1];
  const caddy = await image("gateway.Dockerfile", "caddy");
  const integrated = gateway === "integrated";
  const host = integrated ? "controller.localhost" : "localhost";
  execFileSync(
    "openssl",
    [
      "req",
      "-x509",
      "-newkey",
      "rsa:2048",
      "-nodes",
      "-days",
      "1",
      "-subj",
      `/CN=${host}`,
      "-addext",
      `subjectAltName=DNS:${host}`,
      "-keyout",
      join(directory, "tls.key"),
      "-out",
      join(directory, "tls.crt"),
    ],
    { stdio: "ignore" },
  );
  await chmod(directory, 0o755);
  await chmod(join(directory, "tls.key"), 0o444);
  const edgeIP = "198.18.95.2";
  docker(
    "network",
    "create",
    // Dynamic addresses stay above Edge's fixed one.
    ...(integrated
      ? ["--subnet", "198.18.95.0/24", "--ip-range", "198.18.95.128/25"]
      : []),
    gatewayName,
  );
  docker(
    "run",
    "-d",
    "--name",
    `${gatewayName}-upstream`,
    "--network",
    gatewayName,
    "--network-alias",
    "control-plane",
    caddy,
    "caddy",
    "respond",
    "--listen",
    ":8080",
    "--status",
    "404",
  );
  const config = integrated
    ? [
        "--network-alias",
        "gateway",
        "-e",
        `OCSERV_PUBLIC_HOST=${host}`,
        "-e",
        `OCSERV_EDGE_GATEWAY_IP=${edgeIP}`,
        "-v",
        `${join(root, "deploy/production/integrated/Caddyfile")}:/etc/caddy/Caddyfile:ro`,
        "-v",
        `${join(root, "deploy/production/integrated/site.caddy")}:/etc/caddy/site.caddy:ro`,
      ]
    : [
        "-p",
        "127.0.0.1::8443",
        "-v",
        `${join(root, "deploy/production/Caddyfile")}:/etc/caddy/Caddyfile:ro`,
      ];
  docker(
    "run",
    "-d",
    "--name",
    gatewayName,
    "--network",
    gatewayName,
    ...config,
    "-v",
    `${resolve("dist")}:/srv:ro`,
    "-v",
    `${join(directory, "tls.crt")}:/run/secrets/tls_certificate:ro`,
    "-v",
    `${join(directory, "tls.key")}:/run/secrets/tls_private_key:ro`,
    caddy,
  );
  let origin;
  if (integrated) {
    docker(
      "run",
      "-d",
      "--name",
      `${gatewayName}-edge`,
      "--network",
      gatewayName,
      "--ip",
      edgeIP,
      "--user",
      "65532:65532",
      "--read-only",
      "--tmpfs",
      "/tmp:size=8m,mode=0700,uid=65532,gid=65532",
      "-p",
      "127.0.0.1:443:8443",
      "-e",
      `OCSERV_PUBLIC_HOST=${host}`,
      "-e",
      "OCSERV_RELAY_PUBLIC_HOST=relay.localhost",
      "-v",
      `${join(root, "deploy/production/integrated/nginx.conf.template")}:/etc/nginx/ocservia.conf.template:ro`,
      "-v",
      `${join(root, "deploy/production/integrated/edge-entrypoint.sh")}:/usr/local/bin/ocservia-edge:ro`,
      "--entrypoint",
      "/bin/sh",
      await image("edge.Dockerfile", "nginx"),
      "/usr/local/bin/ocservia-edge",
    );
    origin = `https://${host}`;
  } else {
    const port = docker("port", gatewayName, "8443/tcp").split(":").at(-1);
    origin = `https://127.0.0.1:${port}`;
  }
  for (let attempt = 0; attempt < 60; attempt += 1) {
    if ((await get(`${origin}/`).catch(() => 0)) === 200) return origin;
    await delay(500);
  }
  throw new Error(`gateway did not serve ${origin}/`);
}
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
  let baseURL;
  if (gateway) {
    baseURL = await startGateway();
  } else {
    server = await preview({
      configFile: false,
      preview: { host: "127.0.0.1", port: 0 },
    });
    baseURL = `http://127.0.0.1:${server.httpServer.address().port}`;
  }
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
        "node-install.spec.ts",
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
          PLAYWRIGHT_BASE_URL: baseURL,
          PLAYWRIGHT_GATEWAY: gateway ? "1" : "",
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
  // The JSON report is deleted below, so every failure prints its errors
  // before the first assertion stops the run.
  for (const spec of specs(result.suites))
    for (const test of spec.tests)
      for (const { status, errors } of test.results)
        if (status !== "passed" && status !== "skipped")
          for (const error of errors ?? [])
            console.error(
              `[${test.projectName}] ${spec.title}\n${error.stack ?? error.message ?? ""}`,
            );
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
    `Browser regressions (${projects.join(", ")}${gateway ? `, ${gateway} gateway` : ""}): ${String(passed)} passed, ` +
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
  if (gateway) {
    for (const command of [
      [
        "rm",
        "-f",
        `${gatewayName}-edge`,
        gatewayName,
        `${gatewayName}-upstream`,
      ],
      ["network", "rm", gatewayName],
    ])
      spawnSync("docker", command, { stdio: "ignore" });
  }
  await rm(directory, { recursive: true, force: true });
  process.off("SIGINT", stop);
  process.off("SIGTERM", stop);
}
