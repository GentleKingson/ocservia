import { createHash } from "node:crypto";
import { createRequire } from "node:module";

const require = createRequire(new URL("../web/package.json", import.meta.url));
const { parseDocument } = require("yaml");

const comparisons = new Set(["lte", "gte", "eq"]);
const units = new Set(["seconds", "count", "ratio"]);
const authorities = new Set(["engineering", "production_readiness"]);
const failureDomainClasses = new Set([
  "single_host",
  "multi_host",
  "multi_zone",
  "multi_region",
]);
const roles = new Set([
  "api",
  "worker",
  "scheduler",
  "transportd",
  "agent",
  "privd",
  "relay",
  "postgres_primary",
  "postgres_standby",
  "postgres_dcs",
  "wal_archive",
]);
const shaPattern = /^[0-9a-f]{40}$/;
const digestPattern = /^sha256:[0-9a-f]{64}$/;
const environmentPattern = /^g6-[a-z0-9]{8,32}$/;
const faultDomainPattern = /^fd-[a-z0-9]{2,32}$/;
const eventPattern = /^[a-z][a-z0-9_]{0,127}$/;

function fail(message) {
  throw new Error(message);
}

function object(value, context) {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    fail(`${context} must be an object`);
  }
  return value;
}

function closed(value, allowed, context) {
  object(value, context);
  const extras = Object.keys(value).filter((key) => !allowed.includes(key));
  if (extras.length > 0)
    fail(`${context} has unsupported fields: ${extras.join(", ")}`);
  const missing = allowed.filter((key) => !(key in value));
  if (missing.length > 0)
    fail(`${context} is missing fields: ${missing.join(", ")}`);
}

function exactKeys(value, expected, context) {
  object(value, context);
  const actual = Object.keys(value).sort();
  const wanted = [...expected].sort();
  if (JSON.stringify(actual) !== JSON.stringify(wanted)) {
    fail(`${context} fields do not match the G6 contract`);
  }
}

function finiteNumber(value, context) {
  if (typeof value !== "number" || !Number.isFinite(value) || value < 0) {
    fail(`${context} must be a non-negative finite number`);
  }
}

function digest(value, context) {
  if (typeof value !== "string" || !digestPattern.test(value)) {
    fail(`${context} must be a sha256 digest`);
  }
}

function sha(value, context) {
  if (typeof value !== "string" || !shaPattern.test(value)) {
    fail(`${context} must be a lowercase 40-character commit SHA`);
  }
}

function timestamp(value, context) {
  if (typeof value !== "string" || !Number.isFinite(Date.parse(value))) {
    fail(`${context} must be an RFC 3339 timestamp`);
  }
}

export function sha256Digest(text) {
  return `sha256:${createHash("sha256").update(text).digest("hex")}`;
}

export function parseJSON(text, context) {
  try {
    return JSON.parse(text);
  } catch (error) {
    fail(`${context} is not valid JSON: ${error.message}`);
  }
}

export function parseSlo(text) {
  const document = parseDocument(text, {
    prettyErrors: true,
    uniqueKeys: true,
  });
  if (document.errors.length > 0) {
    fail(document.errors.map((error) => error.message).join("\n"));
  }
  const slo = document.toJS();
  closed(slo, ["schema_version", "topology", "metrics", "observations"], "SLO");
  if (slo.schema_version !== "ocservia.g6-slo.v1") {
    fail("unexpected G6 SLO schema_version");
  }

  closed(
    slo.topology,
    ["final_pass_failure_domain_classes", "failure_domains_min"],
    "SLO topology",
  );
  if (
    !Array.isArray(slo.topology.final_pass_failure_domain_classes) ||
    slo.topology.final_pass_failure_domain_classes.length === 0 ||
    slo.topology.final_pass_failure_domain_classes.some(
      (value) => !failureDomainClasses.has(value) || value === "single_host",
    )
  ) {
    fail("SLO topology final-pass classes must be non-single-host classes");
  }
  if (
    !Number.isInteger(slo.topology.failure_domains_min) ||
    slo.topology.failure_domains_min < 2
  ) {
    fail("SLO topology must require at least two failure domains");
  }

  object(slo.metrics, "SLO metrics");
  if (Object.keys(slo.metrics).length === 0)
    fail("SLO metrics must not be empty");
  for (const [name, metric] of Object.entries(slo.metrics)) {
    if (!eventPattern.test(name)) fail(`invalid SLO metric name: ${name}`);
    closed(
      metric,
      ["limit", "comparison", "unit", "scope"],
      `SLO metric ${name}`,
    );
    finiteNumber(metric.limit, `SLO metric ${name}.limit`);
    if (!comparisons.has(metric.comparison))
      fail(`invalid comparator for SLO metric ${name}`);
    if (!units.has(metric.unit)) fail(`invalid unit for SLO metric ${name}`);
    if (typeof metric.scope !== "string" || metric.scope.length === 0) {
      fail(`SLO metric ${name} must define scope`);
    }
  }

  object(slo.observations, "SLO observations");
  if (Object.keys(slo.observations).length === 0)
    fail("SLO observations must not be empty");
  for (const [name, observation] of Object.entries(slo.observations)) {
    closed(
      observation,
      ["required", "scope", "required_timeline_events"],
      `SLO observation ${name}`,
    );
    if (observation.required !== true)
      fail(`SLO observation ${name} must be required`);
    if (
      typeof observation.scope !== "string" ||
      observation.scope.length === 0
    ) {
      fail(`SLO observation ${name} must define scope`);
    }
    if (
      !Array.isArray(observation.required_timeline_events) ||
      observation.required_timeline_events.length === 0 ||
      new Set(observation.required_timeline_events).size !==
        observation.required_timeline_events.length ||
      observation.required_timeline_events.some(
        (event) => !eventPattern.test(event),
      )
    ) {
      fail(`SLO observation ${name} has invalid required timeline events`);
    }
  }
  return slo;
}

function validateEvidence(evidence, slo) {
  closed(
    evidence,
    [
      "schema_version",
      "candidate_sha",
      "release_manifest_digest",
      "slo_contract_digest",
      "topology_digest",
      "started_at",
      "finished_at",
      "environment",
      "measurements",
      "observations",
      "artifacts",
    ],
    "evidence",
  );
  if (evidence.schema_version !== "ocservia.g6-evidence.v1") {
    fail("unexpected G6 evidence schema_version");
  }
  sha(evidence.candidate_sha, "evidence candidate_sha");
  digest(evidence.release_manifest_digest, "evidence release_manifest_digest");
  digest(evidence.slo_contract_digest, "evidence slo_contract_digest");
  digest(evidence.topology_digest, "evidence topology_digest");
  timestamp(evidence.started_at, "evidence started_at");
  timestamp(evidence.finished_at, "evidence finished_at");
  if (Date.parse(evidence.finished_at) <= Date.parse(evidence.started_at)) {
    fail("evidence finished_at must be later than started_at");
  }

  const allowedEnvironmentFields = [
    "environment_id",
    "failure_domain_class",
    "authority",
    ...(evidence.environment?.limitations === undefined ? [] : ["limitations"]),
  ];
  closed(
    evidence.environment,
    allowedEnvironmentFields,
    "evidence environment",
  );
  if (!environmentPattern.test(evidence.environment.environment_id)) {
    fail("evidence environment_id must be an opaque G6 identifier");
  }
  if (!failureDomainClasses.has(evidence.environment.failure_domain_class)) {
    fail("invalid evidence failure_domain_class");
  }
  if (!authorities.has(evidence.environment.authority))
    fail("invalid evidence authority");
  if (
    evidence.environment.limitations !== undefined &&
    (!Array.isArray(evidence.environment.limitations) ||
      evidence.environment.limitations.some((item) => typeof item !== "string"))
  ) {
    fail("evidence limitations must be strings");
  }

  exactKeys(
    evidence.measurements,
    Object.keys(slo.metrics),
    "evidence measurements",
  );
  for (const [name, measurement] of Object.entries(evidence.measurements)) {
    closed(
      measurement,
      ["actual", "sample_count", "source_artifact_digest"],
      `evidence measurement ${name}`,
    );
    finiteNumber(measurement.actual, `evidence measurement ${name}.actual`);
    if (
      !Number.isInteger(measurement.sample_count) ||
      measurement.sample_count < 1
    ) {
      fail(`evidence measurement ${name}.sample_count must be positive`);
    }
    digest(
      measurement.source_artifact_digest,
      `evidence measurement ${name} source`,
    );
  }

  exactKeys(
    evidence.observations,
    Object.keys(slo.observations),
    "evidence observations",
  );
  for (const [name, observation] of Object.entries(evidence.observations)) {
    const allowed = [
      "observed",
      "timeline_event_ids",
      "source_artifact_digest",
      ...(observation.notes === undefined ? [] : ["notes"]),
    ];
    closed(observation, allowed, `evidence observation ${name}`);
    if (typeof observation.observed !== "boolean") {
      fail(`evidence observation ${name}.observed must be boolean`);
    }
    if (
      !Array.isArray(observation.timeline_event_ids) ||
      observation.timeline_event_ids.length === 0 ||
      new Set(observation.timeline_event_ids).size !==
        observation.timeline_event_ids.length ||
      observation.timeline_event_ids.some((event) => !eventPattern.test(event))
    ) {
      fail(`evidence observation ${name} has invalid timeline events`);
    }
    digest(
      observation.source_artifact_digest,
      `evidence observation ${name} source`,
    );
  }

  if (!Array.isArray(evidence.artifacts) || evidence.artifacts.length === 0) {
    fail("evidence artifacts must not be empty");
  }
  const artifactDigests = new Set();
  for (const artifact of evidence.artifacts) {
    closed(artifact, ["name", "digest", "media_type"], "evidence artifact");
    if (
      typeof artifact.name !== "string" ||
      !/^[a-z0-9][a-z0-9._-]{0,127}$/.test(artifact.name)
    ) {
      fail("evidence artifact has an invalid public name");
    }
    digest(artifact.digest, `evidence artifact ${artifact.name} digest`);
    if (
      typeof artifact.media_type !== "string" ||
      artifact.media_type.length === 0
    ) {
      fail(`evidence artifact ${artifact.name} has an invalid media type`);
    }
    artifactDigests.add(artifact.digest);
  }
  for (const [name, measurement] of Object.entries(evidence.measurements)) {
    if (!artifactDigests.has(measurement.source_artifact_digest)) {
      fail(`evidence measurement ${name} references an undeclared artifact`);
    }
  }
  for (const [name, observation] of Object.entries(evidence.observations)) {
    if (!artifactDigests.has(observation.source_artifact_digest)) {
      fail(`evidence observation ${name} references an undeclared artifact`);
    }
  }
}

function validateTopology(topology) {
  closed(
    topology,
    [
      "schema_version",
      "candidate_sha",
      "release_manifest_digest",
      "environment_id",
      "failure_domain_class",
      "instances",
    ],
    "topology",
  );
  if (topology.schema_version !== "ocservia.g6-topology.v1") {
    fail("unexpected G6 topology schema_version");
  }
  sha(topology.candidate_sha, "topology candidate_sha");
  digest(topology.release_manifest_digest, "topology release_manifest_digest");
  if (!environmentPattern.test(topology.environment_id))
    fail("topology environment_id is not opaque");
  if (!failureDomainClasses.has(topology.failure_domain_class)) {
    fail("invalid topology failure_domain_class");
  }
  if (!Array.isArray(topology.instances) || topology.instances.length === 0) {
    fail("topology instances must not be empty");
  }
  const ids = new Set();
  for (const instance of topology.instances) {
    const allowed = [
      "instance_id",
      "fault_domain",
      "role",
      "component_digest",
      "candidate_sha",
      "started_at",
      ...(instance.stopped_at === undefined ? [] : ["stopped_at"]),
    ];
    closed(instance, allowed, "topology instance");
    if (
      typeof instance.instance_id !== "string" ||
      ids.has(instance.instance_id)
    ) {
      fail("topology instance IDs must be unique strings");
    }
    ids.add(instance.instance_id);
    if (!faultDomainPattern.test(instance.fault_domain))
      fail("topology fault_domain is not opaque");
    if (!roles.has(instance.role))
      fail(`invalid topology role: ${instance.role}`);
    digest(instance.component_digest, "topology component_digest");
    sha(instance.candidate_sha, "topology instance candidate_sha");
    timestamp(instance.started_at, "topology instance started_at");
    if (instance.stopped_at !== undefined)
      timestamp(instance.stopped_at, "topology instance stopped_at");
  }
}

function validateManifest(manifest) {
  object(manifest, "release manifest");
  sha(manifest.candidate_sha, "release manifest candidate_sha");
  if (!Array.isArray(manifest.components) || manifest.components.length === 0) {
    fail("release manifest components must not be empty");
  }
  const digests = new Set();
  for (const component of manifest.components) {
    object(component, "release manifest component");
    if (typeof component.name !== "string" || component.name.length === 0) {
      fail("release manifest component name is invalid");
    }
    digest(component.digest, `release manifest component ${component.name}`);
    digests.add(component.digest);
  }
  return digests;
}

function metricPass(actual, contract) {
  if (contract.comparison === "lte") return actual <= contract.limit;
  if (contract.comparison === "gte") return actual >= contract.limit;
  return actual === contract.limit;
}

export function verifyG6({
  sloText,
  evidenceText,
  topologyText,
  manifestText,
  expectedAuthority,
  expectedEnvironmentId,
  expectedFailureDomainClass,
}) {
  const slo = parseSlo(sloText);
  const evidence = parseJSON(evidenceText, "evidence");
  const topology = parseJSON(topologyText, "topology");
  const manifest = parseJSON(manifestText, "release manifest");
  validateEvidence(evidence, slo);
  validateTopology(topology);
  const manifestComponentDigests = validateManifest(manifest);

  if (!authorities.has(expectedAuthority)) fail("invalid expected authority");
  if (!environmentPattern.test(expectedEnvironmentId)) {
    fail("invalid expected environment identifier");
  }
  if (!failureDomainClasses.has(expectedFailureDomainClass)) {
    fail("invalid expected failure-domain class");
  }
  if (evidence.environment.authority !== expectedAuthority) {
    fail("evidence authority does not match the verifier context");
  }
  if (evidence.environment.environment_id !== expectedEnvironmentId) {
    fail("evidence environment does not match the verifier context");
  }
  if (
    evidence.environment.failure_domain_class !== expectedFailureDomainClass
  ) {
    fail("evidence failure-domain class does not match the verifier context");
  }

  const computedSloDigest = sha256Digest(sloText);
  const computedTopologyDigest = sha256Digest(topologyText);
  const computedManifestDigest = sha256Digest(manifestText);
  if (evidence.slo_contract_digest !== computedSloDigest)
    fail("SLO digest mismatch");
  if (evidence.topology_digest !== computedTopologyDigest)
    fail("topology digest mismatch");
  if (evidence.release_manifest_digest !== computedManifestDigest) {
    fail("release manifest digest mismatch");
  }
  if (topology.release_manifest_digest !== computedManifestDigest) {
    fail("topology release manifest digest mismatch");
  }
  if (
    evidence.candidate_sha !== topology.candidate_sha ||
    evidence.candidate_sha !== manifest.candidate_sha ||
    topology.instances.some(
      (instance) => instance.candidate_sha !== evidence.candidate_sha,
    )
  ) {
    fail("candidate SHA mismatch");
  }
  if (evidence.environment.environment_id !== topology.environment_id) {
    fail("environment identifier mismatch");
  }
  if (
    evidence.environment.failure_domain_class !== topology.failure_domain_class
  ) {
    fail("failure-domain class mismatch");
  }
  for (const instance of topology.instances) {
    if (!manifestComponentDigests.has(instance.component_digest)) {
      fail(
        `topology component digest is absent from release manifest: ${instance.instance_id}`,
      );
    }
  }

  const failureReasons = [];
  const measurementResults = {};
  for (const [name, contract] of Object.entries(slo.metrics)) {
    const measurement = evidence.measurements[name];
    const passed = metricPass(measurement.actual, contract);
    measurementResults[name] = {
      actual: measurement.actual,
      limit: contract.limit,
      comparison: contract.comparison,
      unit: contract.unit,
      sample_count: measurement.sample_count,
      source_artifact_digest: measurement.source_artifact_digest,
      passed,
    };
    if (!passed) failureReasons.push(`metric failed: ${name}`);
  }

  const observationResults = {};
  for (const [name, contract] of Object.entries(slo.observations)) {
    const observation = evidence.observations[name];
    const events = new Set(observation.timeline_event_ids);
    const timelineComplete = contract.required_timeline_events.every((event) =>
      events.has(event),
    );
    const passed = observation.observed && timelineComplete;
    observationResults[name] = {
      observed: observation.observed,
      timeline_event_ids: observation.timeline_event_ids,
      source_artifact_digest: observation.source_artifact_digest,
      passed,
    };
    if (!passed) failureReasons.push(`observation failed: ${name}`);
  }

  const allowedClasses = new Set(
    slo.topology.final_pass_failure_domain_classes,
  );
  if (evidence.environment.authority !== "production_readiness") {
    failureReasons.push("final pass requires production_readiness authority");
  }
  if (!allowedClasses.has(topology.failure_domain_class)) {
    failureReasons.push(
      "final pass requires a non-single-host failure-domain class",
    );
  }
  const distinctFailureDomains = new Set(
    topology.instances.map((item) => item.fault_domain),
  );
  if (distinctFailureDomains.size < slo.topology.failure_domains_min) {
    failureReasons.push(
      "topology has too few independent failure-domain aliases",
    );
  }

  return {
    schema_version: "ocservia.g6-verdict.v1",
    candidate_sha: evidence.candidate_sha,
    release_manifest_digest: computedManifestDigest,
    slo_contract_digest: computedSloDigest,
    topology_digest: computedTopologyDigest,
    authority: expectedAuthority,
    failure_domain_class: topology.failure_domain_class,
    measurement_results: measurementResults,
    observation_results: observationResults,
    failure_reasons: failureReasons,
    passed: failureReasons.length === 0,
  };
}
