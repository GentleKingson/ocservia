import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import crypto from "node:crypto";
import { execFileSync } from "node:child_process";
import { pathToFileURL } from "node:url";
import { verifyArtifacts } from "./release-artifacts.mjs";

export function acceptedRun(run, sha) {
  return run.head_sha === sha && run.head_branch === "main" && run.event === "workflow_dispatch" &&
    run.conclusion === "success" && run.path === ".github/workflows/release-upgrade.yml";
}

export function producerArtifact(receipt, sha, version, run, arch, component) {
  if (receipt.source_commit !== sha || receipt.version !== version || receipt.run_id !== String(run.id) ||
      !/^[1-9][0-9]*$/.test(receipt.run_attempt) || Number(receipt.run_attempt) > run.run_attempt)
    throw new Error("accepted candidate identity mismatch");
  const leg = receipt.products?.[arch];
  const id = leg?.outputs?.[`${component}-id`];
  const digest = leg?.outputs?.[`${component}-sha256`];
  if (leg?.result !== "success" || !/^[1-9][0-9]*$/.test(id ?? "") || !/^[0-9a-f]{64}$/.test(digest ?? ""))
    throw new Error("accepted producer artifact is missing");
  return { id, digest };
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  const [directory, arch, version] = process.argv.slice(2);
  const sha = process.env.GITHUB_SHA;
  if (!/^[0-9a-f]{40}$/.test(sha ?? "") || !["amd64", "arm64"].includes(arch) || !/^\d+\.\d+\.\d+$/.test(version ?? ""))
    throw new Error("invalid promotion identity");
  const repository = "GentleKingson/ocservia";
  const api = route => JSON.parse(execFileSync("gh", ["api", `repos/${repository}/${route}`], { encoding: "utf8" }));
  const scratch = fs.mkdtempSync(path.join(os.tmpdir(), "accepted-products-"));
  const download = (artifact, destination) => {
    if (!Number.isSafeInteger(artifact.id) || artifact.expired) throw new Error("accepted artifact expired or invalid");
    const zip = path.join(scratch, `${artifact.id}.zip`);
    const fd = fs.openSync(zip, "w", 0o600);
    try { execFileSync("gh", ["api", `repos/${repository}/actions/artifacts/${artifact.id}/zip`], { stdio: ["ignore", fd, "inherit"] }); }
    finally { fs.closeSync(fd); }
    fs.mkdirSync(destination, { recursive: true });
    execFileSync("unzip", ["-q", zip, "-d", destination]);
    fs.unlinkSync(zip);
  };
  try {
    const runs = api(`actions/workflows/release-upgrade.yml/runs?event=workflow_dispatch&status=success&head_sha=${sha}&per_page=100`).workflow_runs;
    let selected;
    for (const run of runs.filter(run => acceptedRun(run, sha))) {
      const artifacts = api(`actions/runs/${run.id}/artifacts?per_page=100`).artifacts;
      const bundles = artifacts.filter(a => !a.expired && a.name.startsWith(`integrated-candidate-${run.id}-`)).sort((a, b) => b.id - a.id);
      for (const artifact of bundles) {
        const bundle = path.join(scratch, String(artifact.id));
        download(artifact, bundle);
        execFileSync("openssl", ["pkeyutl", "-verify", "-rawin", "-pubin", "-inkey", "candidate-signing.pub.pem", "-in", "SHA256SUMS", "-sigfile", "SHA256SUMS.sig"], { cwd: bundle });
        execFileSync("sha256sum", ["--strict", "-c", "SHA256SUMS"], { cwd: bundle });
        const receipt = JSON.parse(fs.readFileSync(path.join(bundle, "producer.json")));
        if (receipt.version !== version) continue;
        selected = { receipt, run, artifacts, bundle };
        break;
      }
      if (selected) break;
    }
    if (!selected) throw new Error("no successful main Integrated acceptance for this exact SHA/version; run release-upgrade integration first (never rebuild for promotion)");
    fs.mkdirSync(directory, { recursive: true });
    for (const component of ["agent", "controller"]) {
      const { id, digest } = producerArtifact(selected.receipt, sha, version, selected.run, arch, component);
      const artifact = selected.artifacts.find(a => String(a.id) === id);
      if (!artifact) throw new Error("accepted product artifact missing from its run");
      const destination = path.join(directory, component);
      download(artifact, destination);
      verifyArtifacts(destination, { sha, version, arch, component }, digest);
      fs.appendFileSync(process.env.GITHUB_OUTPUT, `${component}-sha256=${digest}\n`);
    }
    fs.cpSync(selected.bundle, path.join(directory, "accepted"), { recursive: true });
    fs.appendFileSync(process.env.GITHUB_OUTPUT, `accepted-sha256=${crypto.createHash("sha256").update(fs.readFileSync(path.join(selected.bundle, "SHA256SUMS"))).digest("hex")}\n`);
    fs.appendFileSync(process.env.GITHUB_STEP_SUMMARY, `Reused accepted candidate run ${selected.run.id}, source ${sha}, version ${version}, ${arch}; no product build.\n`);
  } finally { fs.rmSync(scratch, { recursive: true, force: true }); }
}
