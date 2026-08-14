#!/usr/bin/env node

import { createRequire } from "node:module";
import { readFileSync } from "node:fs";

const require = createRequire(new URL("../web/package.json", import.meta.url));
const { parseDocument } = require("yaml");

const root = new URL("../", import.meta.url);
const read = (path) => readFileSync(new URL(path, root), "utf8");
const fail = (message) => {
  throw new Error(message);
};

const sloDocument = parseDocument(read("docs/acceptance/g6-slo.yaml"), {
  prettyErrors: true,
  uniqueKeys: true,
});
if (sloDocument.errors.length > 0) {
  fail(sloDocument.errors.map((error) => error.message).join("\n"));
}
const slo = sloDocument.toJS();
if (slo.schema_version !== "ocservia.g6-slo.v1") {
  fail("unexpected G6 SLO schema_version");
}

const requiredSections = [
  "stability",
  "capacity",
  "correctness",
  "recovery",
  "service",
  "resources",
  "required_observations",
];
for (const section of requiredSections) {
  if (!slo[section] || typeof slo[section] !== "object") {
    fail(`missing G6 SLO section: ${section}`);
  }
}

for (const [section, values] of Object.entries(slo)) {
  if (section === "schema_version") continue;
  for (const [name, value] of Object.entries(values)) {
    if (section === "required_observations") {
      if (value !== true) fail(`required observation must be true: ${name}`);
      continue;
    }
    if (typeof value !== "number" || !Number.isFinite(value) || value < 0) {
      fail(
        `threshold must be a non-negative finite number: ${section}.${name}`,
      );
    }
    if (name.includes("ratio_") && value > 1) {
      fail(`ratio threshold must not exceed one: ${section}.${name}`);
    }
  }
}

const schemas = [
  ["docs/acceptance/g6-evidence-schema.json", "ocservia.g6-evidence.v1"],
  ["docs/acceptance/g6-topology-schema.json", "ocservia.g6-topology.v1"],
];
for (const [path, version] of schemas) {
  const schema = JSON.parse(read(path));
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

  if (version === "ocservia.g6-evidence.v1") {
    const expectedMeasurements = Object.entries(slo)
      .filter(
        ([section]) =>
          !["schema_version", "required_observations"].includes(section),
      )
      .flatMap(([section, thresholds]) =>
        Object.keys(thresholds)
          .filter(
            (name) =>
              !(section === "stability" && name === "sample_interval_seconds"),
          )
          .map((name) => {
            if (section === "stability") return `stability_${name}`;
            if (name === "production_command_inflight_min") {
              return "max_production_command_inflight";
            }
            return name.replace(/_(?:min|max)$/, "");
          }),
      )
      .sort();
    const requiredMeasurements = [
      ...schema.properties.measurements.required,
    ].sort();
    const allowedMeasurements = [
      ...schema.properties.measurements.propertyNames.enum,
    ].sort();
    if (
      JSON.stringify(requiredMeasurements) !==
      JSON.stringify(expectedMeasurements)
    ) {
      fail(`${path} required measurements drifted from g6-slo.yaml`);
    }
    if (
      JSON.stringify(allowedMeasurements) !==
      JSON.stringify(expectedMeasurements)
    ) {
      fail(`${path} allowed measurements drifted from g6-slo.yaml`);
    }

    const expectedObservations = Object.keys(slo.required_observations).sort();
    const requiredObservations = [
      ...schema.properties.observations.required,
    ].sort();
    const allowedObservations = [
      ...schema.properties.observations.propertyNames.enum,
    ].sort();
    if (
      JSON.stringify(requiredObservations) !==
      JSON.stringify(expectedObservations)
    ) {
      fail(`${path} required observations drifted from g6-slo.yaml`);
    }
    if (
      JSON.stringify(allowedObservations) !==
      JSON.stringify(expectedObservations)
    ) {
      fail(`${path} allowed observations drifted from g6-slo.yaml`);
    }
  }
}

console.log("G6 acceptance contracts are structurally valid");
