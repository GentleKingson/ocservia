#!/usr/bin/env node

import assert from "node:assert/strict";
import { mkdtempSync, mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";
import {
  ASSEMBLY_SCHEMA,
  SECRET_SCAN_SCHEMA,
  assemble,
  gate,
  runtimeResult,
  verifySource,
} from "./g6-pipeline.mjs";

const root = mkdtempSync(join(tmpdir(), "ocservia-g6-pipeline-"));
const binding = {
  "candidate-sha": "a".repeat(40),
  "run-id": "424242",
  "run-attempt": "3",
  "environment-id": "g6-0123456789abcdef",
  authority: "engineering",
  "release-manifest-digest": "f".repeat(64),
};
const normalizedBinding = {
  candidate_sha: binding["candidate-sha"],
  run_id: binding["run-id"],
  run_attempt: 3,
  environment_id: binding["environment-id"],
  authority: binding.authority,
  release_manifest_digest: binding["release-manifest-digest"],
};

function json(path, value) {
  writeFileSync(path, `${JSON.stringify(value, null, 2)}\n`);
}

function gateInputs(directory, authority = "engineering") {
  const exact = { ...normalizedBinding, authority };
  const paths = {
    fdA: join(directory, "fd-a.json"),
    fdB: join(directory, "fd-b.json"),
    assembly: join(directory, "assembly.json"),
    scan: join(directory, "scan.json"),
    verification: join(directory, "verification.json"),
    output: join(directory, "gate.json"),
  };
  json(paths.fdA, {
    schema_version: "ocservia.g6-runtime-result.v1",
    ...exact,
    failure_domain: "fd-a",
    status: "passed",
  });
  json(paths.fdB, {
    schema_version: "ocservia.g6-runtime-result.v1",
    ...exact,
    failure_domain: "fd-b",
    status: "passed",
  });
  json(paths.assembly, { schema_version: ASSEMBLY_SCHEMA, ...exact, status: "passed" });
  json(paths.scan, { schema_version: SECRET_SCAN_SCHEMA, ...exact, status: "passed" });
  json(paths.verification, {
    schema_version: "ocservia.g6-evidence-phase-result.v1",
    ...exact,
    phase: "verify",
    status: authority === "production_readiness" ? "passed" : "accepted_non_final",
  });
  return {
    ...binding,
    authority,
    output: paths.output,
    "fd-a-result": paths.fdA,
    "fd-b-result": paths.fdB,
    "assembly-result": paths.assembly,
    "secret-scan-result": paths.scan,
    "verification-result": paths.verification,
  };
}

try {
  for (const domain of ["fd-a", "fd-b"]) {
    const directory = join(root, domain);
    mkdirSync(join(directory, "evidence"), { recursive: true });
    writeFileSync(join(directory, "evidence", "sample.txt"), `${domain}\n`);
    runtimeResult({
      ...binding,
      root: directory,
      output: join(directory, "runtime-result.json"),
      domain,
      status: "passed",
      "domain-run-id": `${binding["run-id"]}-${domain}`,
    });
    verifySource(directory, normalizedBinding, domain);
  }

  const tampered = join(root, "fd-a", "evidence", "sample.txt");
  writeFileSync(tampered, "tampered\n");
  assert.throws(
    () => verifySource(join(root, "fd-a"), normalizedBinding, "fd-a"),
    /source payload mismatch/,
  );

  const partialSources = join(root, "partial-sources");
  for (const domain of ["fd-a", "fd-b"]) {
    const directory = join(partialSources, domain);
    mkdirSync(join(directory, "evidence"), { recursive: true });
    writeFileSync(join(directory, "evidence", "sample.txt"), `${domain}\n`);
    runtimeResult({
      ...binding,
      root: directory,
      domain,
      status: domain === "fd-a" ? "failed" : "passed",
      "failure-class": "fixture_failure",
      "failure-code": "fixture_runtime_failed",
    });
  }
  const partialOutput = join(root, "partial-output");
  assert.equal(assemble({
    ...binding,
    "fd-a": join(partialSources, "fd-a"),
    "fd-b": join(partialSources, "fd-b"),
    output: partialOutput,
    "work-dir": join(root, "partial-work"),
    builder: join(root, "builder-must-not-run.mjs"),
    slo: join(root, "slo-must-not-be-read.yaml"),
    "fd-a-artifact-id": "1001",
    "fd-a-artifact-digest": "1".repeat(64),
    "fd-b-artifact-id": "1002",
    "fd-b-artifact-digest": "2".repeat(64),
  }), 1);
  assert.equal(
    JSON.parse(readFileSync(join(partialOutput, "assembly-result.json"))).reason,
    "runtime evidence is incomplete",
  );
  assert.equal(
    JSON.parse(readFileSync(join(partialOutput, "source-inventory.json"))).sources.length,
    2,
  );

  const builderFailureSources = join(root, "builder-failure-sources");
  for (const domain of ["fd-a", "fd-b"]) {
    const directory = join(builderFailureSources, domain);
    mkdirSync(join(directory, "evidence", "effects"), { recursive: true });
    writeFileSync(join(directory, "evidence", "sample.txt"), `${domain}\n`);
    if (domain === "fd-a") {
      writeFileSync(join(directory, "evidence", "effects", "agent.tsv"), "effect\n");
    }
    runtimeResult({ ...binding, root: directory, domain, status: "passed" });
  }
  const failingBuilder = join(root, "failing-builder.mjs");
  writeFileSync(failingBuilder, 'console.error("injected builder failure"); process.exit(17);\n');
  const builderFailureOutput = join(root, "builder-failure-output");
  assert.equal(assemble({
    ...binding,
    "fd-a": join(builderFailureSources, "fd-a"),
    "fd-b": join(builderFailureSources, "fd-b"),
    output: builderFailureOutput,
    "work-dir": join(root, "builder-failure-work"),
    builder: failingBuilder,
    slo: join(root, "unused-slo.yaml"),
    "fd-a-artifact-id": "2001",
    "fd-a-artifact-digest": "3".repeat(64),
    "fd-b-artifact-id": "2002",
    "fd-b-artifact-digest": "4".repeat(64),
  }), 17);
  const builderFailure = JSON.parse(
    readFileSync(join(builderFailureOutput, "assembly-result.json")),
  );
  assert.equal(builderFailure.status, "failed");
  assert.equal(builderFailure.exit_code, 17);
  assert.match(builderFailure.reason, /injected builder failure/);
  assert.equal(readFileSync(join(builderFailureOutput, "evidence-build-exit-code.txt"), "utf8"), "17\n");

  const missingOutput = join(root, "missing-output");
  assert.equal(assemble({
    ...binding,
    "fd-a": join(root, "missing-fd-a"),
    "fd-b": join(builderFailureSources, "fd-b"),
    output: missingOutput,
    "work-dir": join(root, "missing-work"),
    builder: failingBuilder,
    slo: join(root, "unused-slo.yaml"),
    "fd-b-artifact-id": "2002",
    "fd-b-artifact-digest": "4".repeat(64),
  }), 1);
  const missingInventory = JSON.parse(
    readFileSync(join(missingOutput, "source-inventory.json")),
  );
  assert.equal(missingInventory.sources[0].runtime_status, "unavailable");
  assert.equal(missingInventory.sources[0].artifact_id, null);
  assert.match(
    JSON.parse(readFileSync(join(missingOutput, "assembly-result.json"))).reason,
    /raw source validation failed/,
  );

  const engineering = join(root, "engineering");
  mkdirSync(engineering);
  const engineeringInputs = gateInputs(engineering);
  assert.equal(gate(engineeringInputs), 0);
  assert.equal(JSON.parse(readFileSync(engineeringInputs.output)).final_status, "accepted_non_final");

  const production = join(root, "production");
  mkdirSync(production);
  const productionInputs = gateInputs(production, "production_readiness");
  assert.equal(gate(productionInputs), 0);
  assert.equal(JSON.parse(readFileSync(productionInputs.output)).final_status, "passed");

  const productionNonFinal = join(root, "production-non-final");
  mkdirSync(productionNonFinal);
  const productionNonFinalInputs = gateInputs(productionNonFinal, "production_readiness");
  const productionVerification = JSON.parse(
    readFileSync(productionNonFinalInputs["verification-result"]),
  );
  productionVerification.status = "accepted_non_final";
  json(productionNonFinalInputs["verification-result"], productionVerification);
  assert.equal(gate(productionNonFinalInputs), 1);
  assert.equal(
    JSON.parse(readFileSync(productionNonFinalInputs.output)).final_status,
    "failed",
  );

  const failed = join(root, "failed");
  mkdirSync(failed);
  const failedInputs = gateInputs(failed);
  const failedAssembly = JSON.parse(readFileSync(failedInputs["assembly-result"]));
  failedAssembly.status = "failed";
  json(failedInputs["assembly-result"], failedAssembly);
  assert.equal(gate(failedInputs), 1);
  assert.equal(JSON.parse(readFileSync(failedInputs.output)).final_status, "failed");

  const mismatched = join(root, "mismatched");
  mkdirSync(mismatched);
  const mismatchedInputs = gateInputs(mismatched);
  const scan = JSON.parse(readFileSync(mismatchedInputs["secret-scan-result"]));
  scan.run_attempt = 4;
  json(mismatchedInputs["secret-scan-result"], scan);
  assert.throws(() => gate(mismatchedInputs), /secret scan run_attempt mismatch/);

  console.log("G6 pipeline tests passed");
} finally {
  rmSync(root, { recursive: true, force: true });
}
