import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { execFileSync, spawnSync } from "node:child_process";
import { architectures, scenarios, compareVersions, validateInputs, validateRelease, requiredAssets, validateUnit, readJSON, candidateArtifacts, baselineDebAsset } from "./release-upgrade-contract.mjs";
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
const latest = validateInputs("0.6.2", "v0.6.1", sha, sha, sha, baselines);
assert.equal(latest.commit, "1805962fe1a98a22955b3105bfa8ebce7f2ea1eb");
assert.equal(latest.sums_sha256, "d562822bfdc55c784bf950a21c746f380a1cb6a7a2c86c6c801cfa25121df53b");
assert.equal(latest.key_der_sha256, valid().key_der_sha256);
assert.deepEqual(latest.controller, { database: "postgres", migration: 36, authentication: "oidc" });
const stable = baselines["v0.6.2"];
assert.equal(stable.commit, "518df6e9c488e58613c9cc194c896b4edfd57c2e");
assert.equal(stable.sums_sha256, "a386f64d81f0ccb0b87482c3f4e4029d0a756b5e76c679b72d70d1afa23abc66");
assert.equal(stable.key_der_sha256, valid().key_der_sha256);
assert.deepEqual(stable.controller, { database: "postgres", migration: 36, authentication: "oidc" });
assert(!Object.hasOwn(stable, "deb_asset_release"));
const maintenance = validateInputs("1.0.1", "v1.0.0", sha, sha, sha, baselines);
assert.equal(maintenance.commit, "e85ab3fa90d1d5f6e4c53b56b2f5e0278f6da060");
assert.equal(maintenance.sums_sha256, "0aa8270a68a65cc81b59a70001c53a9b4ace1ed6e33f4b36ef696db56ef4a401");
assert.equal(maintenance.key_der_sha256, stable.key_der_sha256);
assert.equal(maintenance.deb_asset_release, 1);
for (const baselineTag of Object.keys(baselines).filter(tag => tag.startsWith("v0."))) {
  for (const version of ["1.0.0", "1.0.1", "1.1.0"]) {
    assert.throws(() => validateInputs(version, baselineTag, sha, sha, sha, baselines), /pre-1\.0.*redeploy/);
  }
}
assert.equal(validateInputs("1.1.0", "v1.0.0", sha, sha, sha, baselines), maintenance);
for (const version of ["2.0.0", "2.1.0", "3.0.0"])
  assert.throws(() => validateInputs(version, "v1.0.0", sha, sha, sha, baselines), /2\.x and later.*migration contract/);
const contract = new URL("./release-upgrade-contract.mjs", import.meta.url).pathname;
execFileSync(process.execPath, [contract, "upgrade-path", "1.0.1", "v1.0.0"]);
const unsupported = spawnSync(process.execPath, [contract, "upgrade-path", "1.0.1", "v0.6.2"], {
  env: { ...process.env, GITHUB_STEP_SUMMARY: "" }, encoding: "utf8",
});
assert.equal(unsupported.status, 1);
assert.match(unsupported.stderr, /pre-1\.0.*redeploy/);
const majorTwo = spawnSync(process.execPath, [contract, "upgrade-path", "2.0.0", "v1.0.0"], {
  env: { ...process.env, GITHUB_STEP_SUMMARY: "" }, encoding: "utf8",
});
assert.equal(majorTwo.status, 1);
assert.match(majorTwo.stderr, /2\.x and later.*migration contract/);
const smoke = spawnSync("bash", [new URL("./release-baseline-upgrade-smoke.sh", import.meta.url).pathname], {
  env: { ...process.env, VERSION: "1.0.1", BASELINE_RELEASE: "v0.6.2", RUN_ID: "unsupported-upgrade",
    ARTIFACT_DIR: "/unused", CANDIDATE_DEB: "/unused", GITHUB_STEP_SUMMARY: "" }, encoding: "utf8",
});
assert.equal(smoke.status, 1);
assert.match(smoke.stderr, /pre-1\.0.*redeploy/);
console.log("Transitional v1.0.0 upgrades accepted; pre-1.0 to 1.x rejected before native host setup; 2.x+ candidates fail closed pending a reviewed 1.x to 2.x migration contract");
assert.throws(() => validateInputs("1.0.0", "v1.0.0", sha, sha, sha, baselines));
for (const version of ["0.6.2", "0.6.1", "1.0.0-rc.1"])
  assert.throws(() => validateInputs(version, "v0.6.2", sha, sha, sha, baselines));
for (const arch of Object.keys(architectures)) {
  for (const baselineTag of ["v0.1.1", "v0.3.0", "v0.4.0", "v0.5.0", "v0.5.1", "v0.5.2", "v0.6.0", "v0.6.1", "v0.6.2"]) {
    assert.equal(baselineDebAsset(baselineTag, arch, baselines[baselineTag]), `ocservia-agent_${baselineTag.slice(1)}_${arch}.deb`);
  }
  const future = maintenance;
  const legacyName = `ocservia-agent_0.6.1_${arch}.deb`;
  const futureName = `ocservia-agent_1.0.0-1_${arch}.deb`;
  assert(requiredAssets("v0.6.1", latest).includes(legacyName));
  assert(requiredAssets("v1.0.0", future).includes(futureName));
  assert(!requiredAssets("v1.0.0", future).includes(`ocservia-agent_1.0.0_${arch}.deb`));
  assert.equal(baselineDebAsset("v1.0.0", arch, { deb_asset_release: 2 }), `ocservia-agent_1.0.0-2_${arch}.deb`);
  assert.equal(candidateArtifacts("agent", arch, "1.0.0")[0], futureName);
  for (const [baselineTag, baseline, expected] of [["v0.6.1", latest, legacyName],
    ["v0.6.2", stable, `ocservia-agent_0.6.2_${arch}.deb`], ["v1.0.0", future, futureName]]) {
    const actual = execFileSync(process.execPath, [new URL("./release-upgrade-contract.mjs", import.meta.url).pathname,
      "baseline-deb", baselineTag, arch], { input: JSON.stringify(baseline), encoding: "utf8" }).trim();
    assert.equal(actual, expected);
    const published = { tag_name: baselineTag, draft: false, prerelease: false, published_at: "2026-09-19T00:00:00Z",
      assets: requiredAssets(baselineTag, baseline).map(name => ({ name, state: "uploaded" })) };
    const ref = { object: { type: "commit", sha: baseline.commit } };
    validateRelease(published, baselineTag, ref, baseline);
    published.assets = published.assets.filter(asset => asset.name !== expected);
    assert.throws(() => validateRelease(published, baselineTag, ref, baseline));
  }
}
for (const release of [0, -1, 1.5, "1", null, true]) {
  assert.throws(() => requiredAssets("v1.0.0", { ...latest, deb_asset_release: release }), /deb_asset_release/);
}
console.log("Legacy v0.6.1/v0.6.2 and revisioned baseline DEB naming, CLI and candidate separation passed");
for (const version of ["0.6.1", "0.6.0"])
  assert.throws(() => validateInputs(version, "v0.6.1", sha, sha, sha, baselines));
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
  assets: requiredAssets(tag, valid()).map(name => ({ name, state: "uploaded" })) };
const ref = { object: { type: "commit", sha: valid().commit } };
validateRelease(release, tag, ref, valid());
for (const asset of release.assets)
  assert.throws(() => validateRelease({ ...release, assets: release.assets.filter(a => a !== asset) }, tag, ref, valid()));
for (const field of ["draft", "prerelease"])
  assert.throws(() => validateRelease({ ...release, [field]: true }, tag, ref, valid()));
assert.throws(() => validateRelease(release, tag, { object: { type: "commit", sha } }, valid()));
const frozen = { candidate_sha: sha, candidate_version: "0.6.1", baseline_tag: tag, baseline_commit: valid().commit,
  baseline_lock_sha256: "b".repeat(64), run_id: run, run_attempt: attempt };
const units = Object.entries(scenarios).flatMap(([component, required]) => Object.entries(architectures).map(([arch, native]) => ({
  ...frozen, component, arch, native: { runner_arch: native.runner_arch, kernel: native.kernel, docker: arch },
  status: "pass", failure: null, started_at: "2026-09-14T01:00:00Z", finished_at: "2026-09-14T01:05:00Z",
  scenarios: Object.fromEntries(required.map(s => [s, "pass"])),
  artifacts: candidateArtifacts(component, arch, frozen.candidate_version).map(name => ({ name, sha256: "c".repeat(64) })),
})));
const gate = (results = units) => results.forEach(r => validateUnit(frozen, r, run));
gate();
const retry = structuredClone(units);
retry[0].run_attempt = "3";
gate(retry);
const legacyCandidate = structuredClone(units);
legacyCandidate[0].artifacts[0].name = `ocservia-agent_${frozen.candidate_version}_${legacyCandidate[0].arch}.deb`;
assert.throws(() => gate(legacyCandidate), /missing or duplicate candidate artifact/);
for (let i = 0; i < 4; i++) {
  for (const [field, bad] of Object.entries({ candidate_sha: "b".repeat(40), baseline_tag: "v0.4.0", run_id: "foreign", status: "cancelled", artifacts: [], failure: "failure" })) {
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
console.log("Release upgrade baseline, native identity, scenarios and independent rerun contracts passed");
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
