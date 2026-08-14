#!/usr/bin/env node

import { readdirSync, readFileSync } from "node:fs";
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

const sloText = read("docs/acceptance/g6-slo.yaml");
const manifestText = read("testdata/g6/release-manifest.json");
const baseTopology = parse("testdata/g6/topology.json");
const baseEvidence = parse("testdata/g6/evidence-pass.json");
const baseVerdict = verifyG6({
  sloText,
  evidenceText: serialize(baseEvidence),
  topologyText: serialize(baseTopology),
  manifestText,
  expectedAuthority: "production_readiness",
  expectedEnvironmentId: "g6-12345678",
  expectedFailureDomainClass: "multi_host",
});
if (!baseVerdict.passed)
  throw new Error("positive G6 evidence fixture did not pass");

const fixtureDirectory = new URL("../testdata/g6/", import.meta.url);
const cases = readdirSync(fixtureDirectory)
  .filter((name) => name.startsWith("evidence-") && name.endsWith(".json"))
  .filter((name) => name !== "evidence-pass.json")
  .sort();

for (const name of cases) {
  const fixture = parse(`testdata/g6/${name}`);
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

  let verdict;
  let rejected;
  try {
    verdict = verifyG6({
      sloText,
      evidenceText: serialize(evidence),
      topologyText,
      manifestText,
      expectedAuthority: fixture.expected_authority ?? "production_readiness",
      expectedEnvironmentId: "g6-12345678",
      expectedFailureDomainClass:
        fixture.expected_failure_domain_class ?? "multi_host",
    });
  } catch (error) {
    rejected = error;
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
  `G6 verifier accepted the positive fixture and rejected ${cases.length} negative fixtures`,
);
