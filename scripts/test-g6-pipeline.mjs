import assert from "node:assert/strict";
import { mkdtempSync, mkdirSync, readFileSync, rmSync, writeFileSync, symlinkSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";
import { assemble, runtimeResult, verifySource, workflowOptions } from "./g6-pipeline.mjs";

const root = mkdtempSync(join(tmpdir(), "ocservia-g6-pipeline-"));
const binding = {
  "candidate-sha": "a".repeat(40), "run-id": "424242", "run-attempt": "3",
  "environment-id": "g6-0123456789abcdef", authority: "engineering",
  "release-manifest-digest": "f".repeat(64),
};
const expected = { candidate_sha: binding["candidate-sha"], run_id: binding["run-id"], run_attempt: 3,
  environment_id: binding["environment-id"], authority: binding.authority, release_manifest_digest: binding["release-manifest-digest"] };
try {
  const prepare = (domain, status = "passed") => {
    const directory = join(root, domain);
    mkdirSync(join(directory, "evidence"), { recursive: true });
    mkdirSync(join(directory, "evidence", "effects"), { recursive: true });
    writeFileSync(join(directory, "evidence", "sample.txt"), domain);
    runtimeResult({ ...binding, root: directory, domain, status, "domain-run-id": `424242-${domain}` });
    return directory;
  };
  const a = prepare("fd-a"), b = prepare("fd-b");
  verifySource(a, expected, "fd-a");
  for (const [field, wrong] of [["candidate_sha", "b".repeat(40)], ["run_attempt", 4], ["environment_id", "g6-ffffffffffffffff"]])
    assert.throws(() => verifySource(a, { ...expected, [field]: wrong }, "fd-a"));
  assert.throws(() => verifySource(a, expected, "fd-b"));
  const source = join(a, "source-manifest.json");
  const manifest = readFileSync(source, "utf8");
  for (const mutate of [
    value => value.files.push(value.files[0]),
    value => { value.files[0].path = "../outside"; },
    value => { value.files[0].sha256 = "0".repeat(64); },
  ]) {
    const value = JSON.parse(manifest); mutate(value);
    writeFileSync(source, JSON.stringify(value));
    assert.throws(() => verifySource(a, expected, "fd-a"));
  }
  writeFileSync(source, manifest);
  symlinkSync("sample.txt", join(a, "evidence", "link"));
  assert.throws(() => verifySource(a, expected, "fd-a"));
  rmSync(join(a, "evidence", "link"));
  writeFileSync(join(a, "evidence", "sample.txt"), "tampered");
  assert.throws(() => verifySource(a, expected, "fd-a"));
  prepare("fd-a");
  const builder = join(root, "builder.mjs"), output = join(root, "output");
  const options = { ...binding, "fd-a": a, "fd-b": b, output, "work-dir": join(root, "work"), builder, slo: "unused" };
  writeFileSync(builder, 'console.error("builder failed"); process.exit(17);');
  assert.equal(assemble(options), 17);
  assert.match(readFileSync(join(output, "build.stderr.log"), "utf8"), /builder failed/);
  writeFileSync(builder, 'process.exit(0);');
  assert.equal(assemble(options), 0);
  prepare("fd-b", "failed");
  assert.equal(assemble(options), 1);
  prepare("fd-b");
  assert.equal(assemble({ ...options, "fd-a": b }), 1);
  const env = { GITHUB_SHA: binding["candidate-sha"], GITHUB_RUN_ID: "424242", GITHUB_RUN_ATTEMPT: "3",
    G6_AUTHORITY: "engineering", G6_PIPELINE_NEEDS: JSON.stringify({ "g6-rd-fd-a": { outputs: { "release-manifest-digest": "f".repeat(64) } } }) };
  assert.notEqual(workflowOptions(env)["environment-id"], workflowOptions({ ...env, GITHUB_RUN_ATTEMPT: "4" })["environment-id"]);
  const diagnostics = join(root, "diagnostics");
  const record = (path, value) => {
    const file = join(diagnostics, path);
    mkdirSync(join(file, ".."), { recursive: true });
    writeFileSync(file, JSON.stringify(value));
  };
  const summarize = (status = "failed") => {
    runtimeResult({ ...binding, root: diagnostics, domain: "fd-b", status });
    verifySource(diagnostics, expected, "fd-b");
    return JSON.parse(readFileSync(join(diagnostics, "runtime-result.json"), "utf8"));
  };
  mkdirSync(diagnostics);
  assert.deepEqual(summarize().failure, { class: "harness_contract_failed", code: "runtime_job_failed" });
  assert.equal(summarize().last_phase, "unknown");
  record("harness/runtime/state.json", { completed_phases: [{ name: "prepare" }] });
  assert.equal(summarize().last_phase, "prepare");
  record("harness/runtime/state.json", { active_phase: { name: "promote" }, completed_phases: [{ name: "prepare" }] });
  assert.equal(summarize("cancelled").last_phase, "promote");
  assert.equal(summarize("cancelled").status, "cancelled");
  const peerFailure = { class: "peer_failed", code: "peer_job_failed" };
  record("harness/rendezvous/older.result.json", { status: "failed", checkpoint: "older", completed_at: "2026-01-01T00:00:00Z", failure: peerFailure });
  record("harness/rendezvous/newer.result.json", { status: "failed", checkpoint: "newer", completed_at: "2026-01-02T00:00:00Z", failure: peerFailure });
  record("harness/rendezvous/ignored.json", { status: "failed", checkpoint: "ignored", completed_at: "2026-01-03T00:00:00Z", failure: peerFailure });
  assert.equal(summarize().last_phase, "newer");
  assert.deepEqual(summarize().failure, peerFailure);
  const phaseFailure = { class: "phase_timeout", code: "phase_deadline_exceeded" };
  record("harness/runtime/phase-results/010.json", { status: "failed", phase: "prepare", sequence: 10, failure: phaseFailure });
  record("harness/runtime/phase-results/020.json", { status: "failed", phase: "build-images", sequence: 20, failure: phaseFailure });
  record("harness/runtime/phase-results/030.json", { status: "passed", phase: "tunnel-up", sequence: 30, failure: null });
  assert.equal(summarize().last_phase, "build-images");
  assert.deepEqual(summarize("cancelled").failure, phaseFailure);
  assert.equal(summarize("passed").failure, null);
  assert.equal(summarize("passed").evidence_complete, true);
  record("harness/runtime/phase-results/040.json", { status: "failed", phase: "invalid", sequence: 40, failure: {} });
  assert.throws(() => summarize(), /class and code/);
} finally { rmSync(root, { recursive: true }); }
console.log("Raw integrity, candidate/run binding, traversal, incomplete runtime and builder failures checked");
