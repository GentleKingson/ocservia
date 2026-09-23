#!/usr/bin/env node

import { createHash } from "node:crypto";
import {
  cpSync,
  existsSync,
  lstatSync,
  mkdirSync,
  readFileSync,
  readdirSync,
  rmSync,
  statSync,
  writeFileSync,
} from "node:fs";
import { spawnSync } from "node:child_process";
import { basename, dirname, join, relative, resolve, sep } from "node:path";
import { fileURLToPath } from "node:url";
import { parseArgs } from "node:util";

const RUNTIME_SCHEMA = "ocservia.g6-runtime-result.v1";
const SOURCE_SCHEMA = "ocservia.g6-source-manifest.v1";

function fail(message) {
  throw new Error(message);
}

function readJson(path) {
  try {
    return JSON.parse(readFileSync(path, "utf8"));
  } catch (error) {
    fail(`cannot read JSON ${path}: ${error.message}`);
  }
}

function writeJson(path, value) {
  mkdirSync(dirname(path), { recursive: true });
  writeFileSync(path, `${JSON.stringify(value, null, 2)}\n`);
}

function digestFile(path) {
  return createHash("sha256").update(readFileSync(path)).digest("hex");
}

function walkFiles(root, current = root) {
  const files = [];
  for (const entry of readdirSync(current, { withFileTypes: true })) {
    const path = join(current, entry.name);
    if (entry.isSymbolicLink()) {
      fail(`source evidence must not contain symlinks: ${relative(root, path)}`);
    }
    if (entry.isDirectory()) {
      files.push(...walkFiles(root, path));
    } else if (entry.isFile()) {
      const name = relative(root, path).split(sep).join("/");
      if (name !== "source-manifest.json") {
        files.push(name);
      }
    } else {
      fail(`source evidence contains a non-regular entry: ${relative(root, path)}`);
    }
  }
  return files.sort();
}

function bindingFromOptions(values, allowMissingRelease = false) {
  const binding = {
    candidate_sha: values["candidate-sha"],
    run_id: values["run-id"],
    run_attempt: Number(values["run-attempt"]),
    environment_id: values["environment-id"],
    authority: values.authority,
    release_manifest_digest: values["release-manifest-digest"] || null,
  };
  if (!/^[0-9a-f]{40}$/.test(binding.candidate_sha ?? "")) {
    fail("candidate-sha must be a lowercase 40-character Git SHA");
  }
  if (!binding.run_id) fail("run-id is required");
  if (!Number.isSafeInteger(binding.run_attempt) || binding.run_attempt < 1) {
    fail("run-attempt must be a positive integer");
  }
  if (!/^g6-[a-z0-9]{8,32}$/.test(binding.environment_id ?? "")) {
    fail("environment-id is invalid");
  }
  if (!["engineering", "production_readiness"].includes(binding.authority)) {
    fail("authority is invalid");
  }
  if (!(allowMissingRelease && binding.release_manifest_digest === null) &&
      !/^[0-9a-f]{64}$/.test(binding.release_manifest_digest ?? "")) {
    fail("release-manifest-digest must be a lowercase SHA-256 digest");
  }
  return binding;
}

function workflowOptions(env = process.env) {
  if (!env.G6_PIPELINE_NEEDS) return {};
  const needs = JSON.parse(env.G6_PIPELINE_NEEDS);
  return {
    "candidate-sha": env.GITHUB_SHA,
    "run-id": env.GITHUB_RUN_ID,
    "run-attempt": env.GITHUB_RUN_ATTEMPT,
    "environment-id": `g6-${createHash("sha256").update(`${env.GITHUB_RUN_ID}-${env.GITHUB_RUN_ATTEMPT}`).digest("hex").slice(0, 16)}`,
    authority: env.G6_AUTHORITY,
    "release-manifest-digest": needs["g6-rd-fd-a"]?.outputs?.["release-manifest-digest"],
  };
}

function assertBinding(actual, expected, label) {
  for (const key of [
    "candidate_sha",
    "run_id",
    "run_attempt",
    "environment_id",
    "authority",
    "release_manifest_digest",
  ]) {
    if (actual?.[key] !== expected[key]) {
      fail(`${label} ${key} mismatch: expected ${expected[key]}, got ${actual?.[key]}`);
    }
  }
}

function sourceManifest(root, binding, failureDomain) {
  const files = walkFiles(root).map((path) => {
    const absolute = resolve(root, path);
    if (!absolute.startsWith(`${resolve(root)}${sep}`)) {
      fail(`source evidence path escapes its root: ${path}`);
    }
    return {
      path,
      sha256: digestFile(absolute),
      size: statSync(absolute).size,
    };
  });
  return {
    schema_version: SOURCE_SCHEMA,
    ...binding,
    failure_domain: failureDomain,
    files,
  };
}

function verifySource(root, expectedBinding, expectedDomain) {
  const path = join(root, "source-manifest.json");
  const manifest = readJson(path);
  if (manifest.schema_version !== SOURCE_SCHEMA) {
    fail(`${expectedDomain} source manifest schema is invalid`);
  }
  assertBinding(manifest, expectedBinding, `${expectedDomain} source manifest`);
  if (manifest.failure_domain !== expectedDomain) {
    fail(`${expectedDomain} source manifest producer mismatch`);
  }
  if (!Array.isArray(manifest.files)) fail(`${expectedDomain} source files are invalid`);
  const actual = walkFiles(root);
  const declared = manifest.files.map((entry) => entry.path);
  if (new Set(declared).size !== declared.length) {
    fail(`${expectedDomain} source manifest contains duplicate paths`);
  }
  if (JSON.stringify([...declared].sort()) !== JSON.stringify(actual)) {
    fail(`${expectedDomain} source manifest does not exactly cover its payload`);
  }
  for (const entry of manifest.files) {
    if (!/^[0-9a-f]{64}$/.test(entry.sha256 ?? "")) {
      fail(`${expectedDomain} source digest is invalid for ${entry.path}`);
    }
    const absolute = resolve(root, entry.path);
    if (!absolute.startsWith(`${resolve(root)}${sep}`) || lstatSync(absolute).isSymbolicLink()) {
      fail(`${expectedDomain} source path is unsafe: ${entry.path}`);
    }
    if (digestFile(absolute) !== entry.sha256 || statSync(absolute).size !== entry.size) {
      fail(`${expectedDomain} source payload mismatch: ${entry.path}`);
    }
  }
  return manifest;
}

function runtimeResult(values) {
  const root = resolve(values.root);
  const output = resolve(values.output ?? join(root, "runtime-result.json"));
  const domain = values.domain;
  const status = values.status;
  if (!["fd-a", "fd-b"].includes(domain)) fail("domain must be fd-a or fd-b");
  if (!["passed", "failed", "cancelled"].includes(status)) fail("runtime status is invalid");
  const binding = bindingFromOptions(values);
  const result = {
    schema_version: RUNTIME_SCHEMA,
    ...binding,
    failure_domain: domain,
    domain_run_id: values["domain-run-id"] || `${binding.run_id}-${domain}`,
    status,
    evidence_complete: status === "passed",
    last_phase: values["last-phase"] || (status === "passed" ? "runtime_complete" : "unknown"),
    failure: status === "passed"
      ? null
      : {
          class: values["failure-class"] || "harness_contract_failed",
          code: values["failure-code"] || "runtime_job_failed",
        },
  };
  writeJson(output, result);
  writeJson(join(root, "source-manifest.json"), sourceManifest(root, binding, domain));
}

function validateRuntime(root, binding, domain) {
  const result = readJson(join(root, "runtime-result.json"));
  if (result.schema_version !== RUNTIME_SCHEMA || result.failure_domain !== domain) {
    fail(`${domain} runtime result is invalid`);
  }
  assertBinding(result, binding, `${domain} runtime result`);
  verifySource(root, binding, domain);
  return result;
}

function copyAssemblyInput(source, destination) {
  rmSync(destination, { recursive: true, force: true });
  cpSync(source, destination, {
    recursive: true,
    filter: (path) => basename(path) !== "source-manifest.json",
  });
}

function mergePeerEffects(fdA, runDir) {
  const source = join(fdA, "evidence", "effects");
  const destination = join(runDir, "state", "evidence", "effects");
  if (!existsSync(source)) fail("fd-a raw evidence has no Agent effects directory");
  mkdirSync(destination, { recursive: true });
  for (const entry of readdirSync(source, { withFileTypes: true })) {
    if (!entry.isFile() || entry.isSymbolicLink()) {
      fail(`fd-a effects contains an unsafe entry: ${entry.name}`);
    }
    const target = join(destination, entry.name);
    if (existsSync(target)) fail(`duplicate Agent effect journal: ${entry.name}`);
    cpSync(join(source, entry.name), target);
  }
}

function assemble(values) {
  const fdA = resolve(values["fd-a"]), fdB = resolve(values["fd-b"]);
  const out = resolve(values.output), work = resolve(values["work-dir"]);
  const binding = bindingFromOptions(values);
  mkdirSync(out, { recursive: true });
  try {
    const a = validateRuntime(fdA, binding, "fd-a");
    const b = validateRuntime(fdB, binding, "fd-b");
    if (a.status !== "passed" || b.status !== "passed") fail("runtime evidence is incomplete");
    copyAssemblyInput(fdB, work);
    mergePeerEffects(fdA, work);
    const builder = spawnSync(process.execPath, [
      values.builder, "--run-dir", work, "--peer-dir", fdA, "--out-dir", out,
      "--slo", values.slo, "--environment-id", binding.environment_id,
      "--candidate-sha", binding.candidate_sha, "--authority", binding.authority,
      "--failure-domain-class", "multi_host", "--run-id", b.domain_run_id || `${binding.run_id}-fd-b`,
    ], { encoding: "utf8" });
    writeFileSync(join(out, "build.stdout.log"), builder.stdout || "");
    writeFileSync(join(out, "build.stderr.log"), builder.stderr || builder.error?.message || "");
    return builder.status ?? 1;
  } catch (error) {
    writeFileSync(join(out, "build.stderr.log"), error.stack || error.message);
    return 1;
  }
}

function parse(command, args) {
  const common = {
    "candidate-sha": { type: "string" },
    "run-id": { type: "string" },
    "run-attempt": { type: "string" },
    "environment-id": { type: "string" },
    authority: { type: "string" },
    "release-manifest-digest": { type: "string" },
  };
  const commandOptions = {
    "check-runtime": { ...common, root: { type: "string" }, domain: { type: "string" } },
    "runtime-result": {
      ...common, root: { type: "string" }, output: { type: "string" }, domain: { type: "string" },
      status: { type: "string" }, "last-phase": { type: "string" }, "domain-run-id": { type: "string" },
      "failure-class": { type: "string" }, "failure-code": { type: "string" },
    },
    assemble: {
      ...common, "fd-a": { type: "string" }, "fd-b": { type: "string" }, output: { type: "string" },
      "work-dir": { type: "string" }, builder: { type: "string" }, slo: { type: "string" },
    },
  };
  if (!commandOptions[command]) fail(`unknown command: ${command}`);
  return parseArgs({ args, options: commandOptions[command], strict: true }).values;
}

export { assemble, runtimeResult, sourceManifest, verifySource, workflowOptions };

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    const command = process.argv[2];
    const values = { ...workflowOptions(), ...parse(command, process.argv.slice(3)) };
    if (command === "runtime-result") runtimeResult(values);
    else if (command === "check-runtime") validateRuntime(values.root, bindingFromOptions(values), values.domain);
    else process.exitCode = assemble(values);
  } catch (error) {
    console.error(error.stack || error.message);
    process.exitCode = 1;
  }
}
