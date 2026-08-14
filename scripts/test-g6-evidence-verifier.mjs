#!/usr/bin/env node

import {
  cpSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { sha256Digest, verifyG6 } from "./g6-contract-lib.mjs";

const root = new URL("../", import.meta.url);
const read = (path) => readFileSync(new URL(path, root), "utf8");
const parse = (path) => JSON.parse(read(path));
const clone = (value) => structuredClone(value);
const serialize = (value) => `${JSON.stringify(value, null, 2)}\n`;

function mutate(target, mutation) {
  const parts = mutation.path.split(".");
  const key = parts.pop();
  let parent = target;
  for (const part of parts) parent = parent[part];
  if (mutation.operation === "replace" && !(key in parent)) {
    throw new Error(`replace target is missing: ${mutation.path}`);
  }
  if (!new Set(["add", "replace"]).has(mutation.operation)) {
    throw new Error(`unsupported fixture mutation: ${mutation.operation}`);
  }
  parent[key] = mutation.value;
}

const allowedFixtureFields = new Set([
  "base",
  "topology_mutations",
  "evidence_mutations",
  "mutations",
  "expected_error",
  "expected_failure_reason",
  "expected_authority",
  "expected_failure_domain_class",
  "artifact_root",
  "artifact_files",
]);

function buildArtifactRoot(mode, overrides) {
  const realRoot = fileURLToPath(new URL("testdata/g6/artifacts/", root));
  const overrideEntries = Object.entries(overrides ?? {});
  if (mode !== "symlink" && overrideEntries.length === 0) return realRoot;
  const tempRoot = mkdtempSync(join(tmpdir(), "g6-artifacts-"));
  cpSync(realRoot, tempRoot, { recursive: true });
  for (const [name, content] of overrideEntries) {
    writeFileSync(join(tempRoot, name), content);
  }
  if (mode === "symlink") {
    rmSync(join(tempRoot, "resource-samples.csv"));
    symlinkSync(
      join(realRoot, "resource-samples.csv"),
      join(tempRoot, "resource-samples.csv"),
    );
  }
  return tempRoot;
}

const sloText = read("docs/acceptance/g6-slo.yaml");
const manifestText = read("testdata/g6/release-manifest.json");
const baseTopology = parse("testdata/g6/topology.json");
const baseEvidence = parse("testdata/g6/evidence-pass.json");
const baseVerdict = verifyG6({
  sloText,
  evidenceText: serialize(baseEvidence),
  topologyText: serialize(baseTopology),
  manifestText,
  artifactRoot: buildArtifactRoot(),
  expectedAuthority: "production_readiness",
  expectedEnvironmentId: "g6-12345678",
  expectedFailureDomainClass: "multi_host",
});
if (baseVerdict.passed) {
  throw new Error(
    "positive fixture must not produce a final G6 pass while declared metrics remain",
  );
}
const unresolvedReasons = baseVerdict.failure_reasons.filter(
  (reason) => reason !== "final pass requires verified metric producers",
);
if (unresolvedReasons.length > 0) {
  throw new Error(
    `positive fixture has unresolved failure reasons: ${unresolvedReasons.join("; ")}`,
  );
}
for (const [name, result] of Object.entries(baseVerdict.measurement_results)) {
  if (!result.passed) {
    throw new Error(`positive fixture metric did not pass: ${name}`);
  }
}
for (const [name, result] of Object.entries(baseVerdict.observation_results)) {
  if (!result.passed) {
    throw new Error(`positive fixture observation did not pass: ${name}`);
  }
}

const fixtureDirectory = new URL("../testdata/g6/", import.meta.url);
const cases = readdirSync(fixtureDirectory)
  .filter((name) => name.startsWith("evidence-") && name.endsWith(".json"))
  .filter((name) => name !== "evidence-pass.json")
  .sort();

for (const name of cases) {
  const fixture = parse(`testdata/g6/${name}`);
  for (const field of Object.keys(fixture)) {
    if (!allowedFixtureFields.has(field)) {
      throw new Error(`${name}: unsupported fixture field: ${field}`);
    }
  }
  const evidence = clone(baseEvidence);
  const topology = clone(baseTopology);
  for (const mutation of fixture.topology_mutations ?? [])
    mutate(topology, mutation);
  for (const mutation of fixture.evidence_mutations ?? [])
    mutate(evidence, mutation);
  for (const mutation of fixture.mutations ?? []) mutate(evidence, mutation);

  const topologyText = serialize(topology);
  if ((fixture.topology_mutations ?? []).length > 0) {
    evidence.topology_digest = sha256Digest(topologyText);
  }

  const artifactRoot = buildArtifactRoot(
    fixture.artifact_root,
    fixture.artifact_files,
  );
  let verdict;
  let rejected;
  try {
    verdict = verifyG6({
      sloText,
      evidenceText: serialize(evidence),
      topologyText,
      manifestText,
      artifactRoot,
      expectedAuthority: fixture.expected_authority ?? "production_readiness",
      expectedEnvironmentId: "g6-12345678",
      expectedFailureDomainClass:
        fixture.expected_failure_domain_class ?? "multi_host",
    });
  } catch (error) {
    rejected = error;
  } finally {
    if (artifactRoot.startsWith(join(tmpdir(), "g6-artifacts-"))) {
      rmSync(artifactRoot, { recursive: true, force: true });
    }
  }

  if (fixture.expected_error) {
    if (!rejected?.message.includes(fixture.expected_error)) {
      throw new Error(
        `${name}: expected rejection containing ${fixture.expected_error}`,
      );
    }
    continue;
  }
  if (rejected) throw rejected;
  if (verdict.passed)
    throw new Error(`${name}: forged or failing evidence produced PASS`);
  if (!verdict.failure_reasons.includes(fixture.expected_failure_reason)) {
    throw new Error(`${name}: missing expected failure reason`);
  }
}

console.log(
  `G6 verifier computed the positive fixture verdict (final pass blocked by declared metrics) and rejected ${cases.length} negative fixtures`,
);
