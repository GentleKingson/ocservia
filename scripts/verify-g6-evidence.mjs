#!/usr/bin/env node

import { readFileSync, writeFileSync } from "node:fs";
import { verifyG6 } from "./g6-contract-lib.mjs";

const usage =
  "usage: verify-g6-evidence.mjs --slo FILE --evidence FILE --topology FILE --release-manifest FILE --artifact-root DIR --expected-authority AUTHORITY --expected-environment-id ID --expected-failure-domain-class CLASS [--result FILE]";
const values = {};
for (let index = 2; index < process.argv.length; index += 2) {
  const option = process.argv[index];
  const value = process.argv[index + 1];
  if (!option?.startsWith("--") || !value) throw new Error(usage);
  values[option.slice(2)] = value;
}
for (const name of [
  "slo",
  "evidence",
  "topology",
  "release-manifest",
  "artifact-root",
  "expected-authority",
  "expected-environment-id",
  "expected-failure-domain-class",
]) {
  if (!values[name]) throw new Error(usage);
}

function writeResult(result) {
  if (!values.result) return;
  writeFileSync(values.result, `${JSON.stringify(result, null, 2)}\n`);
}

try {
  const verdict = verifyG6({
    sloText: readFileSync(values.slo, "utf8"),
    evidenceText: readFileSync(values.evidence, "utf8"),
    topologyText: readFileSync(values.topology, "utf8"),
    manifestText: readFileSync(values["release-manifest"], "utf8"),
    artifactRoot: values["artifact-root"],
    expectedAuthority: values["expected-authority"],
    expectedEnvironmentId: values["expected-environment-id"],
    expectedFailureDomainClass: values["expected-failure-domain-class"],
  });
  const authorityFenceOnly =
    values["expected-authority"] === "engineering" &&
    verdict.failure_reasons.length === 1 &&
    verdict.failure_reasons[0] ===
      "final pass requires production_readiness authority" &&
    Object.values(verdict.measurement_results).every(
      (result) => result.passed,
    ) &&
    Object.values(verdict.observation_results).every((result) => result.passed);
  writeResult({
    schema_version: "ocservia.g6-evidence-phase-result.v1",
    phase: "verify",
    status: verdict.passed
      ? "passed"
      : authorityFenceOnly
        ? "accepted_non_final"
        : "failed",
    verdict_passed: verdict.passed,
    exit_code: verdict.passed ? 0 : 1,
    failure_reasons: verdict.failure_reasons,
  });
  process.stdout.write(`${JSON.stringify(verdict, null, 2)}\n`);
  if (!verdict.passed) process.exitCode = 1;
} catch (error) {
  writeResult({
    schema_version: "ocservia.g6-evidence-phase-result.v1",
    phase: "verify",
    status: "failed",
    verdict_passed: false,
    exit_code: 1,
    reason: error.message,
  });
  process.stderr.write(`G6 evidence rejected: ${error.message}\n`);
  process.exitCode = 1;
}
