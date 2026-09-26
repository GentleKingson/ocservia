import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import crypto from "node:crypto";
import { artifactManifest, verifyArtifacts, validateCandidate } from "./release-artifacts.mjs";
const sha = "a".repeat(40);
for (const version of ["0.1.0", "1.1.0", "2.0.0"]) validateCandidate(version, sha, sha, sha);
for (const version of [undefined, "", "v1.1.0", "1.1", "../1.1.0"])
  assert.throws(() => validateCandidate(version, sha, sha, sha));
for (const args of [["bad", sha, sha], [sha, "b".repeat(40), sha], [sha, sha, "b".repeat(40)]])
  assert.throws(() => validateCandidate("1.1.0", ...args));
const root = fs.mkdtempSync(path.join(os.tmpdir(), "release-artifacts-"));
const identity = { sha: "a".repeat(40), version: "1.0.2", arch: "amd64", component: "controller" };
const manifest = path.join(root, "candidate-controller-amd64.json");
const seal = value => {
  const bytes = JSON.stringify(value) + "\n";
  fs.writeFileSync(manifest, bytes);
  return crypto.createHash("sha256").update(bytes).digest("hex");
};
try {
  for (const arch of ["amd64", "arm64"]) {
    const agent = { ...identity, arch, component: "agent" };
    const rpm = arch === "amd64" ? "x86_64" : "aarch64";
    const archive = `ocservia-agent-${agent.version}-linux-${arch}.tar.gz`;
    const files = [`ocservia-agent_${agent.version}-1_${arch}.deb`,
      `ocservia-agent-${agent.version}-1.${rpm}.rpm`, archive,
      ...[".sha256", ".sha256.sig", ".sha256.pub.pem"].map(suffix => archive + suffix)];
    for (const name of files) fs.writeFileSync(path.join(root, name), name);
    assert.deepEqual(artifactManifest(root, agent).files.map(file => file.name), files);
    fs.unlinkSync(path.join(root, files[0]));
    fs.writeFileSync(path.join(root, `ocservia-agent_${agent.version}_${arch}.deb`), "wrong name");
    assert.throws(() => artifactManifest(root, agent));
  }
  for (const name of ["gateway", "control", "transport", "backup", "edge", "relay", "signer", "mysql_backup", "mariadb_backup"]) fs.writeFileSync(path.join(root, `${name}-linux-amd64.tar`), name);
  const value = artifactManifest(root, identity);
  const digest = seal(value);
  verifyArtifacts(root, identity, digest);
  for (const field of ["sha", "arch", "version"])
    assert.throws(() => verifyArtifacts(root, { ...identity, [field]: "wrong" }, digest));
  assert.throws(() => verifyArtifacts(root, identity, "b".repeat(64)));
  const wrong = { ...value, sha: "b".repeat(40) };
  assert.throws(() => verifyArtifacts(root, identity, seal(wrong)));
  const traversal = structuredClone(value); traversal.files[0].name = "../outside";
  assert.throws(() => verifyArtifacts(root, identity, seal(traversal)));
  seal(value);
  fs.writeFileSync(path.join(root, "control-linux-amd64.tar"), "different build of same SHA");
  assert.throws(() => verifyArtifacts(root, identity, digest));
  fs.rmSync(path.join(root, "control-linux-amd64.tar"));
  fs.symlinkSync("gateway-linux-amd64.tar", path.join(root, "control-linux-amd64.tar"));
  assert.throws(() => artifactManifest(root, identity));
  for (const [component, names] of Object.entries({
    "test-helpers": ["probe.tar", "relay.tar", "ocservia-g6-tunnel"],
  })) {
    const fixtureIdentity = { ...identity, component };
    for (const name of names) fs.writeFileSync(path.join(root, name), name);
    const bytes = JSON.stringify(artifactManifest(root, fixtureIdentity)) + "\n";
    const digest = crypto.createHash("sha256").update(bytes).digest("hex");
    fs.writeFileSync(path.join(root, `candidate-${component}-amd64.json`), bytes);
    verifyArtifacts(root, fixtureIdentity, digest);
    assert.throws(() => verifyArtifacts(root, { ...fixtureIdentity, sha: "b".repeat(40) }, digest));
    fs.writeFileSync(path.join(root, names[0]), "tampered");
    assert.throws(() => verifyArtifacts(root, fixtureIdentity, digest));
    fs.unlinkSync(path.join(root, names[0]));
    assert.throws(() => verifyArtifacts(root, fixtureIdentity, digest));
  }
} finally { fs.rmSync(root, { recursive: true }); }
console.log("Wrong candidate, altered products/manifest, traversal and symlink rejected");
