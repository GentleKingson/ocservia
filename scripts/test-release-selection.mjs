import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import { selectChecks, checkResults, checkBusinessResilience } from "./release-selection.mjs";

const selected = paths => Object.values(selectChecks(paths)).map(value => value.selected);
assert.deepEqual(selected(["docs/how-to/enroll-node.md", "README.md"]), [false, false]);
assert.deepEqual(selected(["web/src/App.vue"]), [true, false]);
for (const path of ["proto/command.proto", "toolchains.lock", "unknown/path", "docs/reference/support-policy.md",
  "docs/acceptance/g6-slo.yaml", ".github/workflows/release.yml", "scripts/release-selection.mjs"])
  assert.deepEqual(selected([path]), [true, true], path);
assert.deepEqual(selected(["scripts/test-g6-relay-proof.mjs"]), [false, true]);
assert.deepEqual(selected(["web/src/App.vue", "rust/crates/agent/src/main.rs"]), [true, true]);
assert.deepEqual(Object.values(selectChecks([], "diff failed")).map(value => value.selected), [true, true]);
const required = ["build", "security"];
for (const integration of [false, true]) for (const resilience of [false, true]) {
  const selection = { integration: { selected: integration }, resilience: { selected: resilience } };
  const owner = integration ? "integration" : "business-smoke";
  const results = {build: {result: "success"}, security: {result: "success"},
    integration: {result: integration ? "success" : "skipped"},
    "business-smoke": {result: integration ? "skipped" : "success"}};
  results[owner].outputs = {"resilience-result": resilience ? "PASS" : "SKIPPED"};
  checkResults(selection, results, required);
  for (const job of Object.keys(results)) {
    for (const state of ["failure", "cancelled", "skipped", "success", undefined]) {
      if (state === results[job].result) continue;
      assert.throws(() => checkResults(selection, { ...results, [job]: { result: state } }, required));
    }
    const missing = { ...results }; delete missing[job];
    assert.throws(() => checkResults(selection, missing, required));
  }
  for (const value of ["FAIL", "SKIPPED", "PASS", "success", undefined]) {
    if (value === results[owner].outputs["resilience-result"]) continue;
    assert.throws(() => checkResults(selection, {...results, [owner]: {...results[owner], outputs: {"resilience-result": value}}}, required));
  }
}
assert.throws(() => checkResults({}, {}, []));
const actual = { probe_status: "PASS", exit_code: 0, resilience_requested: true, resilience_result: "PASS",
  planned_topology: {native_systemd_node: true},
  resilience_scenarios: ["controller", "agent_privd", "transport", "database_api", "relay", "complete"].map(name => ({name: `resilience_${name}`, status: "PASS"})) };
assert.equal(checkBusinessResilience(actual, true), "PASS");
for (const changes of [{resilience_result: "SKIPPED"}, {resilience_result: undefined}, {resilience_requested: false},
  {probe_status: "FAIL"}, {exit_code: 130}, {resilience_scenarios: undefined},
  {resilience_scenarios: actual.resilience_scenarios.slice(1)}, {planned_topology: {native_systemd_node: false}}]) {
  assert.throws(() => checkBusinessResilience({...actual, ...changes}, true));
}
for (const requested of [false, true]) {
  const skipped = {...actual, resilience_requested: requested, resilience_result: "SKIPPED", resilience_scenarios: []};
  assert.equal(checkBusinessResilience(skipped, requested, true), "SKIPPED");
}
assert.equal(checkBusinessResilience({...actual, resilience_requested: false, resilience_result: "SKIPPED"}, false), "SKIPPED");
for (const file of ["scripts/release-business-api.py", "scripts/release-business-probe.sh", "docs/development/resilience.md"])
  assert.deepEqual(selected([file]), [true, true]);
const fixture = fs.mkdtempSync(path.join(os.tmpdir(), "release-selection-"));
try {
  const git = (...args) => execFileSync("git", args, { cwd: fixture, encoding: "utf8" }).trim();
  git("init", "-q");
  git("config", "user.name", "test");
  git("config", "user.email", "test@example.invalid");
  fs.mkdirSync(path.join(fixture, "scripts"));
  fs.writeFileSync(path.join(fixture, "scripts/release-selection.mjs"), "// migration present\n");
  git("add", "."); git("commit", "-qm", "baseline"); git("tag", "v1.0.0");
  const base = git("rev-parse", "HEAD");
  fs.mkdirSync(path.join(fixture, "web"));
  fs.writeFileSync(path.join(fixture, "web/change"), "first commit\n");
  git("add", "."); git("commit", "-qm", "integration change");
  fs.writeFileSync(path.join(fixture, "README.md"), "last commit\n");
  git("add", "."); git("commit", "-qm", "documentation only");
  const candidate = git("rev-parse", "HEAD");
  const bin = path.join(fixture, "bin"); fs.mkdirSync(bin);
  fs.writeFileSync(path.join(bin, "gh"), '#!/bin/sh\nprintf "%s" "$RELEASE_PAGES"\n', { mode: 0o755 });
  const output = path.join(fixture, "output");
  const env = { ...process.env, PATH: `${bin}:${process.env.PATH}`, GITHUB_SHA: candidate,
    GITHUB_REPOSITORY: "test/repo", GITHUB_OUTPUT: output,
    GITHUB_STEP_SUMMARY: path.join(fixture, "summary"),
    RELEASE_PAGES: JSON.stringify([[{ tag_name: "v1.0.0", published_at: "2026-01-01T00:00:00Z" }]]) };
  const run = () => {
    fs.writeFileSync(output, "");
    execFileSync(process.execPath, [fileURLToPath(new URL("./release-selection.mjs", import.meta.url))], { cwd: fixture, env, stdio: "pipe" });
    return JSON.parse(fs.readFileSync(output, "utf8").split("\n")[0].slice("selection=".length));
  };
  const cumulative = run();
  assert.equal(cumulative.base, base);
  assert.equal(cumulative.integration.selected, true);
  assert.equal(cumulative.resilience.selected, false);
  env.RELEASE_PAGES = "[]";
  assert.equal(run().resilience.selected, true);
  env.GITHUB_SHA = base;
  assert.throws(run);
} finally {
  fs.rmSync(fixture, { recursive: true, force: true });
}
console.log("Release selection and selected-job failure/cancellation/missing-result checks passed");
