import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import crypto from "node:crypto";
import { artifactManifest, verifyArtifacts } from "./release-artifacts.mjs";
const root = fs.mkdtempSync(path.join(os.tmpdir(), "release-artifacts-"));
const identity = { sha: "a".repeat(40), version: "1.0.2", arch: "amd64", component: "controller" };
const manifest = path.join(root, "candidate-controller-amd64.json");
const seal = value => {
  const bytes = JSON.stringify(value) + "\n";
  fs.writeFileSync(manifest, bytes);
  return crypto.createHash("sha256").update(bytes).digest("hex");
};
try {
  for (const name of ["gateway", "control", "transport", "backup"]) fs.writeFileSync(path.join(root, `${name}-linux-amd64.tar`), name);
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
} finally { fs.rmSync(root, { recursive: true }); }
console.log("Wrong candidate, altered products/manifest, traversal and symlink rejected");
