#!/usr/bin/env node

import { readFileSync } from "node:fs";
import { parseSlo } from "./g6-contract-lib.mjs";

const root = new URL("../", import.meta.url);
const read = (path) => readFileSync(new URL(path, root), "utf8");
const fail = (message) => {
  throw new Error(message);
};
const sorted = (values) => [...values].sort();
const same = (left, right) =>
  JSON.stringify(sorted(left)) === JSON.stringify(sorted(right));

const slo = parseSlo(read("docs/acceptance/g6-slo.yaml"));
const schemas = [
  ["docs/acceptance/g6-evidence-schema.json", "ocservia.g6-evidence.v1"],
  ["docs/acceptance/g6-topology-schema.json", "ocservia.g6-topology.v1"],
  ["docs/acceptance/g6-verdict-schema.json", "ocservia.g6-verdict.v1"],
];
const parsedSchemas = new Map();
for (const [path, version] of schemas) {
  const schema = JSON.parse(read(path));
  parsedSchemas.set(version, schema);
  if (schema.$schema !== "https://json-schema.org/draft/2020-12/schema") {
    fail(`${path} must use JSON Schema draft 2020-12`);
  }
  if (schema.type !== "object" || schema.additionalProperties !== false) {
    fail(`${path} must define a closed top-level object`);
  }
  if (schema.properties?.schema_version?.const !== version) {
    fail(`${path} has an unexpected schema version`);
  }
  if (!schema.required?.includes("candidate_sha")) {
    fail(`${path} must bind the candidate SHA`);
  }
  if (!schema.required?.includes("release_manifest_digest")) {
    fail(`${path} must bind the release manifest digest`);
  }
}

const evidenceSchema = parsedSchemas.get("ocservia.g6-evidence.v1");
const metricNames = Object.keys(slo.metrics);
if (
  !same(evidenceSchema.properties.measurements.required, metricNames) ||
  !same(evidenceSchema.$defs.metric_name.enum, metricNames)
) {
  fail("evidence measurement fields drifted from g6-slo.yaml");
}
const observationNames = Object.keys(slo.observations);
if (
  !same(evidenceSchema.properties.observations.required, observationNames) ||
  !same(evidenceSchema.$defs.observation_name.enum, observationNames)
) {
  fail("evidence observation fields drifted from g6-slo.yaml");
}
const verdictSchema = parsedSchemas.get("ocservia.g6-verdict.v1");
if (
  verdictSchema.properties.measurement_results.minProperties !==
  metricNames.length
) {
  fail("verdict measurement cardinality drifted from g6-slo.yaml");
}
if (
  verdictSchema.properties.observation_results.minProperties !==
  observationNames.length
) {
  fail("verdict observation cardinality drifted from g6-slo.yaml");
}
for (const forbidden of ["limit", "comparison", "unit", "passed"]) {
  if (forbidden in evidenceSchema.$defs.measurement.properties) {
    fail(`untrusted evidence measurement must not contain ${forbidden}`);
  }
}
if ("passed" in evidenceSchema.properties) {
  fail("untrusted evidence must not declare a top-level verdict");
}
if (read("docs/acceptance/g6-topology-schema.json").includes("public_labels")) {
  fail("topology schema must not accept arbitrary public labels");
}

const publicCapacity = read("docs/development/p1-resilience-capacity.md");
const conflictingLongGate =
  /(?:24[- ]hour|24\s*\u5c0f\u65f6)[^\n]{0,120}(?:release|G6)[^\n]{0,40}gate/i;
if (conflictingLongGate.test(publicCapacity)) {
  fail("public capacity documentation still describes a 24-hour release gate");
}
if (!publicCapacity.includes("continuous 300-second")) {
  fail(
    "public capacity documentation must identify the bounded 300-second G6 gate",
  );
}

console.log("G6 acceptance contracts are structurally valid and synchronized");
