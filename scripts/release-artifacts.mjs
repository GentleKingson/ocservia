import fs from "node:fs";
import path from "node:path";
import crypto from "node:crypto";
import { pathToFileURL } from "node:url";
import { candidateArtifacts } from "./release-upgrade-contract.mjs";

const hash = bytes => crypto.createHash("sha256").update(bytes).digest("hex");
const fixtureFiles = {
  "test-helpers": ["probe.tar", "relay.tar", "ocservia-g6-tunnel"],
  "session-base": ["node.tar", "workflow-tools.tar"],
  "rpm-test": ["rpm.tar"],
};
export function artifactManifest(directory, identity) {
  if (!/^[0-9a-f]{40}$/.test(identity.sha) || !/^\d+\.\d+\.\d+$/.test(identity.version) ||
      !["amd64", "arm64"].includes(identity.arch) || !["agent", "controller", ...Object.keys(fixtureFiles)].includes(identity.component))
    throw new Error("invalid candidate identity");
  const names = fixtureFiles[identity.component] ?? candidateArtifacts(identity.component, identity.arch, identity.version);
  if (identity.component === "agent") {
    const archive = names.find(name => name.endsWith(".tar.gz"));
    names.push(...[".sha256", ".sha256.sig", ".sha256.pub.pem"].map(suffix => archive + suffix));
  }
  return { ...identity, files: names.map(name => {
    const file = path.join(directory, name);
    if (!fs.lstatSync(file).isFile()) throw new Error(`not a regular artifact: ${name}`);
    return { name, sha256: hash(fs.readFileSync(file)) };
  }) };
}

export function verifyArtifacts(directory, identity, expectedHash) {
  if (!/^[0-9a-f]{64}$/.test(expectedHash ?? "")) throw new Error("missing producer manifest digest");
  const file = path.join(directory, `candidate-${identity.component}-${identity.arch}.json`);
  if (!fs.lstatSync(file).isFile()) throw new Error("manifest is not a regular file");
  const bytes = fs.readFileSync(file);
  if (hash(bytes) !== expectedHash) throw new Error("producer manifest digest mismatch");
  const expected = artifactManifest(directory, identity);
  if (JSON.stringify(JSON.parse(bytes)) !== JSON.stringify(expected)) throw new Error("candidate or artifact mismatch");
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  const [mode, directory, component, arch, version, expectedHash] = process.argv.slice(2);
  const identity = { sha: process.env.GITHUB_SHA, version, arch, component };
  if (mode === "seal") {
    const bytes = JSON.stringify(artifactManifest(directory, identity)) + "\n";
    fs.writeFileSync(path.join(directory, `candidate-${component}-${arch}.json`), bytes);
    fs.appendFileSync(process.env.GITHUB_OUTPUT, `sha256=${hash(bytes)}\n`);
  } else if (mode === "verify") verifyArtifacts(directory, identity, expectedHash);
  else throw new Error("expected seal or verify");
}
