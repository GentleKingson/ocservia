import fs from "node:fs";
import crypto from "node:crypto";
import { execFileSync } from "node:child_process";
import { pathToFileURL } from "node:url";

export const architectures = {
  amd64: { runner: "ubuntu-24.04", runner_arch: "X64", kernel: "x86_64", rpm: "x86_64" },
  arm64: { runner: "ubuntu-24.04-arm", runner_arch: "ARM64", kernel: "aarch64", rpm: "aarch64" },
};
export const scenarios = {
  agent: ["deb_baseline", "deb_upgrade", "deb_state", "deb_retry", "deb_rejection", "deb_rollback",
    "rpm_baseline", "rpm_upgrade", "rpm_state", "rpm_retry", "rpm_rejection", "rpm_rollback", "no_auto_enable"],
  controller: ["baseline_ready", "baseline_data", "candidate_images", "failure_pending", "same_target_recovery",
    "production_smoke", "authenticated_read_write", "data_preserved", "migration", "idempotence", "release_state", "rollback_contract", "backup_restore"],
};
export const digest = (bytes) => crypto.createHash("sha256").update(bytes).digest("hex");
export const readJSON = (file) => JSON.parse(fs.readFileSync(file, "utf8"));
export function candidateArtifacts(component, arch, version) {
  return component === "agent" ? [`ocservia-agent_${version}-1_${arch}.deb`,
    `ocservia-agent-${version}-1.${architectures[arch].rpm}.rpm`, `ocservia-agent-${version}-linux-${arch}.tar.gz`] :
    ["gateway", "control", "transport", "backup", "edge", "relay", "signer", "mysql_backup", "mariadb_backup"].map(name => `${name}-linux-${arch}.tar`);
}
export function requireThat(condition, message) { if (!condition) throw new Error(message); }
export function compareVersions(a, b) {
  const parse = (v) => {
    requireThat(typeof v === "string" && /^[0-9]+\.[0-9]+\.[0-9]+$/.test(v), "invalid plain version");
    return v.split(".").map(BigInt);
  };
  const left = parse(a), right = parse(b);
  for (let i = 0; i < 3; i++) if (left[i] !== right[i]) return left[i] > right[i] ? 1 : -1;
  return 0;
}
export function validateUpgradePath(version, tag) {
  requireThat(/^v[0-9]+\.[0-9]+\.[0-9]+$/.test(tag), "baseline must be an exact stable tag");
  requireThat(compareVersions(version, tag.slice(1)) > 0, "candidate must be numerically newer than baseline");
  requireThat(compareVersions(version, "1.0.0") < 0 || compareVersions(tag.slice(1), "1.0.0") >= 0,
    "pre-1.0 in-place upgrades to stable releases are unsupported; redeploy instead");
  requireThat(compareVersions(version, "2.0.0") < 0,
    "2.x and later in-place upgrades are unsupported until a separately reviewed 1.x to 2.x migration contract exists");
}
export function validateInputs(version, tag, sha, dispatchSHA, head, baselines) {
  validateUpgradePath(version, tag);
  requireThat(/^[0-9a-f]{40}$/.test(sha) && sha === dispatchSHA && sha === head, "candidate SHA must equal dispatch SHA and checkout HEAD");
  const baseline = baselines[tag];
  requireThat(baseline?.controller?.database === "postgres" && baseline.rpm && baseline.production_relays &&
    baseline.upgrader && baseline.version_query && /^[0-9a-f]{40}$/.test(baseline.commit) &&
    /^[0-9a-f]{64}$/.test(baseline.key_der_sha256), "baseline lacks registered full upgrade capabilities");
  return baseline;
}
export function baselineDebAsset(tag, arch, baseline) {
  const release = baseline.deb_asset_release;
  requireThat(!Object.hasOwn(baseline, "deb_asset_release") || (Number.isSafeInteger(release) && release >= 1),
    "baseline deb_asset_release must be a positive integer when present");
  const revision = release === undefined ? "" : `-${release}`;
  return `ocservia-agent_${tag.slice(1)}${revision}_${arch}.deb`;
}
export function requiredAssets(tag, baseline) {
  const v = tag.slice(1);
  return ["SHA256SUMS", "SHA256SUMS.sig", "release-signing.pub.pem",
    ...Object.entries(architectures).flatMap(([arch, { rpm }]) => [
      baselineDebAsset(tag, arch, baseline), `ocservia-agent-${v}-1.${rpm}.rpm`,
      `controller-release-${arch}.json`, `controller-release-${arch}.json.sha256`,
    ])];
}
export function validateRelease(release, tag, ref, baseline) {
  requireThat(release.tag_name === tag && release.draft === false && release.prerelease === false && release.published_at,
    "baseline is not a published stable Release");
  requireThat(ref.object?.type === "commit" && ref.object.sha === baseline.commit, "baseline tag commit changed");
  for (const name of requiredAssets(tag, baseline)) {
    requireThat(release.assets.filter((a) => a.name === name && a.state === "uploaded").length === 1, `missing or duplicate baseline asset: ${name}`);
  }
}
export function validateUnit(frozen, r, runID) {
  const key = `${r.component}-${r.arch}`;
  requireThat(scenarios[r.component] && architectures[r.arch], "unknown unit");
  for (const field of ["candidate_sha", "candidate_version", "baseline_tag", "baseline_commit", "baseline_lock_sha256"])
    requireThat(r[field] === frozen[field], `inconsistent ${key} ${field}`);
  requireThat(r.run_id === runID && frozen.run_id === runID, "foreign workflow run");
  const expected = architectures[r.arch];
  requireThat(r.native?.runner_arch === expected.runner_arch && r.native?.kernel === expected.kernel &&
    r.native?.docker === r.arch, "native architecture evidence mismatch");
  requireThat(r.status === "pass" && r.failure === null && Number.isFinite(Date.parse(r.started_at)) &&
    Date.parse(r.finished_at) >= Date.parse(r.started_at), "unit incomplete or failed");
  requireThat(scenarios[r.component].every(s => r.scenarios?.[s] === "pass"), "required scenario missing or not passed");
  requireThat(Array.isArray(r.artifacts) && r.artifacts.every(a =>
    typeof a.name === "string" && /^[0-9a-f]{64}$/.test(a.sha256)), "missing tested artifact digests");
  for (const name of candidateArtifacts(r.component, r.arch, r.candidate_version))
    requireThat(r.artifacts.filter(a => a.name === name).length === 1, `missing or duplicate candidate artifact: ${name}`);
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  try {
    const [mode, directory] = process.argv.slice(2);
    const env = process.env;
    if (mode === "baseline-deb") {
      console.log(baselineDebAsset(directory, process.argv[4], readJSON(0)));
    } else if (mode === "upgrade-path") {
      validateUpgradePath(directory, process.argv[4]);
    } else if (mode === "prepare") {
      const raw = fs.readFileSync("scripts/release-upgrade-baselines.json");
      const head = execFileSync("git", ["rev-parse", "HEAD"], { encoding: "utf8" }).trim();
      const baseline = validateInputs(env.VERSION, env.BASELINE_RELEASE, env.CANDIDATE_SHA, env.GITHUB_SHA, head, JSON.parse(raw));
      const api = (route) => JSON.parse(execFileSync("gh", ["api", `repos/GentleKingson/ocservia/${route}`], { encoding: "utf8" }));
      const release = api(`releases/tags/${env.BASELINE_RELEASE}`);
      validateRelease(release, env.BASELINE_RELEASE, api(`git/ref/tags/${env.BASELINE_RELEASE}`), baseline);
      fs.mkdirSync(directory, { recursive: true });
      const frozen = { candidate_sha: head, candidate_version: env.VERSION, baseline_tag: env.BASELINE_RELEASE,
        baseline_commit: baseline.commit, baseline_lock_sha256: digest(raw), baseline,
        run_id: env.GITHUB_RUN_ID, run_attempt: env.GITHUB_RUN_ATTEMPT,
        prepared_at: new Date().toISOString(), release_id: release.id,
        assets: release.assets.filter((a) => requiredAssets(env.BASELINE_RELEASE, baseline).includes(a.name)).map((a) => ({ name: a.name, url: a.browser_download_url, digest: a.digest })) };
      fs.writeFileSync(`${directory}/frozen.json`, JSON.stringify(frozen, null, 2) + "\n");
      fs.appendFileSync(env.GITHUB_OUTPUT, `baseline_commit=${baseline.commit}\n`);
    } else if (mode === "validate-unit") {
      validateUnit(readJSON(env.FROZEN_FILE), readJSON(directory), env.GITHUB_RUN_ID);
    } else throw new Error("expected baseline-deb, upgrade-path, prepare or validate-unit");
  } catch (error) {
    console.error(error.message);
    if (process.env.GITHUB_STEP_SUMMARY) fs.appendFileSync(process.env.GITHUB_STEP_SUMMARY, `Native Upgrade Result: NOT PASS (${error.message})\n`);
    process.exitCode = 1;
  }
}
