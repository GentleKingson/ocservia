#!/usr/bin/env node

import { readFileSync } from "node:fs";
import { parseSlo, structuredArtifactKinds } from "./g6-contract-lib.mjs";

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
  ["docs/acceptance/g6-evidence-schema.json", "ocservia.g6-evidence.v2"],
  ["docs/acceptance/g6-topology-schema.json", "ocservia.g6-topology.v1"],
  ["docs/acceptance/g6-verdict-schema.json", "ocservia.g6-verdict.v2"],
];
const pipelineSchemas = [
  ["docs/acceptance/g6-runtime-result-schema.json", "ocservia.g6-runtime-result.v1"],
  ["docs/acceptance/g6-source-manifest-schema.json", "ocservia.g6-source-manifest.v1"],
  ["docs/acceptance/g6-assembly-result-schema.json", "ocservia.g6-assembly-result.v1"],
  ["docs/acceptance/g6-secret-scan-result-schema.json", "ocservia.g6-secret-scan-result.v1"],
  ["docs/acceptance/g6-gate-result-schema.json", "ocservia.g6-gate-result.v1"],
  ["docs/acceptance/g6-raw-source-inventory-schema.json", "ocservia.g6-raw-source-inventory.v1"],
];
const auxiliarySchemas = [
  ["docs/acceptance/g6-builder-source-inventory-schema.json", "ocservia.g6-builder-source-inventory.v1"],
];
const rendezvousSchemas = [
  ["docs/acceptance/g6-checkpoint-schema.json", "ocservia.g6-checkpoint.v1"],
  ["docs/acceptance/g6-rendezvous-result-schema.json", "ocservia.g6-rendezvous-result.v1"],
];
const runtimeHarnessSchemas = [
  ["docs/acceptance/g6-runtime-state-schema.json", "ocservia.g6-runtime-state.v1"],
  ["docs/acceptance/g6-runtime-event-schema.json", "ocservia.g6-runtime-event.v1"],
  ["docs/acceptance/g6-phase-result-schema.json", "ocservia.g6-phase-result.v1"],
  ["docs/acceptance/g6-resource-registry-schema.json", "ocservia.g6-resource-registry.v1"],
];
const smokeSchemas = [
  ["docs/acceptance/g6-harness-smoke-result-schema.json", "ocservia.g6-harness-smoke-result.v1"],
  ["docs/acceptance/g6-harness-smoke-assembly-result-schema.json", "ocservia.g6-harness-smoke-assembly-result.v1"],
  ["docs/acceptance/g6-harness-smoke-verification-result-schema.json", "ocservia.g6-harness-smoke-verification-result.v1"],
  ["docs/acceptance/g6-harness-smoke-secret-scan-result-schema.json", "ocservia.g6-harness-smoke-secret-scan-result.v1"],
];
const phaseFailureClasses = [
  "product_assertion_failed",
  "harness_contract_failed",
  "runner_infrastructure_failed",
  "peer_failed",
  "peer_checkpoint_timeout",
  "phase_timeout",
  "phase_cancelled",
  "evidence_assembly_failed",
  "secret_scan_failed",
  "verification_failed",
  "cleanup_failed",
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

for (const [path, version] of pipelineSchemas) {
  const schema = JSON.parse(read(path));
  if (
    schema.$schema !== "https://json-schema.org/draft/2020-12/schema" ||
    schema.type !== "object" ||
    schema.additionalProperties !== false ||
    schema.properties?.schema_version?.const !== version
  ) {
    fail(`${path} must define a closed ${version} draft 2020-12 contract`);
  }
  for (const binding of [
    "candidate_sha",
    "run_id",
    "run_attempt",
    "environment_id",
    "authority",
    "release_manifest_digest",
  ]) {
    if (!schema.required?.includes(binding)) {
      fail(`${path} must require exact ${binding} binding`);
    }
  }
}

for (const [path, version] of auxiliarySchemas) {
  const schema = JSON.parse(read(path));
  if (
    schema.$schema !== "https://json-schema.org/draft/2020-12/schema" ||
    schema.type !== "object" ||
    schema.additionalProperties !== false ||
    schema.properties?.schema_version?.const !== version
  ) {
    fail(`${path} must define a closed ${version} draft 2020-12 contract`);
  }
}

for (const [path, version] of rendezvousSchemas) {
  const schema = JSON.parse(read(path));
  if (
    schema.$schema !== "https://json-schema.org/draft/2020-12/schema" ||
    schema.type !== "object" ||
    schema.additionalProperties !== false ||
    schema.properties?.schema_version?.const !== version
  ) {
    fail(`${path} must define a closed ${version} draft 2020-12 contract`);
  }
  for (const binding of [
    "candidate_sha",
    "run_id",
    "run_attempt",
    "environment_id",
    "authority",
  ]) {
    if (!schema.required?.includes(binding)) {
      fail(`${path} must require exact ${binding} binding`);
    }
  }
}

for (const [path, version] of runtimeHarnessSchemas) {
  const schema = JSON.parse(read(path));
  if (
    schema.$schema !== "https://json-schema.org/draft/2020-12/schema" ||
    schema.type !== "object" ||
    schema.additionalProperties !== false ||
    schema.properties?.schema_version?.const !== version ||
    !schema.required?.includes("domain") ||
    !schema.required?.includes("binding")
  ) {
    fail(`${path} must define a closed exact-bound ${version} draft 2020-12 contract`);
  }
  if (
    version === "ocservia.g6-phase-result.v1" &&
    !same(schema.properties.failure.properties.class.enum, phaseFailureClasses)
  ) {
    fail(`${path} failure classes drifted from the frozen harness taxonomy`);
  }
}

for (const [path, version] of smokeSchemas) {
  const schema = JSON.parse(read(path));
  const engineeringBinding = schema.$defs?.binding?.properties?.authority?.const === "engineering" ||
    schema.properties?.binding?.$ref === "g6-harness-smoke-result-schema.json#/$defs/binding";
  if (
    schema.$schema !== "https://json-schema.org/draft/2020-12/schema" ||
    schema.type !== "object" ||
    schema.additionalProperties !== false ||
    schema.properties?.schema_version?.const !== version ||
    schema.properties?.profile?.const !== "smoke" ||
    schema.properties?.formal_verdict_eligible?.const !== false ||
    !engineeringBinding
  ) {
    fail(`${path} must define a closed engineering-only non-formal ${version} contract`);
  }
}

const evidenceSchema = parsedSchemas.get("ocservia.g6-evidence.v2");
const topologySchema = parsedSchemas.get("ocservia.g6-topology.v1");
const instanceDef = topologySchema.$defs.instance;
if (!instanceDef.required.includes("component")) {
  fail("topology instances must bind a release component name");
}
const topologyRoles = instanceDef.properties.role.enum;
const requiredRoles = Object.keys(slo.topology.role_requirements ?? {});
if (requiredRoles.length === 0) {
  fail("g6-slo.yaml must freeze per-role topology requirements");
}
for (const role of requiredRoles) {
  if (!topologyRoles.includes(role)) {
    fail(`SLO role requirement role is not a topology role: ${role}`);
  }
}
for (const [first, second] of slo.topology.distinct_failure_domain_role_pairs ??
  []) {
  for (const role of [first, second]) {
    if (!topologyRoles.includes(role)) {
      fail(`distinct failure-domain pair role is not a topology role: ${role}`);
    }
  }
}
const metricNames = Object.keys(slo.metrics);
const agentInstancesMin = slo.topology.role_requirements?.agent?.instances_min;
if (
  !Number.isInteger(agentInstancesMin) ||
  agentInstancesMin !== slo.metrics.authorized_real_agents?.limit
) {
  fail(
    "agent role requirement instances_min and authorized_real_agents limit must agree",
  );
}
const derivedMetrics = Object.entries(slo.metrics)
  .filter(([, metric]) => metric.derivation !== undefined)
  .map(([name]) => name);
const declaredMetrics = Object.entries(slo.metrics)
  .filter(([, metric]) => metric.declared_by_harness === true)
  .map(([name]) => name);
if (derivedMetrics.length !== metricNames.length) {
  fail(
    "g6-slo.yaml must graduate every required metric from declared_by_harness to a verified producer",
  );
}
if (declaredMetrics.length > 0) {
  fail(
    `g6-slo.yaml still declares harness-trusted metrics: ${declaredMetrics.join(", ")}`,
  );
}
const artifactDef = evidenceSchema.$defs.artifact;
if (!artifactDef.required.includes("kind")) {
  fail("evidence artifacts must declare a verifier-recognized kind");
}
if (!same(artifactDef.properties.kind.enum, [...structuredArtifactKinds, "harness_log"])) {
  fail("evidence artifact kinds drifted from the verifier contract");
}
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
const verdictSchema = parsedSchemas.get("ocservia.g6-verdict.v2");
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
const verdictMeasurement = verdictSchema.$defs.measurement_result;
if (!verdictMeasurement.required.includes("derivation")) {
  fail("verdict measurement results must expose the producing derivation");
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

const acceptanceReadme = read("docs/acceptance/README.md");
if (!acceptanceReadme.includes("--artifact-root")) {
  fail("acceptance README must document artifact-root content verification");
}
if (!acceptanceReadme.includes("role requirements")) {
  fail("acceptance README must document topology role requirements");
}
if (!acceptanceReadme.includes("declared_by_harness")) {
  fail("acceptance README must document the harness-declared trust boundary");
}
if (
  !acceptanceReadme.includes("final pass requires verified metric producers")
) {
  fail(
    "acceptance README must document that declared metrics cannot award a final pass",
  );
}
for (const kind of structuredArtifactKinds) {
  if (!acceptanceReadme.includes(kind)) {
    fail(`acceptance README must document the ${kind} artifact category`);
  }
}
if (!acceptanceReadme.includes("environment_id")) {
  fail("acceptance README must document the per-record artifact binding");
}
for (const [, version] of pipelineSchemas) {
  if (!acceptanceReadme.includes(version)) {
    fail(`acceptance README must document the ${version} pipeline contract`);
  }
}
for (const [, version] of rendezvousSchemas) {
  if (!acceptanceReadme.includes(version)) {
    fail(`acceptance README must document the ${version} rendezvous contract`);
  }
}
for (const [, version] of smokeSchemas) {
  if (!acceptanceReadme.includes(version)) {
    fail(`acceptance README must document the ${version} smoke contract`);
  }
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
