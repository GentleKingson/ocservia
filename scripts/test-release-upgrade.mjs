import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { architectures, scenarios, compareVersions, validateInputs, validateRelease, requiredAssets, validateResults, readJSON, candidateArtifacts } from "./release-upgrade-contract.mjs";
const baselines = readJSON(new URL("./release-upgrade-baselines.json", import.meta.url));
const sha = "a".repeat(40), run = "123", attempt = "2";
assert.equal(compareVersions("0.10.0", "0.9.9"), 1);
assert.equal(compareVersions("10.0.0", "2.999.999"), 1);
assert.equal(compareVersions("9007199254740993.0.0", "9007199254740992.0.0"), 1);
assert.equal(compareVersions("00.05.0", "0.5.0"), 0);
for (const v of ["v0.6.0", "0.6", "0.6.0-rc1", "0.6.0\n", "1;echo bad"])
  assert.throws(() => compareVersions(v, "0.5.0"));
const tag = "v0.6.0";
const valid = () => validateInputs("0.6.1", tag, sha, sha, sha, baselines);
assert.equal(valid().commit, "cc8399641dc32083466a9c77369fcb8debf1ee48");
assert.equal(valid().sums_sha256, "26f4ab236630ff52777dabf5723cd3d5814022d4e08ab4079768117dd4e262df");
assert.deepEqual(valid().controller, { database: "postgres", migration: 36, authentication: "oidc" });
assert.equal(valid().key_der_sha256, baselines["v0.5.0"].key_der_sha256);
assert.equal(baselines["v0.5.0"].commit, "519275567a65f5785260e353ef02ab3fabf30244");
assert.equal(baselines["v0.5.1"].commit, "4afa4756fc89fcad30a0f93edb9b079d612453a6");
assert.equal(baselines["v0.5.1"].sums_sha256, "b1f6b1319a50cdc99cbdf30a352b88af289fc8aa90150aea8088e32bf4e38c25");
assert.equal(valid().key_der_sha256, baselines["v0.5.1"].key_der_sha256);
const historical = validateInputs("0.6.0", "v0.5.2", sha, sha, sha, baselines);
assert.equal(historical.commit, "2bbbbbbc7dd4a9b328acfaae291fdaf11a35bfd6");
assert.equal(historical.sums_sha256, "acbb66cf0f52fe64d3c3398e3fa6dfd8098190cf14863c24a0592e8f858af0be");
assert.equal(valid().key_der_sha256, historical.key_der_sha256);
for (const tag of ["latest", "v0.4.0", "v99.0.0"])
  assert.throws(() => validateInputs("100.0.0", tag, sha, sha, sha, baselines));
for (const version of ["0.6.0", "0.5.2", "0.5.1", "0.5.0", "0.4.99"])
  assert.throws(() => validateInputs(version, tag, sha, sha, sha, baselines));
for (const values of [[sha.slice(0, 7), sha, sha], [sha, "b".repeat(40), sha], [sha, sha, "b".repeat(40)]])
  assert.throws(() => validateInputs("0.6.1", tag, ...values, baselines));
assert.deepEqual(architectures, {
  amd64: { runner: "ubuntu-24.04", runner_arch: "X64", kernel: "x86_64", rpm: "x86_64" },
  arm64: { runner: "ubuntu-24.04-arm", runner_arch: "ARM64", kernel: "aarch64", rpm: "aarch64" },
});
const release = { tag_name: tag, draft: false, prerelease: false, published_at: "2026-09-15T05:56:14Z",
  assets: requiredAssets(tag).map(name => ({ name, state: "uploaded" })) };
const ref = { object: { type: "commit", sha: valid().commit } };
validateRelease(release, tag, ref, valid());
for (const asset of release.assets)
  assert.throws(() => validateRelease({ ...release, assets: release.assets.filter(a => a !== asset) }, tag, ref, valid()));
for (const field of ["draft", "prerelease"])
  assert.throws(() => validateRelease({ ...release, [field]: true }, tag, ref, valid()));
assert.throws(() => validateRelease(release, tag, { object: { type: "commit", sha } }, valid()));
const frozen = { candidate_sha: sha, candidate_version: "0.6.1", baseline_tag: tag, baseline_commit: valid().commit,
  baseline_lock_sha256: "b".repeat(64), run_id: run, run_attempt: attempt };
const needs = Object.fromEntries(["prepare", "agent-upgrade", "controller-upgrade"].map(j => [j, { result: "success" }]));
const units = Object.entries(scenarios).flatMap(([component, required]) => Object.entries(architectures).map(([arch, native]) => ({
  ...frozen, component, arch, native: { runner_arch: native.runner_arch, kernel: native.kernel, docker: arch, binfmt: "none" },
  status: "pass", failure: null, started_at: "2026-09-14T01:00:00Z", finished_at: "2026-09-14T01:05:00Z",
  scenarios: Object.fromEntries(required.map(s => [s, "pass"])),
  artifacts: candidateArtifacts(component, arch, frozen.candidate_version).map(name => ({ name, sha256: "c".repeat(64) })),
})));
const gate = (results = units, jobs = needs) => validateResults(frozen, results, jobs, run, attempt);
assert.equal(gate().status, "pass");
for (let i = 0; i < 4; i++) {
  assert.throws(() => gate(units.filter((_, index) => index !== i)));
  for (const [field, bad] of Object.entries({ candidate_sha: "b".repeat(40), baseline_tag: "v0.4.0", run_attempt: "1", status: "cancelled", artifacts: [], failure: "failure" })) {
    const changed = structuredClone(units); changed[i][field] = bad;
    assert.throws(() => gate(changed));
  }
  for (const s of scenarios[units[i].component]) for (const bad of [undefined, "skip", "fail", "cancelled"]) {
    const changed = structuredClone(units); changed[i].scenarios[s] = bad;
    assert.throws(() => gate(changed));
  }
  const changed = structuredClone(units); changed[i].native.kernel = "wrong";
  assert.throws(() => gate(changed));
}
assert.throws(() => gate([...units, units[0]]));
assert.throws(() => gate([units[0], units[0], ...units.slice(2)]));
for (const job of Object.keys(needs)) for (const result of ["failure", "cancelled", "skipped", "", "timed_out"])
  assert.throws(() => gate(units, { ...needs, [job]: { result } }));
console.log("Release upgrade input, baseline, architecture, identity and four-unit fail-closed contracts passed");
const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "upgrade-redaction-"));
try {
  fs.mkdirSync(`${tmp}/secrets`);
  fs.writeFileSync(`${tmp}/secrets/password`, "fixture-password-never-upload\n");
  fs.writeFileSync(`${tmp}/secrets/private-key`, "-----BEGIN PRIVATE KEY-----\nSENSITIVE-KEY-LINE-0123456789\n-----END PRIVATE KEY-----\n");
  fs.writeFileSync(`${tmp}/raw`, "fixture-password-never-upload SENSITIVE-KEY-LINE-0123456789 postgres://user:other@database:5432/test health=failed");
  execFileSync(process.execPath, [new URL("./redact-release-upgrade-log.mjs", import.meta.url).pathname, `${tmp}/raw`, `${tmp}/secrets`, `${tmp}/safe`]);
  const safe = fs.readFileSync(`${tmp}/safe`, "utf8");
  assert(!safe.includes("fixture-password") && !safe.includes("SENSITIVE-KEY") && !safe.includes("user:other"));
  assert(safe.includes("health=failed"));
} finally { fs.rmSync(tmp, { recursive: true }); }
console.log("Upgrade diagnostic credential redaction passed");
