#!/usr/bin/env node

import { readFileSync } from "node:fs";
import { verifyG6 } from "./g6-contract-lib.mjs";

const usage =
  "usage: verify-g6-evidence.mjs --slo FILE --evidence FILE --topology FILE --release-manifest FILE --expected-authority AUTHORITY --expected-environment-id ID --expected-failure-domain-class CLASS";
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
  "expected-authority",
  "expected-environment-id",
  "expected-failure-domain-class",
]) {
  if (!values[name]) throw new Error(usage);
}

try {
  const verdict = verifyG6({
    sloText: readFileSync(values.slo, "utf8"),
    evidenceText: readFileSync(values.evidence, "utf8"),
    topologyText: readFileSync(values.topology, "utf8"),
    manifestText: readFileSync(values["release-manifest"], "utf8"),
    expectedAuthority: values["expected-authority"],
    expectedEnvironmentId: values["expected-environment-id"],
    expectedFailureDomainClass: values["expected-failure-domain-class"],
  });
  process.stdout.write(`${JSON.stringify(verdict, null, 2)}\n`);
  if (!verdict.passed) process.exitCode = 1;
} catch (error) {
  process.stderr.write(`G6 evidence rejected: ${error.message}\n`);
  process.exitCode = 1;
}
