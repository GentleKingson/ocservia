import { createHash } from "node:crypto";
import { lstatSync, readFileSync } from "node:fs";
import { isAbsolute, join, resolve, sep } from "node:path";
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
const componentPattern = /^[a-z][a-z0-9-]{0,63}$/;
const artifactNamePattern = /^[a-z0-9][a-z0-9._-]{0,127}$/;
const rfc3339Pattern =
  /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/;
const artifactKinds = new Set(["resource_samples", "timeline", "harness_log"]);
const derivations = new Set([
  "resource_samples.sample_span_seconds",
  "resource_samples.max_sample_gap_seconds",
  "resource_samples.valid_sample_count",
]);

function rfc3339(value, context) {
  if (typeof value !== "string" || !rfc3339Pattern.test(value)) {
    fail(`${context} must be a strict RFC 3339 timestamp`);
  }
  const parsed = Date.parse(value);
  if (!Number.isFinite(parsed)) fail(`${context} must be a real timestamp`);
  return parsed;
}

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

function artifactName(value, context) {
  if (typeof value !== "string") {
    fail(`${context} must be a string`);
  }
  if (value.includes("/") || value.includes("\\") || value.includes("..")) {
    fail(
      `${context} must be a restricted relative path without separators or parent references`,
    );
  }
  if (!artifactNamePattern.test(value)) {
    fail(`${context} has an invalid public name`);
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
    [
      "final_pass_failure_domain_classes",
      "failure_domains_min",
      "role_requirements",
      "distinct_failure_domain_role_pairs",
    ],
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
  object(slo.topology.role_requirements, "SLO role requirements");
  if (Object.keys(slo.topology.role_requirements).length === 0) {
    fail("SLO role requirements must not be empty");
  }
  for (const [role, requirement] of Object.entries(
    slo.topology.role_requirements,
  )) {
    if (!roles.has(role)) fail(`invalid SLO role requirement role: ${role}`);
    const allowed = [
      "instances_min",
      "component",
      ...(requirement?.failure_domains_min === undefined
        ? []
        : ["failure_domains_min"]),
    ];
    closed(requirement, allowed, `SLO role requirement ${role}`);
    if (
      !Number.isInteger(requirement.instances_min) ||
      requirement.instances_min < 1
    ) {
      fail(`SLO role requirement ${role} must set a positive instances_min`);
    }
    if (
      requirement.failure_domains_min !== undefined &&
      (!Number.isInteger(requirement.failure_domains_min) ||
        requirement.failure_domains_min < 2)
    ) {
      fail(
        `SLO role requirement ${role} must require at least two failure domains when set`,
      );
    }
    if (
      typeof requirement.component !== "string" ||
      !componentPattern.test(requirement.component)
    ) {
      fail(`SLO role requirement ${role} must bind a release component`);
    }
  }
  if (!Array.isArray(slo.topology.distinct_failure_domain_role_pairs)) {
    fail("SLO must define distinct failure-domain role pairs");
  }
  const seenPairs = new Set();
  for (const pair of slo.topology.distinct_failure_domain_role_pairs) {
    if (
      !Array.isArray(pair) ||
      pair.length !== 2 ||
      pair.some((role) => !roles.has(role)) ||
      pair[0] === pair[1]
    ) {
      fail("invalid distinct failure-domain role pair");
    }
    const key = [...pair].sort().join("|");
    if (seenPairs.has(key)) {
      fail("duplicate distinct failure-domain role pair");
    }
    seenPairs.add(key);
  }

  object(slo.metrics, "SLO metrics");
  if (Object.keys(slo.metrics).length === 0)
    fail("SLO metrics must not be empty");
  for (const [name, metric] of Object.entries(slo.metrics)) {
    if (!eventPattern.test(name)) fail(`invalid SLO metric name: ${name}`);
    const derived = metric?.derivation !== undefined;
    const declared = metric?.declared_by_harness !== undefined;
    if (derived === declared) {
      fail(
        `SLO metric ${name} must freeze exactly one trust boundary: derivation or declared_by_harness`,
      );
    }
    closed(
      metric,
      [
        "limit",
        "comparison",
        "unit",
        "scope",
        ...(derived ? ["derivation"] : ["declared_by_harness"]),
      ],
      `SLO metric ${name}`,
    );
    finiteNumber(metric.limit, `SLO metric ${name}.limit`);
    if (!comparisons.has(metric.comparison))
      fail(`invalid comparator for SLO metric ${name}`);
    if (!units.has(metric.unit)) fail(`invalid unit for SLO metric ${name}`);
    if (typeof metric.scope !== "string" || metric.scope.length === 0) {
      fail(`SLO metric ${name} must define scope`);
    }
    if (derived && !derivations.has(metric.derivation)) {
      fail(`SLO metric ${name} uses an unknown artifact derivation`);
    }
    if (!derived && metric.declared_by_harness !== true) {
      fail(`SLO metric ${name} must declare declared_by_harness: true`);
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

function verifyArtifacts(evidence, artifactRoot) {
  const rootDirectory = resolve(artifactRoot);
  const verified = new Map();
  for (const artifact of evidence.artifacts) {
    if (verified.has(artifact.name)) {
      fail(`duplicate evidence artifact name: ${artifact.name}`);
    }
    if (isAbsolute(artifact.name)) {
      fail(`evidence artifact name must not be absolute: ${artifact.name}`);
    }
    const artifactPath = resolve(rootDirectory, artifact.name);
    if (
      artifactPath !== join(rootDirectory, artifact.name) ||
      !artifactPath.startsWith(rootDirectory + sep)
    ) {
      fail(
        `evidence artifact name must stay inside the artifact root: ${artifact.name}`,
      );
    }
    let stats;
    try {
      stats = lstatSync(artifactPath);
    } catch {
      fail(
        `artifact file is missing under the artifact root: ${artifact.name}`,
      );
    }
    if (stats.isSymbolicLink()) {
      fail(`artifact must not be a symbolic link: ${artifact.name}`);
    }
    if (!stats.isFile()) {
      fail(`artifact must be a regular file: ${artifact.name}`);
    }
    const bytes = readFileSync(artifactPath);
    const computed = `sha256:${createHash("sha256").update(bytes).digest("hex")}`;
    if (computed !== artifact.digest) {
      fail(`artifact content digest mismatch: ${artifact.name}`);
    }
    verified.set(artifact.name, {
      digest: artifact.digest,
      bytes,
      name: artifact.name,
    });
  }
  return verified;
}

function splitArtifactLines(text, name, kind) {
  if (text.includes("\r")) {
    fail(`${kind} artifact ${name} must use LF line endings`);
  }
  const lines = text.split("\n");
  if (lines.at(-1) !== "") {
    fail(`${kind} artifact ${name} must end with a newline`);
  }
  lines.pop();
  if (lines.some((line) => line.length === 0)) {
    fail(`${kind} artifact ${name} must not contain empty lines`);
  }
  return lines;
}

function parseResourceSamples(entry) {
  const lines = splitArtifactLines(
    entry.bytes.toString("utf8"),
    entry.name,
    "resource samples",
  );
  if (lines.length < 2) {
    fail(`resource samples artifact ${entry.name} needs a header and samples`);
  }
  const header = lines[0].split(",");
  if (
    new Set(header).size !== header.length ||
    header.some((column) => !/^[a-z][a-z0-9_]{0,63}$/.test(column)) ||
    !header.includes("timestamp")
  ) {
    fail(`resource samples artifact ${entry.name} has an invalid header`);
  }
  const timestampColumn = header.indexOf("timestamp");
  const timestamps = [];
  for (const line of lines.slice(1)) {
    const columns = line.split(",");
    if (columns.length !== header.length) {
      fail(`resource samples artifact ${entry.name} has a ragged row`);
    }
    timestamps.push(
      rfc3339(
        columns[timestampColumn],
        `resource samples artifact ${entry.name} timestamp`,
      ),
    );
  }
  if (timestamps.length < 2) {
    fail(`resource samples artifact ${entry.name} needs at least two samples`);
  }
  timestamps.sort((left, right) => left - right);
  let maxGap = 0;
  for (let index = 1; index < timestamps.length; index += 1) {
    maxGap = Math.max(maxGap, timestamps[index] - timestamps[index - 1]);
  }
  return {
    sampleSpanSeconds: (timestamps.at(-1) - timestamps[0]) / 1000,
    maxSampleGapSeconds: maxGap / 1000,
    validSampleCount: timestamps.length,
  };
}

function parseTimeline(entry) {
  const lines = splitArtifactLines(
    entry.bytes.toString("utf8"),
    entry.name,
    "timeline",
  );
  const events = new Map();
  let lastSequence;
  let lastTimestamp;
  for (const line of lines) {
    const record = parseJSON(line, `timeline artifact ${entry.name} entry`);
    exactKeys(record, ["event_id", "sequence", "timestamp"], "timeline entry");
    if (!eventPattern.test(record.event_id)) {
      fail(`timeline artifact ${entry.name} has an invalid event_id`);
    }
    if (events.has(record.event_id)) {
      fail(
        `timeline artifact ${entry.name} repeats event_id ${record.event_id}`,
      );
    }
    if (!Number.isInteger(record.sequence)) {
      fail(`timeline artifact ${entry.name} needs integer sequences`);
    }
    if (lastSequence !== undefined && record.sequence <= lastSequence) {
      fail(`timeline artifact ${entry.name} sequences must strictly increase`);
    }
    const parsed = rfc3339(
      record.timestamp,
      `timeline artifact ${entry.name} timestamp`,
    );
    if (lastTimestamp !== undefined && parsed < lastTimestamp) {
      fail(`timeline artifact ${entry.name} timestamps must not decrease`);
    }
    lastSequence = record.sequence;
    lastTimestamp = parsed;
    events.set(record.event_id, { sequence: record.sequence });
  }
  if (events.size === 0) {
    fail(`timeline artifact ${entry.name} must not be empty`);
  }
  return events;
}

function validateEvidence(evidence, slo, artifactRoot) {
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
  for (const artifact of evidence.artifacts) {
    closed(
      artifact,
      ["name", "digest", "media_type", "kind"],
      "evidence artifact",
    );
    artifactName(artifact.name, `evidence artifact ${artifact.name} name`);
    digest(artifact.digest, `evidence artifact ${artifact.name} digest`);
    if (
      typeof artifact.media_type !== "string" ||
      artifact.media_type.length === 0
    ) {
      fail(`evidence artifact ${artifact.name} has an invalid media type`);
    }
    if (!artifactKinds.has(artifact.kind)) {
      fail(`evidence artifact ${artifact.name} has an invalid kind`);
    }
  }
  const verifiedArtifacts = verifyArtifacts(evidence, artifactRoot);
  const verifiedDigests = new Set(
    [...verifiedArtifacts.values()].map((entry) => entry.digest),
  );
  for (const [name, measurement] of Object.entries(evidence.measurements)) {
    if (!verifiedDigests.has(measurement.source_artifact_digest)) {
      fail(`evidence measurement ${name} references an unverified artifact`);
    }
  }
  for (const [name, observation] of Object.entries(evidence.observations)) {
    if (!verifiedDigests.has(observation.source_artifact_digest)) {
      fail(`evidence observation ${name} references an unverified artifact`);
    }
  }
  return verifiedArtifacts;
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
      "component",
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
    if (
      typeof instance.component !== "string" ||
      !componentPattern.test(instance.component)
    ) {
      fail(
        `topology instance ${instance.instance_id} has an invalid component`,
      );
    }
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
  const components = new Map();
  for (const component of manifest.components) {
    object(component, "release manifest component");
    if (
      typeof component.name !== "string" ||
      !componentPattern.test(component.name)
    ) {
      fail("release manifest component name is invalid");
    }
    if (components.has(component.name)) {
      fail(`duplicate release manifest component name: ${component.name}`);
    }
    digest(component.digest, `release manifest component ${component.name}`);
    components.set(component.name, component.digest);
  }
  return components;
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
  artifactRoot,
  expectedAuthority,
  expectedEnvironmentId,
  expectedFailureDomainClass,
}) {
  if (typeof artifactRoot !== "string" || artifactRoot.length === 0) {
    fail("an artifact root directory is required for content verification");
  }
  const slo = parseSlo(sloText);
  const evidence = parseJSON(evidenceText, "evidence");
  const topology = parseJSON(topologyText, "topology");
  const manifest = parseJSON(manifestText, "release manifest");
  const verifiedArtifacts = validateEvidence(evidence, slo, artifactRoot);
  validateTopology(topology);
  const manifestComponents = validateManifest(manifest);

  const standardArtifacts = new Map();
  for (const artifact of evidence.artifacts) {
    if (artifact.kind === "harness_log") continue;
    if (standardArtifacts.has(artifact.kind)) {
      fail(`evidence must declare exactly one ${artifact.kind} artifact`);
    }
    standardArtifacts.set(artifact.kind, artifact);
  }
  for (const kind of ["resource_samples", "timeline"]) {
    if (!standardArtifacts.has(kind)) {
      fail(`evidence must declare exactly one ${kind} artifact`);
    }
  }
  const resourceSamples = verifiedArtifacts.get(
    standardArtifacts.get("resource_samples").name,
  );
  const timelineArtifact = verifiedArtifacts.get(
    standardArtifacts.get("timeline").name,
  );
  const samples = parseResourceSamples(resourceSamples);
  const timelineEvents = parseTimeline(timelineArtifact);
  const derivedValues = {
    "resource_samples.sample_span_seconds": samples.sampleSpanSeconds,
    "resource_samples.max_sample_gap_seconds": samples.maxSampleGapSeconds,
    "resource_samples.valid_sample_count": samples.validSampleCount,
  };

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
    if (
      manifestComponents.get(instance.component) !== instance.component_digest
    ) {
      fail(
        `topology instance ${instance.instance_id} does not match release manifest component ${instance.component}`,
      );
    }
  }
  const topologyAgentInstances = topology.instances.filter(
    (instance) => instance.role === "agent",
  ).length;
  if (
    evidence.measurements.authorized_real_agents?.actual >
    topologyAgentInstances
  ) {
    fail(
      "evidence claims more authorized real agents than topology agent instances",
    );
  }

  const failureReasons = [];
  const measurementResults = {};
  for (const [name, contract] of Object.entries(slo.metrics)) {
    const measurement = evidence.measurements[name];
    if (contract.derivation !== undefined) {
      if (measurement.source_artifact_digest !== resourceSamples.digest) {
        fail(
          `evidence measurement ${name} must reference the resource_samples artifact`,
        );
      }
      const computed = derivedValues[contract.derivation];
      if (measurement.actual !== computed) {
        fail(
          `evidence measurement ${name} does not match the artifact-derived value`,
        );
      }
    }
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
    if (observation.source_artifact_digest !== timelineArtifact.digest) {
      fail(`evidence observation ${name} must reference the timeline artifact`);
    }
    for (const event of observation.timeline_event_ids) {
      if (!timelineEvents.has(event)) {
        fail(
          `evidence observation ${name} declares a timeline event absent from the artifact: ${event}`,
        );
      }
    }
    const requiredSequences = contract.required_timeline_events.map(
      (event) => timelineEvents.get(event)?.sequence,
    );
    const timelineComplete =
      requiredSequences.every((sequence) => sequence !== undefined) &&
      requiredSequences.every(
        (sequence, index) =>
          index === 0 || sequence > requiredSequences[index - 1],
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

  for (const [role, requirement] of Object.entries(
    slo.topology.role_requirements,
  )) {
    const roleInstances = topology.instances.filter(
      (instance) => instance.role === role,
    );
    if (roleInstances.length < requirement.instances_min) {
      failureReasons.push(`topology role ${role} has too few instances`);
    }
    if (requirement.failure_domains_min !== undefined) {
      const roleDomains = new Set(
        roleInstances.map((instance) => instance.fault_domain),
      );
      if (roleDomains.size < requirement.failure_domains_min) {
        failureReasons.push(
          `topology role ${role} spans too few failure domains`,
        );
      }
    }
    if (
      roleInstances.some(
        (instance) => instance.component !== requirement.component,
      )
    ) {
      failureReasons.push(
        `topology role ${role} does not use component ${requirement.component}`,
      );
    }
  }

  for (const [first, second] of slo.topology
    .distinct_failure_domain_role_pairs) {
    const firstDomains = new Set(
      topology.instances
        .filter((instance) => instance.role === first)
        .map((instance) => instance.fault_domain),
    );
    const shared = topology.instances
      .filter(
        (instance) =>
          instance.role === second && firstDomains.has(instance.fault_domain),
      )
      .map((instance) => instance.fault_domain);
    if (shared.length > 0) {
      failureReasons.push(
        `topology roles ${first} and ${second} share fault domain ${shared[0]}`,
      );
    }
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
