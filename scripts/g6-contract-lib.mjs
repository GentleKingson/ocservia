import { createHash } from "node:crypto";
import { lstatSync, readFileSync } from "node:fs";
import { isAbsolute, join, resolve, sep } from "node:path";
import { createRequire } from "node:module";

const require = createRequire(new URL("./g6-runtime/package.json", import.meta.url));
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
const identifierPattern = /^[a-z0-9][a-z0-9._-]{0,127}$/;
const endpointIdentityPattern = /^[0-9a-f]{64}$/;
const rfc3339Pattern =
  /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/;

// Structured artifact kinds the verifier parses and recomputes from. Every
// structured kind must appear exactly once in a final evidence bundle, while
// opaque harness_log files may appear any number of times (including zero).
export const structuredArtifactKinds = [
  "resource_samples",
  "timeline",
  "epoch_events",
  "authority_cut",
  "command_trace",
  "outbox_snapshot",
  "http_samples",
  "telemetry_snapshot",
  "audit_correlation",
  "postgres_recovery",
  "pitr_report",
  "agent_sessions",
  "relay_transitions",
];
const artifactKinds = new Set([...structuredArtifactKinds, "harness_log"]);

function rfc3339(value, context) {
  if (typeof value !== "string" || !rfc3339Pattern.test(value)) {
    fail(`${context} must be a strict RFC 3339 timestamp`);
  }
  const parsed = Date.parse(value);
  if (!Number.isFinite(parsed)) fail(`${context} must be a real timestamp`);
  return parsed;
}

function rfc3339Nanoseconds(value, context) {
  const match =
    /^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(?:\.(\d{1,9}))?(Z|[+-]\d{2}:\d{2})$/.exec(
      value,
    );
  if (!match) {
    fail(`${context} must be an RFC 3339 timestamp with at most nanosecond precision`);
  }
  const wholeSecondMs = Date.parse(`${match[1]}${match[3]}`);
  if (!Number.isFinite(wholeSecondMs)) {
    fail(`${context} must be a real timestamp`);
  }
  return (
    BigInt(wholeSecondMs) * 1000000n +
    BigInt((match[2] ?? "").padEnd(9, "0"))
  );
}

function compareRfc3339(left, right, leftContext, rightContext) {
  const leftNs = rfc3339Nanoseconds(left, leftContext);
  const rightNs = rfc3339Nanoseconds(right, rightContext);
  return leftNs < rightNs ? -1 : leftNs > rightNs ? 1 : 0;
}

function utcStampMicros(value, context) {
  const match =
    /^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(?:\.(\d{1,6}))?Z$/.exec(
      value,
    );
  if (!match) {
    fail(`${context} must be a microsecond RFC 3339 UTC timestamp`);
  }
  rfc3339(value, context);
  return (
    BigInt(Date.parse(`${match[1]}Z`)) * 1000n +
    BigInt((match[2] ?? "").padEnd(6, "0"))
  );
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

function positiveNumber(value, context) {
  if (typeof value !== "number" || !Number.isFinite(value) || value <= 0) {
    fail(`${context} must be a positive finite number`);
  }
}

function nonNegativeInteger(value, context) {
  if (!Number.isInteger(value) || value < 0) {
    fail(`${context} must be a non-negative integer`);
  }
}

function positiveDecimalString(value, context) {
  if (typeof value !== "string" || !/^[1-9][0-9]*$/.test(value)) {
    fail(`${context} must be a canonical positive decimal string`);
  }
  return value;
}

function boolean(value, context) {
  if (typeof value !== "boolean") fail(`${context} must be a boolean`);
}

function identifier(value, context) {
  if (typeof value !== "string" || !identifierPattern.test(value)) {
    fail(`${context} has an invalid identifier`);
  }
}

// Live probes render UUIDs with dashes while PostgreSQL snapshots may render
// the same binary identity as 32 hexadecimal digits.
function normalizedUUIDIdentity(value, context) {
  if (/^[0-9a-f]{32}$/.test(value ?? "")) return value;
  if (
    /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/.test(
      value ?? "",
    )
  ) {
    return value.replaceAll("-", "");
  }
  return fail(`${context} must be a UUID identity`);
}

function endpointIdentity(value, context) {
  if (typeof value !== "string" || !endpointIdentityPattern.test(value)) {
    fail(`${context} must be a 64-character lowercase hexadecimal endpoint id`);
  }
  return value;
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

// Every structured artifact is bound to the exact run context: each record or
// row repeats the environment ID and candidate SHA, so an artifact swapped in
// from a different run, environment, or build is rejected even when the
// evidence digest is updated to match the swapped bytes.
function requireBinding(environmentId, candidateSha, label, binding) {
  if (environmentId !== binding.environmentId) {
    fail(
      `${label} binds environment ${environmentId} but the evidence declares ${binding.environmentId}`,
    );
  }
  if (candidateSha !== binding.candidateSha) {
    fail(
      `${label} binds candidate ${candidateSha} but the evidence declares ${binding.candidateSha}`,
    );
  }
}

function requireWindow(timestampValue, label, binding) {
  const parsedTimestampNs =
    typeof timestampValue === "bigint"
      ? timestampValue
      : rfc3339Nanoseconds(timestampValue, `${label} timestamp`);
  if (parsedTimestampNs < binding.startedAtNs) {
    fail(`${label} timestamp precedes the evidence window`);
  }
  if (parsedTimestampNs > binding.finishedAtNs) {
    fail(`${label} timestamp escapes the evidence window`);
  }
}

function parseArtifactJSON(entry, kindLabel) {
  return parseJSON(entry.bytes.toString("utf8"), `${kindLabel} artifact ${entry.name}`);
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
    if (derived && !derivationRegistry.has(metric.derivation)) {
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
      fail(
        `artifact content digest mismatch: ${artifact.name} (expected ${artifact.digest}, actual ${computed})`,
      );
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

function ordinal(label) {
  return `${label} record`;
}

// Shared JSONL streaming: strict binding, strictly increasing sequences,
// non-decreasing timestamps, and evidence-window containment for every record.
function streamEventLines(entry, kindLabel, recordFields, binding, onRecord) {
  const lines = splitArtifactLines(
    entry.bytes.toString("utf8"),
    entry.name,
    kindLabel,
  );
  if (lines.length === 0) {
    fail(`${kindLabel} artifact ${entry.name} must not be empty`);
  }
  let lastSequence;
  let lastTimestampNs;
  for (const line of lines) {
    const record = parseJSON(
      line,
      `${kindLabel} artifact ${entry.name} entry`,
    );
    const fields = recordFields(record);
    if (!fields) {
      fail(`${kindLabel} artifact ${entry.name} has an invalid record shape`);
    }
    closed(
      record,
      [
        "sequence",
        "timestamp",
        "environment_id",
        "candidate_sha",
        ...fields,
      ],
      `${kindLabel} artifact ${entry.name} entry`,
    );
    if (!Number.isInteger(record.sequence)) {
      fail(`${kindLabel} artifact ${entry.name} needs integer sequences`);
    }
    if (lastSequence !== undefined && record.sequence <= lastSequence) {
      fail(`${kindLabel} artifact ${entry.name} sequences must strictly increase`);
    }
    const label = ordinal(`${kindLabel} artifact ${entry.name}`);
    const parsed = rfc3339(record.timestamp, `${label} timestamp`);
    const parsedNs = rfc3339Nanoseconds(
      record.timestamp,
      `${label} timestamp`,
    );
    if (lastTimestampNs !== undefined && parsedNs < lastTimestampNs) {
      fail(`${kindLabel} artifact ${entry.name} timestamps must not decrease`);
    }
    requireWindow(record.timestamp, `${label}`, binding);
    requireBinding(
      record.environment_id,
      record.candidate_sha,
      `${kindLabel} artifact ${entry.name} entry`,
      binding,
    );
    lastSequence = record.sequence;
    lastTimestampNs = parsedNs;
    onRecord(record, parsed, parsedNs);
  }
}

const resourceSampleHeader = [
  "timestamp",
  "component",
  "instance",
  "rss_bytes",
  "fd_count",
  "tasks",
  "queue_depth",
  "db_connections",
  "environment_id",
  "candidate_sha",
];
const resourceComponents = new Set([
  "controller",
  "transportd",
  "agent",
  "postgres",
]);
const resourceSampleBatchKeys = new Set([
  "controller:api-fd-b",
  "controller:worker-fd-b",
  "controller:scheduler-fd-b",
  "transportd:transportd-fd-b",
  "agent:agent-fd-b-01",
  "postgres:postgres-fd-b",
]);

function csvNonNegativeInteger(text, context) {
  if (!/^\d+$/.test(text)) {
    fail(`${context} must be a non-negative integer`);
  }
  return Number(text);
}

function csvSafeNonNegativeInteger(text, context) {
  const value = csvNonNegativeInteger(text, context);
  if (!Number.isSafeInteger(value)) {
    fail(`${context} must be a safe integer`);
  }
  return value;
}

function csvFiniteNumber(text, context) {
  const value = Number(text);
  if (!/^\d+(?:\.\d+)?$/.test(text) || !Number.isFinite(value)) {
    fail(`${context} must be a non-negative decimal number`);
  }
  return value;
}

function csvSecondsNanoseconds(text, context) {
  const match = /^(\d+)(?:\.(\d{1,9}))?$/.exec(text);
  const value = csvFiniteNumber(text, context);
  if (!match) {
    fail(`${context} must have at most nanosecond precision`);
  }
  return {
    value,
    nanoseconds:
      BigInt(match[1]) * 1000000000n +
      BigInt((match[2] ?? "").padEnd(9, "0")),
  };
}

function parseCsvRow(line, context) {
  const values = [];
  let value = "";
  let quoted = false;
  let closedQuote = false;
  for (let index = 0; index < line.length; index += 1) {
    const character = line[index];
    if (quoted) {
      if (character === '"') {
        if (line[index + 1] === '"') {
          value += '"';
          index += 1;
        } else {
          quoted = false;
          closedQuote = true;
        }
      } else {
        value += character;
      }
    } else if (closedQuote) {
      if (character !== ",") {
        fail(`${context} has characters after a closing CSV quote`);
      }
      values.push(value);
      value = "";
      closedQuote = false;
    } else if (character === ",") {
      values.push(value);
      value = "";
    } else if (character === '"' && value.length === 0) {
      quoted = true;
    } else if (character === '"') {
      fail(`${context} has an invalid CSV quote`);
    } else {
      value += character;
    }
  }
  if (quoted) fail(`${context} has an unterminated CSV quote`);
  values.push(value);
  return values;
}

function parseResourceSamples(entry, binding) {
  const lines = splitArtifactLines(
    entry.bytes.toString("utf8"),
    entry.name,
    "resource samples",
  );
  if (lines.length < 2) {
    fail(`resource samples artifact ${entry.name} needs a header and samples`);
  }
  const header = lines[0].split(",");
  if (JSON.stringify(header) !== JSON.stringify(resourceSampleHeader)) {
    fail(`resource samples artifact ${entry.name} has an invalid header`);
  }
  const rows = [];
  const componentRows = new Map();
  const batches = new Map();
  let lastTimestampNs;
  for (const component of resourceComponents) componentRows.set(component, []);
  for (const [index, line] of lines.slice(1).entries()) {
    const columns = line.split(",");
    if (columns.length !== header.length) {
      fail(`resource samples artifact ${entry.name} has a ragged row`);
    }
    const rowNumber = index + 2;
    const label = `resource samples artifact ${entry.name} row ${rowNumber}`;
    const parsedTimestamp = rfc3339(columns[0], `${label} timestamp`);
    const parsedTimestampNs = rfc3339Nanoseconds(
      columns[0],
      `${label} timestamp`,
    );
    if (lastTimestampNs !== undefined && parsedTimestampNs < lastTimestampNs) {
      fail(`resource samples artifact ${entry.name} timestamps must not decrease`);
    }
    lastTimestampNs = parsedTimestampNs;
    requireWindow(columns[0], label, binding);
    requireBinding(columns[8], columns[9], label, binding);
    const component = columns[1];
    if (!resourceComponents.has(component)) {
      fail(`${label} has an unknown component: ${component}`);
    }
    if (!identifierPattern.test(columns[2])) {
      fail(`${label} has an invalid instance identifier`);
    }
    const batchKey = `${component}:${columns[2]}`;
    if (!resourceSampleBatchKeys.has(batchKey)) {
      fail(`${label} is outside the exact formal sampler component/instance set`);
    }
    const timestampKey = parsedTimestampNs.toString();
    if (!batches.has(timestampKey)) {
      batches.set(timestampKey, {
        timestampMs: parsedTimestamp,
        timestampNs: parsedTimestampNs,
        keys: new Set(),
      });
    }
    const batch = batches.get(timestampKey);
    if (batch.keys.has(batchKey)) {
      fail(`${label} duplicates ${batchKey} in one sampler tick`);
    }
    batch.keys.add(batchKey);
    const dbConnectionsText = columns[7];
    const dbConnections =
      component === "postgres"
        ? csvNonNegativeInteger(dbConnectionsText, `${label} db_connections`)
        : null;
    if (component !== "postgres" && dbConnectionsText !== "") {
      fail(`${label} must leave db_connections empty for ${component}`);
    }
    const row = {
      timestampMs: parsedTimestamp,
      timestampNs: parsedTimestampNs,
      component,
      instance: columns[2],
      rssBytes: csvNonNegativeInteger(columns[3], `${label} rss_bytes`),
      fdCount: csvNonNegativeInteger(columns[4], `${label} fd_count`),
      tasks: csvNonNegativeInteger(columns[5], `${label} tasks`),
      queueDepth: csvNonNegativeInteger(columns[6], `${label} queue_depth`),
      dbConnections,
    };
    rows.push(row);
    componentRows.get(component).push(row);
  }
  for (const batch of batches.values()) {
    if (
      batch.keys.size !== resourceSampleBatchKeys.size ||
      [...resourceSampleBatchKeys].some((key) => !batch.keys.has(key))
    ) {
      fail(
        `resource samples artifact ${entry.name} has an incomplete sampler tick`,
      );
    }
  }
  const sampleBatches = [...batches.values()];
  if (sampleBatches.length < 2) {
    fail(`resource samples artifact ${entry.name} needs at least two complete ticks`);
  }
  let maxGapNs = 0n;
  for (let index = 1; index < sampleBatches.length; index += 1) {
    const gap =
      sampleBatches[index].timestampNs - sampleBatches[index - 1].timestampNs;
    if (gap > maxGapNs) maxGapNs = gap;
  }
  return {
    rows,
    componentRows,
    batchCount: sampleBatches.length,
    firstTimestampNs: sampleBatches[0].timestampNs,
    lastTimestampNs: sampleBatches.at(-1).timestampNs,
    sampleSpanSeconds: Number(
      sampleBatches.at(-1).timestampNs - sampleBatches[0].timestampNs,
    ) / 1_000_000_000,
    maxSampleGapSeconds: Number(maxGapNs) / 1_000_000_000,
  };
}

function parseTimeline(entry, binding) {
  const events = new Map();
  streamEventLines(
    entry,
    "timeline",
    () => ["event_id"],
    binding,
    (record, parsed, parsedNs) => {
      if (!eventPattern.test(record.event_id)) {
        fail(`timeline artifact ${entry.name} has an invalid event_id`);
      }
      if (events.has(record.event_id)) {
        fail(
          `timeline artifact ${entry.name} repeats event_id ${record.event_id}`,
        );
      }
      events.set(record.event_id, {
        sequence: record.sequence,
        timestamp: record.timestamp,
        timestampMs: parsed,
        timestampNs: parsedNs,
      });
    },
  );
  if (events.size === 0) {
    fail(`timeline artifact ${entry.name} must not be empty`);
  }
  return events;
}

const epochSubjects = new Set(["connection_owner", "scheduler"]);

function parseEpochEvents(entry, binding) {
  const state = {
    ownerMaxEpoch: new Map(),
    ownerLatest: new Map(),
    ownerRegistrationsByTerm: new Map(),
    ownerActive: new Map(),
    ownerInactive: new Set(),
    leaderMaxEpoch: 0,
    leaderLatest: null,
    leaderActive: new Set(),
    leaderExpired: new Set(),
    leaderTerms: new Map(),
    leaderLastMaintenanceId: 0n,
    staleOwnerAccepts: 0,
    staleSchedulerCommits: 0,
    maxConcurrentOwners: 0,
    maxConcurrentLeaders: 0,
    ownerAcceptCount: 0,
    ownerRegisteredCount: 0,
    leaderCommitCount: 0,
    leaderAcquiredCount: 0,
    ownerExpiries: [],
    leaderExpiries: [],
    ownerRegistrations: [],
    leaderCommits: [],
  };
  streamEventLines(
    entry,
    "epoch event log",
    (record) => {
      if (!epochSubjects.has(record.subject)) return null;
      switch (record.event_type) {
        case "owner_registered":
          return [
            "subject",
            "event_type",
            "node",
            "instance",
            "incarnation",
            "connection_id",
            "epoch",
            "lease_until",
            ...(record.session_connected_at === undefined
              ? []
              : ["session_connected_at"]),
          ];
        case "owner_lease_expired":
        case "owner_retired":
          return ["subject", "event_type", "node", "epoch"];
        case "owner_accept":
          return ["subject", "event_type", "node", "instance", "epoch", "accepted"];
        case "leader_acquired":
          return [
            "subject",
            "event_type",
            "instance",
            "incarnation",
            "epoch",
            "lease_until",
          ];
        case "leader_lease_expired":
          return ["subject", "event_type", "epoch"];
        case "leader_commit":
          return record.accepted === true
            ? [
                "subject",
                "event_type",
                "instance",
                "incarnation",
                "epoch",
                "maintenance_id",
                "marker_completed_at",
                "accepted",
              ]
            : [
                "subject",
                "event_type",
                "instance",
                "incarnation",
                "epoch",
                "accepted",
              ];
        default:
          return null;
      }
    },
    binding,
    (record, parsed, parsedNs) => {
      if (record.subject === "connection_owner") {
        const node = normalizedUUIDIdentity(record.node, "epoch event node");
        nonNegativeInteger(record.epoch, "epoch event epoch");
        if (record.epoch < 1) fail("epoch event epoch must be positive");
        if (!state.ownerActive.has(node)) state.ownerActive.set(node, new Set());
        const active = state.ownerActive.get(node);
        const maxEpoch = state.ownerMaxEpoch.get(node) ?? 0;
        if (record.event_type === "owner_registered") {
          identifier(record.instance, "epoch event instance");
          positiveDecimalString(
            record.incarnation,
            "epoch event owner incarnation",
          );
          const connectionId = normalizedUUIDIdentity(
            record.connection_id,
            "epoch event owner connection_id",
          );
          const leaseUntilMs = rfc3339(
            record.lease_until,
            "epoch event owner lease_until",
          );
          if (leaseUntilMs <= parsed) {
            fail("epoch event owner lease must extend past registration");
          }
          let sessionConnectedMs;
          let sessionConnectedNs;
          if (record.session_connected_at !== undefined) {
            sessionConnectedMs = rfc3339(
              record.session_connected_at,
              "epoch event owner session_connected_at",
            );
            sessionConnectedNs = rfc3339Nanoseconds(
              record.session_connected_at,
              "epoch event owner session_connected_at",
            );
            if (sessionConnectedNs < parsedNs) {
              fail("epoch event owner session completion predates registration");
            }
          }
          state.ownerRegisteredCount += 1;
          if (record.epoch <= maxEpoch) {
            fail(
              `epoch event log ${entry.name} owner epochs must strictly increase per node`,
            );
          }
          state.ownerMaxEpoch.set(node, record.epoch);
          state.ownerLatest.set(node, {
            epoch: record.epoch,
            instance: record.instance,
            incarnation: record.incarnation,
            connectionId,
            leaseUntil: record.lease_until,
            leaseUntilMs,
          });
          const registrationKey = [
            node,
            record.instance,
            record.incarnation,
            connectionId,
            record.epoch,
          ].join(":");
          if (state.ownerRegistrationsByTerm.has(registrationKey)) {
            fail(
              `epoch event log ${entry.name} repeats a connection-owner registration term`,
            );
          }
          state.ownerRegistrationsByTerm.set(registrationKey, {
            timestamp: record.timestamp,
            timestampMs: parsed,
            timestampNs: parsedNs,
            sessionConnectedAt: record.session_connected_at,
            sessionConnectedMs,
            sessionConnectedNs,
          });
          active.add(record.epoch);
          state.ownerRegistrations.push({
            node,
            epoch: record.epoch,
            timestampMs: parsed,
            timestampNs: parsedNs,
            sessionConnectedMs,
            sessionConnectedNs,
          });
        } else if (record.event_type === "owner_lease_expired") {
          if (!active.has(record.epoch)) {
            fail(
              `epoch event log ${entry.name} owner lease expiry must reference the active owner epoch`,
            );
          }
          active.delete(record.epoch);
          state.ownerInactive.add(`${node}:${record.epoch}`);
          state.ownerExpiries.push({
            node,
            epoch: record.epoch,
            timestampMs: parsed,
            timestampNs: parsedNs,
          });
        } else if (record.event_type === "owner_retired") {
          if (!active.has(record.epoch)) {
            fail(
              `epoch event log ${entry.name} owner retirement must reference the active owner epoch`,
            );
          }
          active.delete(record.epoch);
          state.ownerInactive.add(`${node}:${record.epoch}`);
        } else if (record.event_type === "owner_accept") {
          identifier(record.instance, "epoch event instance");
          boolean(record.accepted, "epoch event accepted");
          state.ownerAcceptCount += 1;
          if (record.epoch > maxEpoch) {
            fail(
              `epoch event log ${entry.name} owner accept references an unregistered epoch`,
            );
          }
          if (
            record.accepted &&
            (record.epoch < maxEpoch || state.ownerInactive.has(`${node}:${record.epoch}`))
          ) {
            state.staleOwnerAccepts += 1;
          }
        }
        state.maxConcurrentOwners = Math.max(
          state.maxConcurrentOwners,
          active.size,
        );
      } else {
        nonNegativeInteger(record.epoch, "epoch event epoch");
        if (record.epoch < 1) fail("epoch event epoch must be positive");
        if (record.event_type === "leader_acquired") {
          identifier(record.instance, "epoch event instance");
          positiveDecimalString(
            record.incarnation,
            "epoch event scheduler incarnation",
          );
          const leaseUntilMs = rfc3339(
            record.lease_until,
            "epoch event scheduler lease_until",
          );
          if (leaseUntilMs <= parsed) {
            fail("epoch event scheduler lease must extend past acquisition");
          }
          state.leaderAcquiredCount += 1;
          if (record.epoch <= state.leaderMaxEpoch) {
            fail(
              `epoch event log ${entry.name} scheduler epochs must strictly increase`,
            );
          }
          state.leaderMaxEpoch = record.epoch;
          state.leaderLatest = {
            epoch: record.epoch,
            instance: record.instance,
            incarnation: record.incarnation,
            leaseUntil: record.lease_until,
            leaseUntilMs,
          };
          state.leaderTerms.set(record.epoch, {
            instance: record.instance,
            incarnation: record.incarnation,
            acquiredAtNs: parsedNs,
            active: true,
            acceptedMaintenanceCount: 0,
            maintenanceCompletions: [],
          });
          state.leaderActive.add(record.epoch);
        } else if (record.event_type === "leader_lease_expired") {
          if (!state.leaderActive.has(record.epoch)) {
            fail(
              `epoch event log ${entry.name} scheduler lease expiry must reference the active leader epoch`,
            );
          }
          state.leaderActive.delete(record.epoch);
          state.leaderExpired.add(record.epoch);
          state.leaderTerms.get(record.epoch).active = false;
          state.leaderExpiries.push({
            epoch: record.epoch,
            timestampMs: parsed,
            timestampNs: parsedNs,
          });
        } else if (record.event_type === "leader_commit") {
          identifier(record.instance, "epoch event instance");
          positiveDecimalString(
            record.incarnation,
            "epoch event scheduler incarnation",
          );
          boolean(record.accepted, "epoch event accepted");
          state.leaderCommitCount += 1;
          const term = state.leaderTerms.get(record.epoch);
          if (!term) {
            fail(
              `epoch event log ${entry.name} scheduler commit references an unacquired epoch`,
            );
          }
          if (
            term.instance !== record.instance ||
            term.incarnation !== record.incarnation
          ) {
            fail(
              `epoch event log ${entry.name} scheduler commit does not match its exact acquired term`,
            );
          }
          if (parsedNs < term.acquiredAtNs) {
            fail(
              `epoch event log ${entry.name} scheduler commit predates its acquired term`,
            );
          }
          if (record.accepted) {
            const maintenanceId = BigInt(
              positiveDecimalString(
                record.maintenance_id,
                "epoch event scheduler maintenance_id",
              ),
            );
            const markerCompletedAtMs = rfc3339(
              record.marker_completed_at,
              "epoch event scheduler marker_completed_at",
            );
            const markerCompletedAtNs = rfc3339Nanoseconds(
              record.marker_completed_at,
              "epoch event scheduler marker_completed_at",
            );
            if (markerCompletedAtNs < term.acquiredAtNs) {
              fail(
                `epoch event log ${entry.name} scheduler maintenance marker predates its acquired term`,
              );
            }
            if (markerCompletedAtNs > parsedNs) {
              fail(
                `epoch event log ${entry.name} scheduler maintenance marker completed after its committed observation`,
              );
            }
            if (maintenanceId <= state.leaderLastMaintenanceId) {
              fail(
                `epoch event log ${entry.name} scheduler maintenance ids must strictly increase`,
              );
            }
            state.leaderLastMaintenanceId = maintenanceId;
            term.acceptedMaintenanceCount += 1;
            term.maintenanceCompletions.push({
              maintenanceId,
              markerCompletedAtMs,
              markerCompletedAtNs,
              timestampMs: parsed,
              timestampNs: parsedNs,
            });
          } else if (term.active) {
            fail(
              `epoch event log ${entry.name} rejects a scheduler term that is still active`,
            );
          }
          if (record.accepted && !term.active) {
            state.staleSchedulerCommits += 1;
          }
          state.leaderCommits.push({
            epoch: record.epoch,
            instance: record.instance,
            incarnation: record.incarnation,
            timestampMs: parsed,
            timestampNs: parsedNs,
            accepted: record.accepted,
          });
        }
        state.maxConcurrentLeaders = Math.max(
          state.maxConcurrentLeaders,
          state.leaderActive.size,
        );
      }
    },
  );
  for (const [epoch, term] of state.leaderTerms) {
    if (term.acceptedMaintenanceCount === 0) {
      fail(
        `epoch event log ${entry.name} leadership epoch ${epoch} has no exact-term maintenance completion`,
      );
    }
  }
  return state;
}

function parseTransportBracketInventory(
  records,
  label,
  boundaryNs,
  cutNs,
) {
  if (!Array.isArray(records) || records.length === 0) {
    fail(`${label} must contain at least one transport observation`);
  }
  const observations = new Map();
  for (const [index, observation] of records.entries()) {
    const observationLabel = `${label} observation ${index + 1}`;
    closed(
      observation,
      [
        "node",
        "endpoint_id",
        "agent_instance_id",
        "connected_at",
        "session_expires_at",
        "owner_fence_id",
        "owner_instance",
        "owner_incarnation",
        "connection_id",
        "owner_epoch",
        "owner_lease_until",
        "authorization_revision",
        "negotiated_capabilities",
      ],
      observationLabel,
    );
    const node = normalizedUUIDIdentity(
      observation.node,
      `${observationLabel} node`,
    );
    const endpointId = endpointIdentity(
      observation.endpoint_id,
      `${observationLabel} endpoint_id`,
    );
    const agentInstanceId = normalizedUUIDIdentity(
      observation.agent_instance_id,
      `${observationLabel} agent_instance_id`,
    );
    const connectedAtMs = rfc3339(
      observation.connected_at,
      `${observationLabel} connected_at`,
    );
    const connectedAtNs = rfc3339Nanoseconds(
      observation.connected_at,
      `${observationLabel} connected_at`,
    );
    if (connectedAtNs > boundaryNs) {
      fail(`${observationLabel} connects after its inventory boundary`);
    }
    const sessionExpiresAtMs = rfc3339(
      observation.session_expires_at,
      `${observationLabel} session_expires_at`,
    );
    const sessionExpiresAtNs = rfc3339Nanoseconds(
      observation.session_expires_at,
      `${observationLabel} session_expires_at`,
    );
    if (sessionExpiresAtNs <= cutNs) {
      fail(`${observationLabel} session does not remain live across the cut`);
    }
    const ownerFenceId = normalizedUUIDIdentity(
      observation.owner_fence_id,
      `${observationLabel} owner_fence_id`,
    );
    identifier(observation.owner_instance, `${observationLabel} owner_instance`);
    positiveDecimalString(
      observation.owner_incarnation,
      `${observationLabel} owner_incarnation`,
    );
    const connectionId = normalizedUUIDIdentity(
      observation.connection_id,
      `${observationLabel} connection_id`,
    );
    nonNegativeInteger(
      observation.owner_epoch,
      `${observationLabel} owner_epoch`,
    );
    if (observation.owner_epoch < 1) {
      fail(`${observationLabel} owner_epoch must be positive`);
    }
    const ownerLeaseUntilMs = rfc3339(
      observation.owner_lease_until,
      `${observationLabel} owner_lease_until`,
    );
    const ownerLeaseUntilNs = rfc3339Nanoseconds(
      observation.owner_lease_until,
      `${observationLabel} owner_lease_until`,
    );
    if (ownerLeaseUntilNs <= cutNs) {
      fail(`${observationLabel} owner lease does not remain live across the cut`);
    }
    nonNegativeInteger(
      observation.authorization_revision,
      `${observationLabel} authorization_revision`,
    );
    if (!Number.isSafeInteger(observation.authorization_revision)) {
      fail(`${observationLabel} authorization_revision must be a safe integer`);
    }
    if (
      !Array.isArray(observation.negotiated_capabilities) ||
      observation.negotiated_capabilities.length === 0
    ) {
      fail(`${observationLabel} negotiated_capabilities must not be empty`);
    }
    const capabilities = observation.negotiated_capabilities.map(
      (capability, capabilityIndex) => {
        identifier(
          capability,
          `${observationLabel} negotiated_capabilities ${capabilityIndex + 1}`,
        );
        return capability;
      },
    );
    if (new Set(capabilities).size !== capabilities.length) {
      fail(`${observationLabel} repeats a negotiated capability`);
    }
    if (observations.has(node)) {
      fail(`${label} repeats node ${node}`);
    }
    observations.set(node, {
      node,
      endpointId,
      agentInstanceId,
      connectedAt: observation.connected_at,
      connectedAtMs,
      connectedAtNs,
      sessionExpiresAt: observation.session_expires_at,
      sessionExpiresAtMs,
      sessionExpiresAtNs,
      ownerFenceId,
      ownerInstance: observation.owner_instance,
      ownerIncarnation: observation.owner_incarnation,
      connectionId,
      ownerEpoch: observation.owner_epoch,
      ownerLeaseUntil: observation.owner_lease_until,
      ownerLeaseUntilMs,
      ownerLeaseUntilNs,
      authorizationRevision: observation.authorization_revision,
      capabilities: [...capabilities].sort(),
    });
  }
  return observations;
}

function parseAuthorityCut(entry, binding) {
  const label = `authority cut artifact ${entry.name}`;
  const doc = parseArtifactJSON(entry, "authority cut");
  closed(
    doc,
    [
      "environment_id",
      "candidate_sha",
      "cut_at",
      "transport_bracket",
      "owners",
      "scheduler",
    ],
    label,
  );
  requireBinding(doc.environment_id, doc.candidate_sha, label, binding);
  const cutMs = rfc3339(doc.cut_at, `${label} cut_at`);
  const cutNs = rfc3339Nanoseconds(doc.cut_at, `${label} cut_at`);
  const cutMicros = utcStampMicros(doc.cut_at, `${label} cut_at`);
  requireWindow(doc.cut_at, label, binding);
  closed(
    doc.transport_bracket,
    ["before_complete_at", "after_start_at", "before", "after"],
    `${label} transport bracket`,
  );
  const beforeCompleteMs = rfc3339(
    doc.transport_bracket.before_complete_at,
    `${label} transport bracket before_complete_at`,
  );
  const beforeCompleteMicros = utcStampMicros(
    doc.transport_bracket.before_complete_at,
    `${label} transport bracket before_complete_at`,
  );
  const beforeCompleteNs = rfc3339Nanoseconds(
    doc.transport_bracket.before_complete_at,
    `${label} transport bracket before_complete_at`,
  );
  const afterStartMs = rfc3339(
    doc.transport_bracket.after_start_at,
    `${label} transport bracket after_start_at`,
  );
  const afterStartMicros = utcStampMicros(
    doc.transport_bracket.after_start_at,
    `${label} transport bracket after_start_at`,
  );
  const afterStartNs = rfc3339Nanoseconds(
    doc.transport_bracket.after_start_at,
    `${label} transport bracket after_start_at`,
  );
  requireWindow(
    doc.transport_bracket.before_complete_at,
    `${label} before inventory boundary`,
    binding,
  );
  requireWindow(
    doc.transport_bracket.after_start_at,
    `${label} after inventory boundary`,
    binding,
  );
  if (!(beforeCompleteMicros < cutMicros && cutMicros < afterStartMicros)) {
    fail(`${label} transport inventories must strictly bracket cut_at`);
  }
  const beforeObservations = parseTransportBracketInventory(
    doc.transport_bracket.before,
    `${label} before-cut transport inventory`,
    beforeCompleteNs,
    cutNs,
  );
  const afterObservations = parseTransportBracketInventory(
    doc.transport_bracket.after,
    `${label} after-cut transport inventory`,
    afterStartNs,
    cutNs,
  );
  if (!Array.isArray(doc.owners) || doc.owners.length === 0) {
    fail(`${label} must contain at least one owner`);
  }
  const owners = new Map();
  for (const [index, owner] of doc.owners.entries()) {
    const ownerLabel = `${label} owner ${index + 1}`;
    closed(
      owner,
      [
        "node",
        "instance",
        "incarnation",
        "connection_id",
        "epoch",
        "lease_until",
      ],
      ownerLabel,
    );
    const node = normalizedUUIDIdentity(owner.node, `${ownerLabel} node`);
    identifier(owner.instance, `${ownerLabel} instance`);
    positiveDecimalString(owner.incarnation, `${ownerLabel} incarnation`);
    const connectionId = normalizedUUIDIdentity(
      owner.connection_id,
      `${ownerLabel} connection_id`,
    );
    nonNegativeInteger(owner.epoch, `${ownerLabel} epoch`);
    if (owner.epoch < 1) fail(`${ownerLabel} epoch must be positive`);
    const leaseUntilMs = rfc3339(
      owner.lease_until,
      `${ownerLabel} lease_until`,
    );
    const leaseUntilNs = rfc3339Nanoseconds(
      owner.lease_until,
      `${ownerLabel} lease_until`,
    );
    if (leaseUntilNs <= cutNs) {
      fail(`${ownerLabel} lease must remain live after the cut`);
    }
    if (owners.has(node)) {
      fail(`${label} repeats owner node ${node}`);
    }
    owners.set(node, {
      instance: owner.instance,
      incarnation: owner.incarnation,
      connectionId,
      epoch: owner.epoch,
      leaseUntil: owner.lease_until,
      leaseUntilMs,
      leaseUntilNs,
    });
  }

  if (
    beforeObservations.size !== afterObservations.size ||
    beforeObservations.size !== owners.size
  ) {
    fail(`${label} transport and database owner populations must match`);
  }
  for (const [node, before] of beforeObservations) {
    const after = afterObservations.get(node);
    if (!after) {
      fail(`${label} after-cut transport inventory omits node ${node}`);
    }
    if (
      before.endpointId !== after.endpointId ||
      before.agentInstanceId !== after.agentInstanceId ||
      before.connectedAt !== after.connectedAt ||
      before.sessionExpiresAt !== after.sessionExpiresAt ||
      before.ownerFenceId !== after.ownerFenceId ||
      before.ownerInstance !== after.ownerInstance ||
      before.ownerIncarnation !== after.ownerIncarnation ||
      before.connectionId !== after.connectionId ||
      before.ownerEpoch !== after.ownerEpoch ||
      before.authorizationRevision !== after.authorizationRevision ||
      JSON.stringify(before.capabilities) !== JSON.stringify(after.capabilities)
    ) {
      fail(`${label} immutable transport tuple changes across cut_at for node ${node}`);
    }
    const owner = owners.get(node);
    if (!owner) {
      fail(`${label} database authority cut omits transport node ${node}`);
    }
    if (
      owner.instance !== after.ownerInstance ||
      owner.incarnation !== after.ownerIncarnation ||
      owner.connectionId !== after.connectionId ||
      owner.epoch !== after.ownerEpoch
    ) {
      fail(`${label} transport owner term does not match the database cut for node ${node}`);
    }
  }
  for (const node of owners.keys()) {
    if (!beforeObservations.has(node)) {
      fail(`${label} database authority owner ${node} is absent from the bracket`);
    }
  }

  object(doc.scheduler, `${label} scheduler`);
  closed(
    doc.scheduler,
    [
      "instance",
      "incarnation",
      "epoch",
      "lease_until",
      "maintenance_id",
      "maintenance_completed_at",
    ],
    `${label} scheduler`,
  );
  identifier(doc.scheduler.instance, `${label} scheduler instance`);
  positiveDecimalString(
    doc.scheduler.incarnation,
    `${label} scheduler incarnation`,
  );
  nonNegativeInteger(doc.scheduler.epoch, `${label} scheduler epoch`);
  if (doc.scheduler.epoch < 1) {
    fail(`${label} scheduler epoch must be positive`);
  }
  const schedulerLeaseUntilMs = rfc3339(
    doc.scheduler.lease_until,
    `${label} scheduler lease_until`,
  );
  const schedulerLeaseUntilNs = rfc3339Nanoseconds(
    doc.scheduler.lease_until,
    `${label} scheduler lease_until`,
  );
  if (schedulerLeaseUntilNs <= cutNs) {
    fail(`${label} scheduler lease must remain live after the cut`);
  }
  const schedulerMaintenanceId = positiveDecimalString(
    doc.scheduler.maintenance_id,
    `${label} scheduler maintenance_id`,
  );
  const schedulerMaintenanceCompletedAtNs = rfc3339Nanoseconds(
    doc.scheduler.maintenance_completed_at,
    `${label} scheduler maintenance_completed_at`,
  );
  if (schedulerMaintenanceCompletedAtNs > cutNs) {
    fail(`${label} scheduler maintenance completed after the cut`);
  }
  return {
    cutAt: doc.cut_at,
    cutMs,
    cutNs,
    beforeCompleteAt: doc.transport_bracket.before_complete_at,
    beforeCompleteMs,
    afterStartAt: doc.transport_bracket.after_start_at,
    afterStartMs,
    beforeObservations,
    afterObservations,
    owners,
    scheduler: {
      instance: doc.scheduler.instance,
      incarnation: doc.scheduler.incarnation,
      epoch: doc.scheduler.epoch,
      leaseUntil: doc.scheduler.lease_until,
      leaseUntilMs: schedulerLeaseUntilMs,
      leaseUntilNs: schedulerLeaseUntilNs,
      maintenanceId: schedulerMaintenanceId,
      maintenanceCompletedAt: doc.scheduler.maintenance_completed_at,
      maintenanceCompletedAtNs: schedulerMaintenanceCompletedAtNs,
    },
  };
}

const commandOutcomes = new Set(["success", "failed", "unknown"]);

function parseCommandTrace(entry, binding) {
  const state = {
    dispatchBoundSeconds: null,
    commands: new Map(),
    effects: new Map(),
    effectIdSeen: new Set(),
    resultCommandSeen: new Set(),
    dispatchedCommands: 0,
    resultCount: 0,
    unmatchedResultCount: 0,
    duplicateEffectCount: 0,
    inflight: 0,
    maxInflight: 0,
    inflightSnapshot: null,
    lastTimestampMicros: null,
  };
  streamEventLines(
    entry,
    "command trace",
    (record) => {
      switch (record.record_type) {
        case "profile":
          return ["record_type", "dispatch_bound_seconds"];
        case "enqueued":
          return ["record_type", "command_id", "idempotency_key"];
        case "dispatched":
          return ["record_type", "command_id"];
        case "effect":
          return [
            "record_type",
            "command_id",
            "idempotency_key",
            "effect_id",
          ];
        case "inflight_snapshot":
          return [
            "record_type",
            "expected_count",
            "result_count",
            "commands",
          ];
        case "result":
          return ["record_type", "command_id", "outcome"];
        default:
          return null;
      }
    },
    binding,
    (record) => {
      const label = `command trace artifact ${entry.name}`;
      const timestampMicros = utcStampMicros(
        record.timestamp,
        `${label} timestamp`,
      );
      if (
        state.lastTimestampMicros !== null &&
        timestampMicros < state.lastTimestampMicros
      ) {
        fail(`${label} timestamps must not decrease at microsecond precision`);
      }
      state.lastTimestampMicros = timestampMicros;
      if (record.record_type === "profile") {
        if (state.dispatchBoundSeconds !== null) {
          fail(`${label} must declare exactly one profile record`);
        }
        if (state.commands.size > 0 || state.dispatchedCount > 0) {
          fail(`${label} profile record must come first`);
        }
        positiveNumber(record.dispatch_bound_seconds, `${label} dispatch_bound_seconds`);
        state.dispatchBoundSeconds = record.dispatch_bound_seconds;
        return;
      }
      if (state.dispatchBoundSeconds === null) {
        fail(`${label} profile record must come first`);
      }
      if (record.record_type === "enqueued") {
        identifier(record.command_id, `${label} command_id`);
        identifier(record.idempotency_key, `${label} idempotency_key`);
        if (state.commands.has(record.command_id)) {
          fail(`${label} repeats command_id ${record.command_id}`);
        }
        state.commands.set(record.command_id, {
          idempotencyKey: record.idempotency_key,
          enqueuedAtMicros: timestampMicros,
          dispatchedAtMicros: null,
          firstResultAtMicros: null,
          proofOnly: false,
        });
      } else if (record.record_type === "dispatched") {
        identifier(record.command_id, `${label} command_id`);
        const command = state.commands.get(record.command_id);
        if (!command) {
          fail(
            `${label} dispatch references unknown command ${record.command_id}`,
          );
        }
        // Repeated dispatches model at-least-once retries; only each
        // command's first dispatch counts for inflight and sample counts.
        if (command.dispatchedAtMicros === null) {
          command.dispatchedAtMicros = timestampMicros;
          state.dispatchedCommands += 1;
          state.inflight += 1;
          state.maxInflight = Math.max(state.maxInflight, state.inflight);
        }
      } else if (record.record_type === "effect") {
        identifier(record.command_id, `${label} command_id`);
        identifier(record.idempotency_key, `${label} idempotency_key`);
        identifier(record.effect_id, `${label} effect_id`);
        if (!state.commands.has(record.command_id)) {
          fail(`${label} effect references unknown command ${record.command_id}`);
        }
        if (state.effectIdSeen.has(record.effect_id)) {
          fail(`${label} repeats effect_id ${record.effect_id}`);
        }
        state.effectIdSeen.add(record.effect_id);
        const effects = state.effects.get(record.idempotency_key) ?? [];
        if (effects.length > 0) state.duplicateEffectCount += 1;
        effects.push({
          commandId: record.command_id,
          effectId: record.effect_id,
          timestampMicros,
        });
        state.effects.set(record.idempotency_key, effects);
      } else if (record.record_type === "inflight_snapshot") {
        if (state.inflightSnapshot !== null) {
          fail(`${label} must contain exactly one inflight snapshot`);
        }
        if (
          !Number.isInteger(record.expected_count) ||
          record.expected_count <= 0 ||
          record.result_count !== 0 ||
          !Array.isArray(record.commands) ||
          record.commands.length !== record.expected_count
        ) {
          fail(`${label} inflight snapshot is not an exact result-free population`);
        }
        const commandIds = new Set();
        const nodeIds = new Set();
        const snapshotCommands = new Map();
        for (const [index, snapshotCommand] of record.commands.entries()) {
          const commandLabel = `${label} inflight snapshot command ${index + 1}`;
          closed(
            snapshotCommand,
            ["command_id", "node_id", "state"],
            commandLabel,
          );
          identifier(snapshotCommand.command_id, `${commandLabel} command_id`);
          const nodeId = normalizedUUIDIdentity(
            snapshotCommand.node_id,
            `${commandLabel} node_id`,
          );
          if (
            !["dispatched", "accepted", "running"].includes(
              snapshotCommand.state,
            )
          ) {
            fail(`${commandLabel} is not active`);
          }
          if (
            commandIds.has(snapshotCommand.command_id) ||
            nodeIds.has(nodeId)
          ) {
            fail(`${label} inflight snapshot repeats a command or managed node`);
          }
          const command = state.commands.get(snapshotCommand.command_id);
          if (
            !command ||
            command.dispatchedAtMicros === null ||
            command.dispatchedAtMicros > timestampMicros
          ) {
            fail(
              `${commandLabel} was not dispatched by the snapshot boundary`,
            );
          }
          commandIds.add(snapshotCommand.command_id);
          nodeIds.add(nodeId);
          snapshotCommands.set(snapshotCommand.command_id, {
            nodeId,
            state: snapshotCommand.state,
          });
        }
        state.inflightSnapshot = {
          timestampMicros,
          expectedCount: record.expected_count,
          commands: snapshotCommands,
          nodeIds,
        };
      } else if (record.record_type === "result") {
        identifier(record.command_id, `${label} command_id`);
        if (!commandOutcomes.has(record.outcome)) {
          fail(`${label} result has an invalid outcome`);
        }
        // A command carries exactly one terminal result; a second result
        // for the same command cannot be attributed to an attempt.
        if (state.resultCommandSeen.has(record.command_id)) {
          fail(
            `${label} repeats a terminal result for command ${record.command_id}`,
          );
        }
        state.resultCommandSeen.add(record.command_id);
        state.resultCount += 1;
        const command = state.commands.get(record.command_id);
        if (!command || command.dispatchedAtMicros === null) {
          state.unmatchedResultCount += 1;
          return;
        }
        command.firstResultAtMicros = timestampMicros;
        state.inflight -= 1;
      }
    },
  );
  if (state.dispatchBoundSeconds === null) {
    fail(`command trace artifact ${entry.name} must declare a profile record`);
  }
  if (state.inflightSnapshot === null) {
    fail(`command trace artifact ${entry.name} must contain one inflight snapshot`);
  }
  for (const commandId of state.inflightSnapshot.commands.keys()) {
    const command = state.commands.get(commandId);
    if (
      command?.firstResultAtMicros === null ||
      command.firstResultAtMicros <= state.inflightSnapshot.timestampMicros
    ) {
      fail(
        `command trace inflight snapshot command ${commandId} is not result-free at its boundary`,
      );
    }
  }
  return state;
}

const outboxStates = new Set([
  "terminal",
  "pending",
  "reconciliation_active",
  "unknown_reconciling",
  "unknown",
]);

function parseOutboxSnapshot(entry, binding) {
  const label = `outbox snapshot artifact ${entry.name}`;
  const doc = parseArtifactJSON(entry, "outbox snapshot");
  closed(
    doc,
    ["environment_id", "candidate_sha", "snapshot_taken_at", "rows"],
    label,
  );
  requireBinding(doc.environment_id, doc.candidate_sha, label, binding);
  const snapshotMs = rfc3339(doc.snapshot_taken_at, `${label} snapshot_taken_at`);
  const snapshotNs = rfc3339Nanoseconds(
    doc.snapshot_taken_at,
    `${label} snapshot_taken_at`,
  );
  requireWindow(doc.snapshot_taken_at, label, binding);
  if (!Array.isArray(doc.rows) || doc.rows.length === 0) {
    fail(`${label} must contain at least one row`);
  }
  const ids = new Set();
  const rows = [];
  for (const [index, row] of doc.rows.entries()) {
    const rowLabel = `${label} row ${index + 1}`;
    closed(row, ["command_id", "created_at", "due_at", "state"], rowLabel);
    identifier(row.command_id, `${rowLabel} command_id`);
    if (ids.has(row.command_id)) {
      fail(`${label} repeats command_id ${row.command_id}`);
    }
    ids.add(row.command_id);
    const createdMs = rfc3339(row.created_at, `${rowLabel} created_at`);
    const createdNs = rfc3339Nanoseconds(
      row.created_at,
      `${rowLabel} created_at`,
    );
    requireWindow(row.created_at, rowLabel, binding);
    const dueMs = rfc3339(row.due_at, `${rowLabel} due_at`);
    const dueNs = rfc3339Nanoseconds(row.due_at, `${rowLabel} due_at`);
    requireWindow(row.due_at, rowLabel, binding);
    if (!outboxStates.has(row.state)) {
      fail(`${rowLabel} has an invalid state`);
    }
    if (createdNs > dueNs) {
      fail(`${rowLabel} must not be due before it was created`);
    }
    rows.push({ ...row, createdMs, createdNs, dueMs, dueNs });
  }
  return { snapshotMs, snapshotNs, rows };
}

const httpSampleHeader = [
  "timestamp",
  "kind",
  "status",
  "latency_seconds",
  "request_id",
  "idempotency_key",
  "attempt_ordinal",
  "attempt_limit",
  "requested_revision",
  "http_status",
  "problem_type",
  "problem_detail",
  "command_id",
  "environment_id",
  "candidate_sha",
];
const httpKinds = new Set(["read", "enqueue"]);
const httpStatuses = new Set(["ok", "error"]);
const staleRevisionProblemType =
  "https://ocservia.dev/problems/stale-revision";
const staleRevisionProblemDetail =
  "the node changed after this operation was prepared";

function knownPreMutationStaleRevision(attempt) {
  return (
    attempt.httpStatus === 409 &&
    attempt.problemType === staleRevisionProblemType &&
    attempt.problemDetail === staleRevisionProblemDetail
  );
}

function parseHttpSamples(entry, binding) {
  const lines = splitArtifactLines(
    entry.bytes.toString("utf8"),
    entry.name,
    "http samples",
  );
  if (lines.length < 2) {
    fail(`http samples artifact ${entry.name} needs a header and samples`);
  }
  const header = parseCsvRow(
    lines[0],
    `http samples artifact ${entry.name} header`,
  );
  if (JSON.stringify(header) !== JSON.stringify(httpSampleHeader)) {
    fail(`http samples artifact ${entry.name} has an invalid header`);
  }
  const state = {
    reads: [],
    readSuccesses: 0,
    enqueues: [],
    enqueueSuccesses: 0,
    okEnqueueLatencies: [],
    enqueueRequestIds: new Set(),
    okEnqueueRequestIds: new Set(),
    okEnqueueIdempotencyByCommand: new Map(),
    okEnqueueAttemptRequestIdByCommand: new Map(),
    logicalEnqueues: new Map(),
    configuredAttemptLimit: null,
  };
  for (const [index, line] of lines.slice(1).entries()) {
    const columns = parseCsvRow(
      line,
      `http samples artifact ${entry.name} row ${index + 2}`,
    );
    if (columns.length !== header.length) {
      fail(`http samples artifact ${entry.name} has a ragged row`);
    }
    const rowNumber = index + 2;
    const label = `http samples artifact ${entry.name} row ${rowNumber}`;
    rfc3339(columns[0], `${label} timestamp`);
    const timestampNs = rfc3339Nanoseconds(columns[0], `${label} timestamp`);
    requireWindow(columns[0], label, binding);
    requireBinding(columns[13], columns[14], label, binding);
    if (!httpKinds.has(columns[1])) {
      fail(`${label} has an invalid kind`);
    }
    if (!httpStatuses.has(columns[2])) {
      fail(`${label} has an invalid status`);
    }
    const latency = csvSecondsNanoseconds(
      columns[3],
      `${label} latency_seconds`,
    );
    const httpStatus = csvSafeNonNegativeInteger(
      columns[9],
      `${label} http_status`,
    );
    if (httpStatus !== 0 && (httpStatus < 100 || httpStatus > 599)) {
      fail(`${label} http_status must be zero or a three-digit HTTP status`);
    }
    if (columns[1] === "read") {
      if (
        columns.slice(4, 9).some((value) => value !== "") ||
        columns.slice(10, 13).some((value) => value !== "")
      ) {
        fail(`${label} read sample must not carry enqueue attempt fields`);
      }
      if ((columns[2] === "ok") !== (httpStatus === 200)) {
        fail(`${label} read outcome does not match its HTTP status`);
      }
      state.reads.push(latency.value);
      if (columns[2] === "ok") state.readSuccesses += 1;
    } else {
      // Each wire attempt has a unique request identity. Attempts sharing an
      // idempotency key are one logical enqueue and must form a strict,
      // bounded stale-revision retry chain.
      identifier(columns[4], `${label} request_id`);
      if (state.enqueueRequestIds.has(columns[4])) {
        fail(`${label} repeats enqueue request_id ${columns[4]}`);
      }
      state.enqueueRequestIds.add(columns[4]);
      identifier(columns[5], `${label} idempotency_key`);
      const attemptOrdinal = csvSafeNonNegativeInteger(
        columns[6],
        `${label} attempt_ordinal`,
      );
      const attemptLimit = csvSafeNonNegativeInteger(
        columns[7],
        `${label} attempt_limit`,
      );
      if (columns[4] !== `${columns[5]}.attempt-${attemptOrdinal}`) {
        fail(`${label} request_id does not bind its idempotency key and ordinal`);
      }
      if (attemptLimit !== 3) {
        fail(`${label} attempt_limit must equal the formal three-attempt bound`);
      }
      if (state.configuredAttemptLimit === null) {
        state.configuredAttemptLimit = attemptLimit;
      } else if (attemptLimit !== state.configuredAttemptLimit) {
        fail(`${label} changes the run-wide configured enqueue attempt limit`);
      }
      if (attemptOrdinal < 1 || attemptOrdinal > attemptLimit) {
        fail(`${label} attempt_ordinal exceeds its bounded attempt limit`);
      }
      const requestedRevision = csvSafeNonNegativeInteger(
        columns[8],
        `${label} requested_revision`,
      );
      const ok = httpStatus >= 200 && httpStatus < 300;
      if ((columns[2] === "ok") !== ok) {
        fail(`${label} enqueue outcome does not match its HTTP status`);
      }
      const commandId = columns[12];
      if (ok) {
        identifier(commandId, `${label} command_id`);
        if (columns[10] !== "" || columns[11] !== "") {
          fail(`${label} successful enqueue must not carry an RFC7807 problem`);
        }
      } else if (commandId !== "") {
        fail(`${label} failed enqueue must not carry a command_id`);
      }

      let logical = state.logicalEnqueues.get(columns[5]);
      if (!logical) {
        if (attemptOrdinal !== 1) {
          fail(`${label} logical enqueue must begin with attempt ordinal 1`);
        }
        logical = {
          attemptLimit,
          attempts: [],
          terminal: false,
          commandId: null,
          acceptedLatency: null,
        };
        state.logicalEnqueues.set(columns[5], logical);
      } else {
        const prior = logical.attempts.at(-1);
        if (attemptLimit !== logical.attemptLimit) {
          fail(`${label} changes the configured enqueue attempt limit`);
        }
        if (attemptOrdinal !== prior.attemptOrdinal + 1) {
          fail(`${label} enqueue attempt ordinals must be contiguous`);
        }
        if (!knownPreMutationStaleRevision(prior)) {
          fail(
            `${label} retries an outcome other than the known pre-mutation stale-revision conflict`,
          );
        }
        if (logical.terminal) {
          fail(`${label} follows a terminal enqueue attempt`);
        }
        if (requestedRevision <= prior.requestedRevision) {
          fail(`${label} did not advance the stale requested revision`);
        }
        if (timestampNs < prior.timestampNs + prior.latencyNanoseconds) {
          fail(`${label} begins before the stale 409 response completed`);
        }
      }
      const attempt = {
        attemptOrdinal,
        requestedRevision,
        httpStatus,
        problemType: columns[10],
        problemDetail: columns[11],
        timestampNs,
        latencyNanoseconds: latency.nanoseconds,
      };
      logical.attempts.push(attempt);
      logical.terminal =
        ok ||
        !knownPreMutationStaleRevision(attempt) ||
        attemptOrdinal === attemptLimit;
      if (ok) {
        if (state.okEnqueueRequestIds.has(commandId)) {
          fail(`${label} repeats accepted command_id ${commandId}`);
        }
        logical.commandId = commandId;
        logical.acceptedLatency = latency.value;
        state.okEnqueueRequestIds.add(commandId);
        state.okEnqueueIdempotencyByCommand.set(commandId, columns[5]);
        state.okEnqueueAttemptRequestIdByCommand.set(commandId, columns[4]);
      }
    }
  }
  if (state.reads.length === 0) {
    fail(`http samples artifact ${entry.name} needs at least one read sample`);
  }
  if (state.logicalEnqueues.size === 0) {
    fail(`http samples artifact ${entry.name} needs at least one enqueue sample`);
  }
  for (const logical of state.logicalEnqueues.values()) {
    if (!logical.terminal) {
      fail(
        `http samples artifact ${entry.name} ends with an incomplete stale-revision retry chain`,
      );
    }
    state.enqueues.push(logical.acceptedLatency ?? 0);
    if (logical.commandId !== null) {
      state.enqueueSuccesses += 1;
      state.okEnqueueLatencies.push(logical.acceptedLatency);
    }
  }
  return state;
}

function parseTelemetrySnapshot(entry, binding) {
  const label = `telemetry snapshot artifact ${entry.name}`;
  const doc = parseArtifactJSON(entry, "telemetry snapshot");
  closed(
    doc,
    [
      "environment_id",
      "candidate_sha",
      "snapshot_taken_at",
      "freshness_bound_seconds",
      "agents",
    ],
    label,
  );
  requireBinding(doc.environment_id, doc.candidate_sha, label, binding);
  positiveNumber(doc.freshness_bound_seconds, `${label} freshness_bound_seconds`);
  const boundNanoseconds = doc.freshness_bound_seconds * 1_000_000_000;
  if (!Number.isSafeInteger(boundNanoseconds)) {
    fail(`${label} freshness_bound_seconds must have exact nanosecond precision`);
  }
  const boundNs = BigInt(boundNanoseconds);
  const snapshotMs = rfc3339(doc.snapshot_taken_at, `${label} snapshot_taken_at`);
  const snapshotNs = rfc3339Nanoseconds(
    doc.snapshot_taken_at,
    `${label} snapshot_taken_at`,
  );
  requireWindow(doc.snapshot_taken_at, label, binding);
  if (!Array.isArray(doc.agents) || doc.agents.length === 0) {
    fail(`${label} must contain at least one agent`);
  }
  const ids = new Set();
  const agents = [];
  for (const [index, agent] of doc.agents.entries()) {
    const agentLabel = `${label} agent ${index + 1}`;
    closed(agent, ["agent_id", "last_telemetry_at"], agentLabel);
    identifier(agent.agent_id, `${agentLabel} agent_id`);
    if (ids.has(agent.agent_id)) {
      fail(`${label} repeats agent_id ${agent.agent_id}`);
    }
    ids.add(agent.agent_id);
    const telemetryMs = rfc3339(
      agent.last_telemetry_at,
      `${agentLabel} last_telemetry_at`,
    );
    const telemetryNs = rfc3339Nanoseconds(
      agent.last_telemetry_at,
      `${agentLabel} last_telemetry_at`,
    );
    requireWindow(agent.last_telemetry_at, agentLabel, binding);
    if (telemetryNs > snapshotNs) {
      fail(`${agentLabel} must not be newer than the snapshot`);
    }
    agents.push({ ...agent, telemetryMs, telemetryNs });
  }
  return {
    snapshotMs,
    snapshotNs,
    boundSeconds: doc.freshness_bound_seconds,
    boundNs,
    agents,
  };
}

function parseAuditCorrelation(entry, binding) {
  const label = `audit correlation artifact ${entry.name}`;
  const doc = parseArtifactJSON(entry, "audit correlation");
  closed(doc, ["environment_id", "candidate_sha", "writes"], label);
  requireBinding(doc.environment_id, doc.candidate_sha, label, binding);
  if (!Array.isArray(doc.writes) || doc.writes.length === 0) {
    fail(`${label} must contain at least one write`);
  }
  const ids = new Set();
  for (const [index, write] of doc.writes.entries()) {
    const writeLabel = `${label} write ${index + 1}`;
    closed(
      write,
      [
        "write_id",
        "intent_recorded",
        "intent_request_id",
        "result_recorded",
        "result_request_id",
      ],
      writeLabel,
    );
    identifier(write.write_id, `${writeLabel} write_id`);
    if (ids.has(write.write_id)) {
      fail(`${label} repeats write_id ${write.write_id}`);
    }
    ids.add(write.write_id);
    boolean(write.intent_recorded, `${writeLabel} intent_recorded`);
    if (write.intent_recorded) {
      identifier(write.intent_request_id, `${writeLabel} intent_request_id`);
    } else if (write.intent_request_id !== "") {
      fail(`${writeLabel} unrecorded intent must not carry a request_id`);
    }
    boolean(write.result_recorded, `${writeLabel} result_recorded`);
    if (write.result_recorded) {
      identifier(write.result_request_id, `${writeLabel} result_request_id`);
    } else if (write.result_request_id !== "") {
      fail(`${writeLabel} unrecorded result must not carry a request_id`);
    }
  }
  return { writes: doc.writes };
}

function parsePostgresRecovery(entry, binding) {
  const label = `postgres recovery artifact ${entry.name}`;
  const doc = parseArtifactJSON(entry, "postgres recovery");
  closed(
    doc,
    [
      "environment_id",
      "candidate_sha",
      "outage_declared_at",
      "rto_started_at",
      "service_restored_at",
      "acknowledged",
      "failover",
      "recovery",
    ],
    label,
  );
  requireBinding(doc.environment_id, doc.candidate_sha, label, binding);
  const outageMs = rfc3339(doc.outage_declared_at, `${label} outage_declared_at`);
  const outageNs = rfc3339Nanoseconds(
    doc.outage_declared_at,
    `${label} outage_declared_at`,
  );
  requireWindow(doc.outage_declared_at, label, binding);
  const rtoStartedNs = rfc3339Nanoseconds(
    doc.rto_started_at,
    `${label} rto_started_at`,
  );
  requireWindow(doc.rto_started_at, label, binding);
  const restoredServiceMs = rfc3339(
    doc.service_restored_at,
    `${label} service_restored_at`,
  );
  const restoredServiceNs = rfc3339Nanoseconds(
    doc.service_restored_at,
    `${label} service_restored_at`,
  );
  requireWindow(doc.service_restored_at, label, binding);
  if (restoredServiceNs <= rtoStartedNs) {
    fail(`${label} must restore service after its same-clock RTO boundary`);
  }
  if (!Array.isArray(doc.acknowledged) || doc.acknowledged.length === 0) {
    fail(`${label} must acknowledge at least one marker transaction`);
  }
  const acknowledged = [];
  const txids = new Set();
  for (const [index, marker] of doc.acknowledged.entries()) {
    const markerLabel = `${label} acknowledged marker ${index + 1}`;
    closed(marker, ["txid", "acknowledged_at"], markerLabel);
    identifier(marker.txid, `${markerLabel} txid`);
    if (txids.has(marker.txid)) {
      fail(`${label} repeats txid ${marker.txid}`);
    }
    txids.add(marker.txid);
    const ackMs = rfc3339(marker.acknowledged_at, `${markerLabel} acknowledged_at`);
    const ackNs = rfc3339Nanoseconds(
      marker.acknowledged_at,
      `${markerLabel} acknowledged_at`,
    );
    requireWindow(marker.acknowledged_at, markerLabel, binding);
    if (ackNs > outageNs) {
      fail(`${markerLabel} must be acknowledged before the declared outage`);
    }
    acknowledged.push({
      txid: marker.txid,
      acknowledgedAtMs: ackMs,
      acknowledgedAtNs: ackNs,
    });
  }
  object(doc.failover, `${label} failover`);
  closed(
    doc.failover,
    ["old_primary", "new_primary", "isolated_at", "promoted_at", "isolated_primary_writes"],
    `${label} failover`,
  );
  identifier(doc.failover.old_primary, `${label} failover old_primary`);
  identifier(doc.failover.new_primary, `${label} failover new_primary`);
  if (doc.failover.old_primary === doc.failover.new_primary) {
    fail(`${label} failover must name distinct primary instances`);
  }
  const isolatedMs = rfc3339(doc.failover.isolated_at, `${label} failover isolated_at`);
  const isolatedNs = rfc3339Nanoseconds(
    doc.failover.isolated_at,
    `${label} failover isolated_at`,
  );
  requireWindow(doc.failover.isolated_at, label, binding);
  const promotedMs = rfc3339(doc.failover.promoted_at, `${label} failover promoted_at`);
  const promotedNs = rfc3339Nanoseconds(
    doc.failover.promoted_at,
    `${label} failover promoted_at`,
  );
  requireWindow(doc.failover.promoted_at, label, binding);
  if (doc.service_restored_at !== doc.failover.promoted_at) {
    fail(`${label} service restoration must exactly match the promoted primary boundary`);
  }
  if (!Array.isArray(doc.failover.isolated_primary_writes) || doc.failover.isolated_primary_writes.length === 0) {
    fail(`${label} must probe the isolated former primary`);
  }
  let dualPrimaryWriteAccepts = 0;
  for (const [index, attempt] of doc.failover.isolated_primary_writes.entries()) {
    const attemptLabel = `${label} isolated primary write ${index + 1}`;
    closed(attempt, ["at", "accepted"], attemptLabel);
    const atMs = rfc3339(attempt.at, `${attemptLabel} at`);
    const atNs = rfc3339Nanoseconds(attempt.at, `${attemptLabel} at`);
    requireWindow(attempt.at, attemptLabel, binding);
    if (atNs < promotedNs) {
      fail(`${attemptLabel} must be attempted after promotion`);
    }
    boolean(attempt.accepted, `${attemptLabel} accepted`);
    if (attempt.accepted) dualPrimaryWriteAccepts += 1;
  }
  object(doc.recovery, `${label} recovery`);
  closed(doc.recovery, ["restored_at", "present_txids"], `${label} recovery`);
  const restoredMs = rfc3339(doc.recovery.restored_at, `${label} recovery restored_at`);
  const restoredNs = rfc3339Nanoseconds(
    doc.recovery.restored_at,
    `${label} recovery restored_at`,
  );
  requireWindow(doc.recovery.restored_at, label, binding);
  if (restoredNs < outageNs) {
    fail(`${label} recovery must complete after the declared outage`);
  }
  if (
    !Array.isArray(doc.recovery.present_txids) ||
    doc.recovery.present_txids.length === 0
  ) {
    fail(`${label} recovery must anchor on at least one present marker`);
  }
  for (const txid of doc.recovery.present_txids) {
    identifier(txid, `${label} recovery present txid`);
    if (!txids.has(txid)) {
      fail(`${label} recovery reports the unacknowledged marker ${txid}`);
    }
  }
  const present = new Set(doc.recovery.present_txids);
  const presentMarkers = acknowledged.filter((marker) =>
    present.has(marker.txid),
  );
  const newestPresentNs = presentMarkers
    .map((marker) => marker.acknowledgedAtNs)
    .reduce((newest, value) => (value > newest ? value : newest));
  const acknowledgedTransactionLoss = acknowledged.filter(
    (marker) => !present.has(marker.txid),
  ).length;
  return {
    rtoSeconds: Number(restoredServiceNs - rtoStartedNs) / 1000000000,
    rpoSeconds: Number(outageNs - newestPresentNs) / 1000000000,
    dualPrimaryWriteAccepts,
    acknowledgedTransactionLoss,
    acknowledgedCount: acknowledged.length,
    presentCount: presentMarkers.length,
    isolatedWriteCount: doc.failover.isolated_primary_writes.length,
  };
}

function parsePitrReport(entry, binding) {
  const label = `PITR report artifact ${entry.name}`;
  const doc = parseArtifactJSON(entry, "PITR report");
  closed(
    doc,
    [
      "environment_id",
      "candidate_sha",
      "marker_a",
      "restore_point_created_at",
      "marker_b",
      "restore",
    ],
    label,
  );
  requireBinding(doc.environment_id, doc.candidate_sha, label, binding);
  const markerFields = (marker, markerLabel) => {
    closed(marker, ["txid", "written_at"], markerLabel);
    identifier(marker.txid, `${markerLabel} txid`);
    rfc3339(marker.written_at, `${markerLabel} written_at`);
    const writtenNs = rfc3339Nanoseconds(
      marker.written_at,
      `${markerLabel} written_at`,
    );
    requireWindow(marker.written_at, markerLabel, binding);
    return writtenNs;
  };
  const markerANs = markerFields(doc.marker_a, `${label} marker_a`);
  if (doc.marker_a.txid === doc.marker_b.txid) {
    fail(`${label} markers must use distinct transaction identifiers`);
  }
  rfc3339(
    doc.restore_point_created_at,
    `${label} restore_point_created_at`,
  );
  const restorePointNs = rfc3339Nanoseconds(
    doc.restore_point_created_at,
    `${label} restore_point_created_at`,
  );
  requireWindow(doc.restore_point_created_at, label, binding);
  const markerBNs = markerFields(doc.marker_b, `${label} marker_b`);
  object(doc.restore, `${label} restore`);
  closed(
    doc.restore,
    ["restored_at", "marker_a_present", "marker_b_present"],
    `${label} restore`,
  );
  rfc3339(doc.restore.restored_at, `${label} restore restored_at`);
  const restoredNs = rfc3339Nanoseconds(
    doc.restore.restored_at,
    `${label} restore restored_at`,
  );
  requireWindow(doc.restore.restored_at, label, binding);
  if (!(markerANs < restorePointNs && restorePointNs < markerBNs)) {
    fail(
      `${label} marker order must be marker_a < restore point < marker_b`,
    );
  }
  if (restoredNs < markerBNs) {
    fail(`${label} restore must complete after the last marker`);
  }
  boolean(doc.restore.marker_a_present, `${label} restore marker_a_present`);
  boolean(doc.restore.marker_b_present, `${label} restore marker_b_present`);
  if (doc.restore.marker_a_present !== true) {
    fail(`${label} restore must recover the pre-restore-point marker`);
  }
  if (doc.restore.marker_b_present !== false) {
    fail(`${label} restore must not recover the post-restore-point marker`);
  }
  return {};
}

function parseAgentSessions(entry, binding) {
  const label = `agent session inventory artifact ${entry.name}`;
  const doc = parseArtifactJSON(entry, "agent session inventory");
  closed(
    doc,
    [
      "environment_id",
      "candidate_sha",
      "snapshot_taken_at",
      "sessions",
      "scheduler_authority",
      "reconnect_storm",
    ],
    label,
  );
  requireBinding(doc.environment_id, doc.candidate_sha, label, binding);
  const snapshotMs = rfc3339(doc.snapshot_taken_at, `${label} snapshot_taken_at`);
  const snapshotNs = rfc3339Nanoseconds(
    doc.snapshot_taken_at,
    `${label} snapshot_taken_at`,
  );
  requireWindow(doc.snapshot_taken_at, label, binding);
  if (!Array.isArray(doc.sessions) || doc.sessions.length === 0) {
    fail(`${label} must contain at least one session`);
  }
  const ids = new Set();
  const nodes = new Set();
  const sessions = [];
  for (const [index, session] of doc.sessions.entries()) {
    const sessionLabel = `${label} session ${index + 1}`;
    closed(
      session,
      [
        "agent_id",
        "node",
        "endpoint_id",
        "agent_instance_id",
        "authorized",
        "connected",
        "owner_instance",
        "owner_incarnation",
        "connection_id",
        "owner_epoch",
        "owner_lease_until",
        "session_started_at",
        "connected_at",
        "session_expires_at",
        "reconnected_at",
        "reconnect_owner_instance",
        "reconnect_owner_incarnation",
        "reconnect_connection_id",
        "reconnect_owner_epoch",
      ],
      sessionLabel,
    );
    identifier(session.agent_id, `${sessionLabel} agent_id`);
    const node = normalizedUUIDIdentity(session.node, `${sessionLabel} node`);
    if (ids.has(session.agent_id)) {
      fail(`${label} repeats agent_id ${session.agent_id}`);
    }
    ids.add(session.agent_id);
    if (nodes.has(node)) {
      fail(`${label} repeats node ${node}`);
    }
    nodes.add(node);
    const endpointId = endpointIdentity(
      session.endpoint_id,
      `${sessionLabel} endpoint_id`,
    );
    const agentInstanceId = normalizedUUIDIdentity(
      session.agent_instance_id,
      `${sessionLabel} agent_instance_id`,
    );
    boolean(session.authorized, `${sessionLabel} authorized`);
    boolean(session.connected, `${sessionLabel} connected`);
    identifier(session.owner_instance, `${sessionLabel} owner_instance`);
    positiveDecimalString(
      session.owner_incarnation,
      `${sessionLabel} owner_incarnation`,
    );
    const connectionId = normalizedUUIDIdentity(
      session.connection_id,
      `${sessionLabel} connection_id`,
    );
    nonNegativeInteger(session.owner_epoch, `${sessionLabel} owner_epoch`);
    if (session.owner_epoch < 1) {
      fail(`${sessionLabel} owner_epoch must be positive`);
    }
    const ownerLeaseUntilMs = rfc3339(
      session.owner_lease_until,
      `${sessionLabel} owner_lease_until`,
    );
    const ownerLeaseUntilNs = rfc3339Nanoseconds(
      session.owner_lease_until,
      `${sessionLabel} owner_lease_until`,
    );
    if (ownerLeaseUntilNs <= snapshotNs) {
      fail(`${sessionLabel} owner lease must remain live after the snapshot`);
    }
    const startedMs = rfc3339(
      session.session_started_at,
      `${sessionLabel} session_started_at`,
    );
    const startedNs = rfc3339Nanoseconds(
      session.session_started_at,
      `${sessionLabel} session_started_at`,
    );
    requireWindow(session.session_started_at, sessionLabel, binding);
    if (startedNs > snapshotNs) {
      fail(`${sessionLabel} must start before the snapshot`);
    }
    const connectedAtMs = rfc3339(
      session.connected_at,
      `${sessionLabel} connected_at`,
    );
    const connectedAtNs = rfc3339Nanoseconds(
      session.connected_at,
      `${sessionLabel} connected_at`,
    );
    requireWindow(session.connected_at, sessionLabel, binding);
    if (connectedAtNs > snapshotNs) {
      fail(`${sessionLabel} must connect before the snapshot`);
    }
    const sessionExpiresAtMs = rfc3339(
      session.session_expires_at,
      `${sessionLabel} session_expires_at`,
    );
    const sessionExpiresAtNs = rfc3339Nanoseconds(
      session.session_expires_at,
      `${sessionLabel} session_expires_at`,
    );
    if (sessionExpiresAtNs <= snapshotNs) {
      fail(`${sessionLabel} session must remain live after the snapshot`);
    }
    const reconnectedMs = rfc3339(
      session.reconnected_at,
      `${sessionLabel} reconnected_at`,
    );
    const reconnectedNs = rfc3339Nanoseconds(
      session.reconnected_at,
      `${sessionLabel} reconnected_at`,
    );
    requireWindow(session.reconnected_at, sessionLabel, binding);
    if (reconnectedNs > snapshotNs) {
      fail(`${sessionLabel} must reconnect before the snapshot`);
    }
    identifier(
      session.reconnect_owner_instance,
      `${sessionLabel} reconnect_owner_instance`,
    );
    positiveDecimalString(
      session.reconnect_owner_incarnation,
      `${sessionLabel} reconnect_owner_incarnation`,
    );
    const reconnectConnectionId = normalizedUUIDIdentity(
      session.reconnect_connection_id,
      `${sessionLabel} reconnect_connection_id`,
    );
    nonNegativeInteger(
      session.reconnect_owner_epoch,
      `${sessionLabel} reconnect_owner_epoch`,
    );
    if (session.reconnect_owner_epoch < 1) {
      fail(`${sessionLabel} reconnect_owner_epoch must be positive`);
    }
    sessions.push({
      agentId: session.agent_id,
      node,
      endpointId,
      agentInstanceId,
      ownerInstance: session.owner_instance,
      ownerIncarnation: session.owner_incarnation,
      connectionId,
      ownerEpoch: session.owner_epoch,
      ownerLeaseUntil: session.owner_lease_until,
      ownerLeaseUntilMs,
      ownerLeaseUntilNs,
      authorized: session.authorized,
      connected: session.connected,
      startedMs,
      startedNs,
      connectedAt: session.connected_at,
      connectedAtMs,
      connectedAtNs,
      sessionExpiresAt: session.session_expires_at,
      sessionExpiresAtMs,
      sessionExpiresAtNs,
      reconnectedAt: session.reconnected_at,
      reconnectedMs,
      reconnectedNs,
      reconnectOwnerInstance: session.reconnect_owner_instance,
      reconnectOwnerIncarnation: session.reconnect_owner_incarnation,
      reconnectConnectionId,
      reconnectOwnerEpoch: session.reconnect_owner_epoch,
    });
  }
  object(doc.scheduler_authority, `${label} scheduler authority`);
  closed(
    doc.scheduler_authority,
    ["instance", "incarnation", "epoch", "lease_until"],
    `${label} scheduler authority`,
  );
  identifier(
    doc.scheduler_authority.instance,
    `${label} scheduler authority instance`,
  );
  positiveDecimalString(
    doc.scheduler_authority.incarnation,
    `${label} scheduler authority incarnation`,
  );
  nonNegativeInteger(
    doc.scheduler_authority.epoch,
    `${label} scheduler authority epoch`,
  );
  if (doc.scheduler_authority.epoch < 1) {
    fail(`${label} scheduler authority epoch must be positive`);
  }
  const schedulerLeaseUntilMs = rfc3339(
    doc.scheduler_authority.lease_until,
    `${label} scheduler authority lease_until`,
  );
  const schedulerLeaseUntilNs = rfc3339Nanoseconds(
    doc.scheduler_authority.lease_until,
    `${label} scheduler authority lease_until`,
  );
  if (schedulerLeaseUntilNs <= snapshotNs) {
    fail(`${label} scheduler lease must remain live after the snapshot`);
  }
  object(doc.reconnect_storm, `${label} reconnect storm`);
  closed(doc.reconnect_storm, ["bulk_disconnect_at"], `${label} reconnect storm`);
  const disconnectMs = rfc3339(
    doc.reconnect_storm.bulk_disconnect_at,
    `${label} reconnect storm bulk_disconnect_at`,
  );
  const disconnectNs = rfc3339Nanoseconds(
    doc.reconnect_storm.bulk_disconnect_at,
    `${label} reconnect storm bulk_disconnect_at`,
  );
  requireWindow(doc.reconnect_storm.bulk_disconnect_at, label, binding);
  const authorizedConnected = [];
  for (const session of sessions) {
    // The pre-storm expected population: every inventoried session
    // existed before the bulk disconnect and recovered on its own
    // afterwards; recovery cannot be claimed on behalf of an agent.
    if (session.startedNs > disconnectNs) {
      fail(
        `${label} session for agent ${session.agentId} must predate the bulk disconnect`,
      );
    }
    if (
      compareRfc3339(
        session.reconnectedAt,
        doc.reconnect_storm.bulk_disconnect_at,
        `${label} session for agent ${session.agentId} reconnected_at`,
        `${label} reconnect storm bulk_disconnect_at`,
      ) <= 0
    ) {
      fail(
        `${label} session for agent ${session.agentId} must reconnect after the bulk disconnect`,
      );
    }
    if (session.authorized && session.connected) {
      authorizedConnected.push(session);
    }
  }
  if (authorizedConnected.length === 0) {
    fail(`${label} must contain at least one authorized connected session`);
  }
  const lastReconnectNs = authorizedConnected
    .map((session) => session.reconnectedNs)
    .reduce((worst, value) => (value > worst ? value : worst));
  return {
    agentIds: ids,
    authorizedConnectedIds: new Set(
      authorizedConnected.map((session) => session.agentId),
    ),
    authorizedConnectedCount: authorizedConnected.length,
    sessions,
    snapshotAt: doc.snapshot_taken_at,
    snapshotMs,
    snapshotNs,
    schedulerInstance: doc.scheduler_authority.instance,
    schedulerIncarnation: doc.scheduler_authority.incarnation,
    schedulerEpoch: doc.scheduler_authority.epoch,
    schedulerLeaseUntil: doc.scheduler_authority.lease_until,
    schedulerLeaseUntilMs,
    schedulerLeaseUntilNs,
    bulkDisconnectAt: doc.reconnect_storm.bulk_disconnect_at,
    disconnectMs,
    disconnectNs,
    stormRecoverySeconds:
      Number(lastReconnectNs - disconnectNs) / 1000000000,
  };
}

const relayNames = new Set(["relay-a", "relay-b"]);
const pathNames = new Set(["direct", "relay"]);

function parseRelayTransitions(entry, binding) {
  const state = {
    failures: [],
    activations: [],
    relaySessions: [],
    relayTraffic: [],
    pairCount: 0,
  };
  const relaySessionFields = [
    "event_type",
    "session_id",
    "path",
    "relay",
    "endpoint_id",
    "path_detail",
    "owner_fence_id",
    "owner_instance",
    "owner_incarnation",
    "connection_id",
    "owner_epoch",
    "authorization_revision",
    "negotiated_capabilities",
    "session_connected_at",
    "owner_lease_until",
    "session_expires_at",
  ];
  const relayFailureFields = [
    "event_type",
    "relay",
    "session_id",
    "owner_instance",
    "owner_incarnation",
    "connection_id",
    "owner_epoch",
    "owner_lease_until",
    "authority_lease_until",
    "fault_cut_at",
  ];
  streamEventLines(
    entry,
    "relay transition log",
    (record) => {
      switch (record.event_type) {
        case "relay_failed":
          return relayFailureFields;
        case "relay_active":
          return ["event_type", "relay"];
        case "session_observed":
          return relaySessionFields;
        case "path_active":
          return record.path === "relay"
            ? [
                "authenticated",
                ...relaySessionFields,
                ...(record.relay === "relay-a"
                  ? [
                      "topology_mode",
                      "topology_network_name",
                      "topology_agent_service",
                      "topology_network_internal",
                      "topology_agent_default_network_connected",
                      "topology_ready_at",
                      "relay_b_disabled_at",
                    ]
                  : ["relay_b_started_at"]),
                "command_id",
                "command_idempotency_key",
                "effect_idempotency_key",
                "effect_id",
                "result_observed_at",
              ]
            : ["event_type", "session_id", "path", "authenticated"];
        case "path_failed":
          return ["event_type", "session_id", "path"];
        default:
          return null;
      }
    },
    binding,
    (record, parsed, parsedNs) => {
      const label = `relay transition artifact ${entry.name}`;
      if (record.event_type === "relay_failed") {
        if (!relayNames.has(record.relay)) {
          fail(`${label} has an invalid relay name`);
        }
        const node = normalizedUUIDIdentity(
          record.session_id,
          `${label} failed relay session_id`,
        );
        identifier(record.owner_instance, `${label} failed relay owner_instance`);
        positiveDecimalString(
          record.owner_incarnation,
          `${label} failed relay owner_incarnation`,
        );
        const connectionId = normalizedUUIDIdentity(
          record.connection_id,
          `${label} failed relay connection_id`,
        );
        nonNegativeInteger(record.owner_epoch, `${label} failed relay owner_epoch`);
        if (record.owner_epoch < 1) {
          fail(`${label} failed relay owner_epoch must be positive`);
        }
        const ownerLeaseUntilNs = rfc3339Nanoseconds(
          record.owner_lease_until,
          `${label} failed relay owner_lease_until`,
        );
        const authorityLeaseUntilNs = rfc3339Nanoseconds(
          record.authority_lease_until,
          `${label} failed relay authority_lease_until`,
        );
        const faultCutNs = rfc3339Nanoseconds(
          record.fault_cut_at,
          `${label} failed relay fault_cut_at`,
        );
        if (
          faultCutNs !== parsedNs ||
          ownerLeaseUntilNs <= parsedNs ||
          authorityLeaseUntilNs <= faultCutNs
        ) {
          fail(
            `${label} failed relay proof or authority was not live at its boundary`,
          );
        }
        state.failures.push({
          relay: record.relay,
          node,
          ownerInstance: record.owner_instance,
          ownerIncarnation: record.owner_incarnation,
          connectionId,
          ownerEpoch: record.owner_epoch,
          timestampMs: parsed,
          timestampNs: parsedNs,
          faultCutNs,
        });
        return;
      }
      if (record.event_type === "relay_active") {
        if (!relayNames.has(record.relay)) {
          fail(`${label} has an invalid relay name`);
        }
        state.activations.push({
          relay: record.relay,
          timestampMs: parsed,
          timestampNs: parsedNs,
        });
        return;
      }
      identifier(record.session_id, `${label} session_id`);
      if (!pathNames.has(record.path)) {
        fail(`${label} has an invalid path name`);
      }
      if (record.event_type === "path_active") {
        boolean(record.authenticated, `${label} authenticated`);
      }
      if (
        record.event_type === "session_observed" ||
        (record.event_type === "path_active" && record.path === "relay")
      ) {
          if (!relayNames.has(record.relay)) {
            fail(`${label} has an invalid relay name`);
          }
          const node = normalizedUUIDIdentity(
            record.session_id,
            `${label} relay session_id`,
          );
          const endpointId = endpointIdentity(
            record.endpoint_id,
            `${label} relay endpoint_id`,
          );
          if (
            typeof record.path_detail !== "string" ||
            !record.path_detail.includes(record.relay)
          ) {
            fail(`${label} relay path_detail does not identify ${record.relay}`);
          }
          const ownerFenceId = normalizedUUIDIdentity(
            record.owner_fence_id,
            `${label} relay owner_fence_id`,
          );
          identifier(record.owner_instance, `${label} relay owner_instance`);
          positiveDecimalString(
            record.owner_incarnation,
            `${label} relay owner_incarnation`,
          );
          const connectionId = normalizedUUIDIdentity(
            record.connection_id,
            `${label} relay connection_id`,
          );
          nonNegativeInteger(record.owner_epoch, `${label} relay owner_epoch`);
          if (record.owner_epoch < 1) {
            fail(`${label} relay owner_epoch must be positive`);
          }
          nonNegativeInteger(
            record.authorization_revision,
            `${label} relay authorization_revision`,
          );
          const sessionConnectedNs = rfc3339Nanoseconds(
            record.session_connected_at,
            `${label} relay session_connected_at`,
          );
          const ownerLeaseUntilNs = rfc3339Nanoseconds(
            record.owner_lease_until,
            `${label} relay owner_lease_until`,
          );
          const sessionExpiresAtNs = rfc3339Nanoseconds(
            record.session_expires_at,
            `${label} relay session_expires_at`,
          );
          let topologyReadyNs = null;
          let relayBDisabledNs = null;
          let relayBStartedNs = null;
          if (record.event_type === "path_active" && record.relay === "relay-a") {
            if (record.topology_mode !== "relay-a-only") {
              fail(`${label} relay-a traffic lacks the controlled topology proof`);
            }
            if (
              typeof record.topology_network_name !== "string" ||
              !/^[a-z0-9][a-z0-9_-]{0,191}_relay-a-only$/.test(
                record.topology_network_name,
              ) ||
              record.topology_agent_service !== "agent-fd-a-01" ||
              record.topology_network_internal !== true ||
              record.topology_agent_default_network_connected !== false
            ) {
              fail(`${label} relay-a controlled topology attributes are invalid`);
            }
            topologyReadyNs = rfc3339Nanoseconds(
              record.topology_ready_at,
              `${label} relay-a topology_ready_at`,
            );
            relayBDisabledNs = rfc3339Nanoseconds(
              record.relay_b_disabled_at,
              `${label} relay-b disabled_at`,
            );
            if (
              topologyReadyNs > relayBDisabledNs ||
              relayBDisabledNs >= parsedNs
            ) {
              fail(`${label} relay-a controlled topology boundaries are invalid`);
            }
          }
          if (record.event_type === "path_active" && record.relay === "relay-b") {
            relayBStartedNs = rfc3339Nanoseconds(
              record.relay_b_started_at,
              `${label} relay-b started_at`,
            );
            if (relayBStartedNs >= parsedNs) {
              fail(`${label} relay-b traffic does not follow its bounded start`);
            }
          }
          if (
            sessionConnectedNs > parsedNs ||
            ownerLeaseUntilNs <= parsedNs ||
            sessionExpiresAtNs <= parsedNs
          ) {
            fail(`${label} relay traffic is not bound to a live session`);
          }
          if (
            !Array.isArray(record.negotiated_capabilities) ||
            !record.negotiated_capabilities.includes("ocserv.fencing.v2") ||
            new Set(record.negotiated_capabilities).size !==
              record.negotiated_capabilities.length
          ) {
            fail(`${label} relay traffic lacks the authenticated fencing capability`);
          }
          for (const [index, capability] of record.negotiated_capabilities.entries()) {
            identifier(capability, `${label} relay capability ${index + 1}`);
          }
          const session = {
            node,
            endpointId,
            relay: record.relay,
            authenticated: record.authenticated === true,
            ownerFenceId,
            ownerInstance: record.owner_instance,
            ownerIncarnation: record.owner_incarnation,
            connectionId,
            ownerEpoch: record.owner_epoch,
            timestampMs: parsed,
            timestampNs: parsedNs,
            sessionConnectedAt: record.session_connected_at,
            sessionConnectedNs,
            ownerLeaseUntilNs,
            sessionExpiresAtNs,
            topologyReadyNs,
            relayBDisabledNs,
            relayBStartedNs,
          };
          if (record.event_type === "path_active") {
            identifier(record.command_id, `${label} relay command_id`);
            identifier(
              record.command_idempotency_key,
              `${label} relay command_idempotency_key`,
            );
            identifier(
              record.effect_idempotency_key,
              `${label} relay effect_idempotency_key`,
            );
            identifier(record.effect_id, `${label} relay effect_id`);
            const resultObservedNs = rfc3339Nanoseconds(
              record.result_observed_at,
              `${label} relay result_observed_at`,
            );
            if (resultObservedNs > parsedNs) {
              fail(`${label} relay command result postdates the path proof`);
            }
            Object.assign(session, {
              commandId: record.command_id,
              commandIdempotencyKey: record.command_idempotency_key,
              effectIdempotencyKey: record.effect_idempotency_key,
              effectId: record.effect_id,
              resultObservedNs,
            });
            state.relayTraffic.push(session);
          }
          state.relaySessions.push(session);
      }
    },
  );
  // A takeover is only proven by authenticated session traffic flowing
  // through a replacement relay after the failed relay went down; a
  // control-plane "active" record alone proves nothing about traffic.
  let worstTakeoverNs = null;
  for (const failure of state.failures) {
    const predecessor = state.relaySessions
      .filter(
        (session) =>
          session.relay === failure.relay &&
          session.timestampNs <= failure.timestampNs &&
          session.node === failure.node &&
          session.ownerInstance === failure.ownerInstance &&
          session.ownerIncarnation === failure.ownerIncarnation &&
          session.connectionId === failure.connectionId &&
          session.ownerEpoch === failure.ownerEpoch &&
          session.ownerLeaseUntilNs > failure.timestampNs &&
          session.authenticated &&
          typeof session.commandId === "string",
      )
      .at(-1);
    if (!predecessor) {
      fail(
        `relay transition artifact ${entry.name} lacks a live pre-fault session through ${failure.relay}`,
      );
    }
    predecessor.precedesFailureTimestampNs = failure.timestampNs;
    const successor = state.relayTraffic.find(
      (traffic) =>
        traffic.relay !== failure.relay &&
        traffic.authenticated &&
        traffic.node === predecessor.node &&
        traffic.endpointId === predecessor.endpointId &&
        traffic.timestampNs > failure.timestampNs &&
        predecessor.relayBDisabledNs !== null &&
        predecessor.relayBDisabledNs < failure.timestampNs &&
        traffic.relayBStartedNs !== null &&
        traffic.relayBStartedNs > failure.timestampNs,
    );
    if (!successor) continue;
    successor.failureTimestampNs = failure.timestampNs;
    state.pairCount += 1;
    const delta = successor.timestampNs - failure.timestampNs;
    if (worstTakeoverNs === null || delta > worstTakeoverNs) {
      worstTakeoverNs = delta;
    }
  }
  if (worstTakeoverNs === null) {
    fail(
      `relay transition artifact ${entry.name} must record authenticated traffic through a replacement relay`,
    );
  }
  return {
    takeoverSeconds: Number(worstTakeoverNs) / 1000000000,
    pairCount: state.pairCount,
    relaySessions: state.relaySessions,
    relayTraffic: state.relayTraffic,
  };
}

// Nearest-rank percentile over an ascending-sorted list.
function nearestRank(sortedValues, quantile) {
  const rank = Math.max(1, Math.ceil(quantile * sortedValues.length));
  return sortedValues[rank - 1];
}

function growth(parsed, component, column, ratio) {
  const rows = parsed.componentRows.get(component);
  if (!rows || rows.length === 0) {
    fail(`resource samples must include ${component} samples`);
  }
  const byInstance = new Map();
  for (const row of rows) {
    if (!byInstance.has(row.instance)) byInstance.set(row.instance, []);
    byInstance.get(row.instance).push(row);
  }
  let worst = null;
  for (const instanceRows of byInstance.values()) {
    instanceRows.sort((left, right) => left.timestampMs - right.timestampMs);
    const baseline = instanceRows[0][column];
    const end = instanceRows.at(-1)[column];
    if (ratio && baseline <= 0) {
      fail(`resource samples ${component} baseline must be positive`);
    }
    const value = Math.max(
      0,
      ratio ? (end - baseline) / baseline : end - baseline,
    );
    if (worst === null || value > worst) worst = value;
  }
  return { value: worst, sampleCount: rows.length };
}

function parseStructuredArtifact(kind, entry, binding) {
  switch (kind) {
    case "resource_samples":
      return parseResourceSamples(entry, binding);
    case "timeline":
      return parseTimeline(entry, binding);
    case "epoch_events":
      return parseEpochEvents(entry, binding);
    case "authority_cut":
      return parseAuthorityCut(entry, binding);
    case "command_trace":
      return parseCommandTrace(entry, binding);
    case "outbox_snapshot":
      return parseOutboxSnapshot(entry, binding);
    case "http_samples":
      return parseHttpSamples(entry, binding);
    case "telemetry_snapshot":
      return parseTelemetrySnapshot(entry, binding);
    case "audit_correlation":
      return parseAuditCorrelation(entry, binding);
    case "postgres_recovery":
      return parsePostgresRecovery(entry, binding);
    case "pitr_report":
      return parsePitrReport(entry, binding);
    case "agent_sessions":
      return parseAgentSessions(entry, binding);
    case "relay_transitions":
      return parseRelayTransitions(entry, binding);
    default:
      fail(`unsupported structured artifact kind: ${kind}`);
  }
}

// Every derivation recomputes both the metric value and the sample count from
// the raw artifact bytes, so evidence cannot inflate either dimension.
function completedOwnerTakeovers(state) {
  const pairs = [];
  for (const expiry of state.ownerExpiries) {
    const successor = state.ownerRegistrations.find(
      (registration) =>
        registration.node === expiry.node &&
        registration.epoch === expiry.epoch + 1 &&
        registration.sessionConnectedNs !== undefined &&
        registration.timestampNs >= expiry.timestampNs &&
        registration.sessionConnectedNs >= registration.timestampNs,
    );
    if (!successor) continue;
    pairs.push({
      node: expiry.node,
      expiredEpoch: expiry.epoch,
      successorEpoch: successor.epoch,
      expiryTimestampMs: expiry.timestampMs,
      successorTimestampMs: successor.sessionConnectedMs,
      expiryTimestampNs: expiry.timestampNs,
      successorTimestampNs: successor.sessionConnectedNs,
      seconds:
        Number(successor.sessionConnectedNs - expiry.timestampNs) / 1000000000,
    });
  }
  return pairs;
}

function ownerTakeover(state) {
  const pairs = completedOwnerTakeovers(state);
  if (pairs.length === 0) {
    fail("epoch event log must record a completed connection-owner takeover");
  }
  return {
    value: Math.max(...pairs.map((pair) => pair.seconds)),
    sampleCount: pairs.length,
  };
}

function schedulerTakeover(state) {
  const pairs = [];
  for (const expiry of state.leaderExpiries) {
    const successor = state.leaderCommits.find(
      (commit) =>
        commit.epoch === expiry.epoch + 1 &&
        commit.accepted &&
        commit.timestampNs >= expiry.timestampNs,
    );
    if (!successor) continue;
    pairs.push(successor.timestampNs - expiry.timestampNs);
  }
  if (pairs.length === 0) {
    fail("epoch event log must record a completed scheduler takeover");
  }
  return {
    value:
      Number(pairs.reduce((worst, value) => (value > worst ? value : worst))) /
      1000000000,
    sampleCount: pairs.length,
  };
}

function commandCompletionP99(state) {
  const completions = [];
  for (const command of state.commands.values()) {
    if (command.proofOnly) continue;
    if (command.firstResultAtMicros !== null) {
      completions.push(
        Number(command.firstResultAtMicros - command.enqueuedAtMicros) /
          1000000,
      );
    }
  }
  if (completions.length === 0) {
    fail("command trace must record at least one completed command");
  }
  completions.sort((left, right) => left - right);
  return {
    value: nearestRank(completions, 0.99),
    sampleCount: completions.length,
  };
}

// The dispatch ratio classifies every accepted (enqueued) command: an
// undispatched command is a miss, and only each command's first dispatch
// attempt can satisfy the bound.
function dispatchRatio(state) {
  const profileCommands = [...state.commands.values()].filter(
    (command) => !command.proofOnly,
  );
  if (profileCommands.length === 0) {
    fail("command trace must record at least one accepted command");
  }
  let withinBound = 0;
  for (const command of profileCommands) {
    if (command.dispatchedAtMicros === null) continue;
    if (
      Number(command.dispatchedAtMicros - command.enqueuedAtMicros) / 1000000 <=
      state.dispatchBoundSeconds
    ) {
      withinBound += 1;
    }
  }
  return {
    value: withinBound / profileCommands.length,
    sampleCount: profileCommands.length,
  };
}

function outboxMetrics(snapshot) {
  const total = snapshot.rows.length;
  let pendingDue = 0;
  let unknown = 0;
  let terminalOrReconciled = 0;
  let oldestDueAgeSeconds = 0;
  for (const row of snapshot.rows) {
    if (row.state === "pending" && row.dueNs <= snapshot.snapshotNs) {
      pendingDue += 1;
      const age = Number(snapshot.snapshotNs - row.createdNs) / 1_000_000_000;
      oldestDueAgeSeconds = Math.max(oldestDueAgeSeconds, age);
    }
    if (row.state === "unknown") unknown += 1;
    if (
      row.state === "terminal" ||
      row.state === "reconciliation_active" ||
      row.state === "unknown_reconciling"
    ) {
      terminalOrReconciled += 1;
    }
  }
  return {
    unreconciledDueRowCount: { value: pendingDue, sampleCount: total },
    unreconciledUnknownCommandCount: { value: unknown, sampleCount: total },
    terminalOrReconciledRatio: {
      value: terminalOrReconciled / total,
      sampleCount: total,
    },
    oldestDueAgeSeconds: { value: oldestDueAgeSeconds, sampleCount: total },
  };
}

function enqueueLatencyP95(state) {
  if (state.enqueueSuccesses === 0) {
    fail("http samples must record at least one successful enqueue");
  }
  const sorted = [...state.okEnqueueLatencies].sort((left, right) => left - right);
  return {
    value: nearestRank(sorted, 0.95),
    sampleCount: state.enqueueSuccesses,
  };
}

function telemetryFreshRatio(state) {
  let fresh = 0;
  for (const agent of state.agents) {
    if (state.snapshotNs - agent.telemetryNs <= state.boundNs) {
      fresh += 1;
    }
  }
  return { value: fresh / state.agents.length, sampleCount: state.agents.length };
}

function auditCompleteness(state) {
  let complete = 0;
  for (const write of state.writes) {
    if (write.intent_recorded && write.result_recorded) complete += 1;
  }
  return { value: complete / state.writes.length, sampleCount: state.writes.length };
}

function resourceQueueDepthEnd(parsed) {
  const newest = Math.max(...parsed.rows.map((row) => row.timestampMs));
  const finalRows = parsed.rows.filter((row) => row.timestampMs === newest);
  return {
    value: Math.max(...finalRows.map((row) => row.queueDepth)),
    sampleCount: finalRows.length,
  };
}

export const derivationRegistry = new Map([
  ["resource_samples.sample_span_seconds", {
    kind: "resource_samples",
    compute: (parsed) => ({
      value: parsed.sampleSpanSeconds,
      sampleCount: parsed.batchCount,
    }),
  }],
  ["resource_samples.max_sample_gap_seconds", {
    kind: "resource_samples",
    compute: (parsed) => ({
      value: parsed.maxSampleGapSeconds,
      sampleCount: parsed.batchCount,
    }),
  }],
  ["resource_samples.valid_sample_count", {
    kind: "resource_samples",
    compute: (parsed) => ({
      value: parsed.batchCount,
      sampleCount: parsed.batchCount,
    }),
  }],
  ["resource_samples.controller_rss_growth_ratio", {
    kind: "resource_samples",
    compute: (parsed) => growth(parsed, "controller", "rssBytes", true),
  }],
  ["resource_samples.transportd_rss_growth_ratio", {
    kind: "resource_samples",
    compute: (parsed) => growth(parsed, "transportd", "rssBytes", true),
  }],
  ["resource_samples.agent_rss_growth_ratio", {
    kind: "resource_samples",
    compute: (parsed) => growth(parsed, "agent", "rssBytes", true),
  }],
  ["resource_samples.controller_fd_growth", {
    kind: "resource_samples",
    compute: (parsed) => growth(parsed, "controller", "fdCount", false),
  }],
  ["resource_samples.transportd_fd_growth", {
    kind: "resource_samples",
    compute: (parsed) => growth(parsed, "transportd", "fdCount", false),
  }],
  ["resource_samples.agent_fd_growth", {
    kind: "resource_samples",
    compute: (parsed) => growth(parsed, "agent", "fdCount", false),
  }],
  ["resource_samples.controller_goroutine_growth", {
    kind: "resource_samples",
    compute: (parsed) => growth(parsed, "controller", "tasks", false),
  }],
  ["resource_samples.transportd_tokio_task_growth", {
    kind: "resource_samples",
    compute: (parsed) => growth(parsed, "transportd", "tasks", false),
  }],
  ["resource_samples.agent_tokio_task_growth", {
    kind: "resource_samples",
    compute: (parsed) => growth(parsed, "agent", "tasks", false),
  }],
  ["resource_samples.database_connection_growth", {
    kind: "resource_samples",
    compute: (parsed) => growth(parsed, "postgres", "dbConnections", false),
  }],
  ["resource_samples.queue_depth_end", {
    kind: "resource_samples",
    compute: resourceQueueDepthEnd,
  }],
  ["epoch_events.stale_owner_accept_count", {
    kind: "epoch_events",
    compute: (parsed) => ({
      value: parsed.staleOwnerAccepts,
      sampleCount: parsed.ownerAcceptCount,
    }),
  }],
  ["epoch_events.max_concurrent_owners", {
    kind: "epoch_events",
    compute: (parsed) => ({
      value: parsed.maxConcurrentOwners,
      sampleCount: parsed.ownerRegisteredCount,
    }),
  }],
  ["epoch_events.stale_scheduler_commit_count", {
    kind: "epoch_events",
    compute: (parsed) => ({
      value: parsed.staleSchedulerCommits,
      sampleCount: parsed.leaderCommitCount,
    }),
  }],
  ["epoch_events.max_concurrent_leaders", {
    kind: "epoch_events",
    compute: (parsed) => ({
      value: parsed.maxConcurrentLeaders,
      sampleCount: parsed.leaderAcquiredCount,
    }),
  }],
  ["epoch_events.owner_takeover_seconds", {
    kind: "epoch_events",
    compute: ownerTakeover,
  }],
  ["epoch_events.scheduler_takeover_seconds", {
    kind: "epoch_events",
    compute: schedulerTakeover,
  }],
  ["command_trace.max_inflight", {
    kind: "command_trace",
    compute: (parsed) => {
      const profileDispatchedCommands = [...parsed.commands.values()].filter(
        (command) => !command.proofOnly && command.dispatchedAtMicros !== null,
      ).length;
      if (profileDispatchedCommands === 0) {
        fail("command trace must record at least one dispatched command");
      }
      return {
        value: parsed.inflightSnapshot.expectedCount,
        sampleCount: profileDispatchedCommands,
      };
    },
  }],
  ["command_trace.completion_seconds_p99", {
    kind: "command_trace",
    compute: commandCompletionP99,
  }],
  ["command_trace.duplicate_effect_count", {
    kind: "command_trace",
    compute: (parsed) => ({
      value: parsed.duplicateEffectCount,
      sampleCount: parsed.effectIdSeen.size,
    }),
  }],
  ["command_trace.unmatched_result_count", {
    kind: "command_trace",
    compute: (parsed) => ({
      value: parsed.unmatchedResultCount,
      sampleCount: parsed.resultCount,
    }),
  }],
  ["command_trace.dispatch_within_bound_ratio", {
    kind: "command_trace",
    compute: dispatchRatio,
  }],
  ["command_trace.dispatch_bound_seconds", {
    kind: "command_trace",
    compute: (parsed) => ({
      value: parsed.dispatchBoundSeconds,
      sampleCount: [...parsed.commands.values()].filter(
        (command) => !command.proofOnly,
      ).length,
    }),
  }],
  ["outbox_snapshot.unreconciled_due_row_count", {
    kind: "outbox_snapshot",
    compute: (parsed) => outboxMetrics(parsed).unreconciledDueRowCount,
  }],
  ["outbox_snapshot.unreconciled_unknown_command_count", {
    kind: "outbox_snapshot",
    compute: (parsed) => outboxMetrics(parsed).unreconciledUnknownCommandCount,
  }],
  ["outbox_snapshot.terminal_or_reconciled_ratio", {
    kind: "outbox_snapshot",
    compute: (parsed) => outboxMetrics(parsed).terminalOrReconciledRatio,
  }],
  ["outbox_snapshot.oldest_due_age_seconds", {
    kind: "outbox_snapshot",
    compute: (parsed) => outboxMetrics(parsed).oldestDueAgeSeconds,
  }],
  ["http_samples.read_success_ratio", {
    kind: "http_samples",
    compute: (parsed) => ({
      value: parsed.readSuccesses / parsed.reads.length,
      sampleCount: parsed.reads.length,
    }),
  }],
  ["http_samples.enqueue_success_ratio", {
    kind: "http_samples",
    compute: (parsed) => ({
      value: parsed.enqueueSuccesses / parsed.enqueues.length,
      sampleCount: parsed.enqueues.length,
    }),
  }],
  ["http_samples.enqueue_latency_p95_seconds", {
    kind: "http_samples",
    compute: enqueueLatencyP95,
  }],
  ["telemetry_snapshot.fresh_ratio", {
    kind: "telemetry_snapshot",
    compute: telemetryFreshRatio,
  }],
  ["telemetry_snapshot.freshness_bound_seconds", {
    kind: "telemetry_snapshot",
    compute: (parsed) => ({
      value: parsed.boundSeconds,
      sampleCount: parsed.agents.length,
    }),
  }],
  ["audit_correlation.completeness_ratio", {
    kind: "audit_correlation",
    compute: auditCompleteness,
  }],
  ["postgres_recovery.dual_primary_write_accept_count", {
    kind: "postgres_recovery",
    compute: (parsed) => ({
      value: parsed.dualPrimaryWriteAccepts,
      sampleCount: parsed.isolatedWriteCount,
    }),
  }],
  ["postgres_recovery.acknowledged_transaction_loss_count", {
    kind: "postgres_recovery",
    compute: (parsed) => ({
      value: parsed.acknowledgedTransactionLoss,
      sampleCount: parsed.acknowledgedCount,
    }),
  }],
  ["postgres_recovery.rpo_seconds", {
    kind: "postgres_recovery",
    compute: (parsed) => ({
      value: parsed.rpoSeconds,
      sampleCount: parsed.presentCount,
    }),
  }],
  ["postgres_recovery.rto_seconds", {
    kind: "postgres_recovery",
    compute: (parsed) => ({ value: parsed.rtoSeconds, sampleCount: 1 }),
  }],
  ["agent_sessions.authorized_agent_count", {
    kind: "agent_sessions",
    compute: (parsed) => ({
      value: parsed.sessions.filter(
        (session) => session.authorized && session.connected,
      ).length,
      sampleCount: parsed.sessions.length,
    }),
  }],
  ["agent_sessions.reconnect_storm_recovery_seconds", {
    kind: "agent_sessions",
    compute: (parsed) => ({
      value: parsed.stormRecoverySeconds,
      sampleCount: parsed.authorizedConnectedCount,
    }),
  }],
  ["relay_transitions.relay_takeover_seconds", {
    kind: "relay_transitions",
    compute: (parsed) => ({
      value: parsed.takeoverSeconds,
      sampleCount: parsed.pairCount,
    }),
  }],
]);

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
  if (evidence.schema_version !== "ocservia.g6-evidence.v2") {
    fail("unexpected G6 evidence schema_version");
  }
  sha(evidence.candidate_sha, "evidence candidate_sha");
  digest(evidence.release_manifest_digest, "evidence release_manifest_digest");
  digest(evidence.slo_contract_digest, "evidence slo_contract_digest");
  digest(evidence.topology_digest, "evidence topology_digest");
  timestamp(evidence.started_at, "evidence started_at");
  timestamp(evidence.finished_at, "evidence finished_at");
  if (
    rfc3339Nanoseconds(evidence.finished_at, "evidence finished_at") <=
    rfc3339Nanoseconds(evidence.started_at, "evidence started_at")
  ) {
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

// Parses every structured artifact exactly once with the evidence binding
// context and cross-checks artifacts against each other.
function parseAllStructuredArtifacts(standardArtifacts, verifiedArtifacts, evidence) {
  const binding = {
    environmentId: evidence.environment.environment_id,
    candidateSha: evidence.candidate_sha,
    startedAtMs: Date.parse(evidence.started_at),
    finishedAtMs: Date.parse(evidence.finished_at),
    startedAtNs: rfc3339Nanoseconds(
      evidence.started_at,
      "evidence started_at",
    ),
    finishedAtNs: rfc3339Nanoseconds(
      evidence.finished_at,
      "evidence finished_at",
    ),
  };
  const parsed = new Map();
  for (const [kind, artifact] of standardArtifacts) {
    const entry = verifiedArtifacts.get(artifact.name);
    parsed.set(kind, parseStructuredArtifact(kind, entry, binding));
  }

  // The DB-clock authority cut is the durable source for live lease tuples.
  // Cross-check it against both the independently parsed epoch history and
  // the final session inventory so rebinding any one artifact and its digest
  // cannot manufacture a coherent authority boundary.
  const epochs = parsed.get("epoch_events");
  const authority = parsed.get("authority_cut");
  const sessions = parsed.get("agent_sessions");
  const relay = parsed.get("relay_transitions");
  const trace = parsed.get("command_trace");
  const timeline = parsed.get("timeline");
  const resourceSamples = parsed.get("resource_samples");
  if (authority.cutAt !== sessions.snapshotAt) {
    fail("authority cut_at does not exactly match the agent session snapshot");
  }
  const bulkDisconnect = timeline.get("bulk_disconnect_injected");
  const reconnectCompleted = timeline.get("reconnect_completed");
  const ownerPaused = timeline.get("owner_a_paused");
  const ownerAcquired = timeline.get("owner_b_acquired");
  const samplingStopped = timeline.get("resource_sampling_stopped");
  const apiSloMeasured = timeline.get("api_slo_measured");
  if (!bulkDisconnect || !reconnectCompleted) {
    fail("timeline must bind the reconnect storm boundaries");
  }
  if (!ownerPaused || !ownerAcquired) {
    fail("timeline must bind the connection-owner failover boundaries");
  }
  if (!samplingStopped || !apiSloMeasured) {
    fail("timeline must bind the resource sampler stop boundary");
  }
  if (
    resourceSamples.lastTimestampNs > samplingStopped.timestampNs ||
    samplingStopped.timestampNs - resourceSamples.lastTimestampNs >
      5_000_000_000n ||
    samplingStopped.timestampNs > apiSloMeasured.timestampNs
  ) {
    fail(
      "resource samples must end within five seconds of the graceful sampler stop boundary",
    );
  }
  if (
    ownerPaused.timestampNs >= ownerAcquired.timestampNs ||
    ownerAcquired.timestampNs >= bulkDisconnect.timestampNs
  ) {
    fail(
      "timeline connection-owner takeover must finish before the reconnect storm",
    );
  }
  for (const traffic of relay.relaySessions) {
    const session = sessions.sessions.find(
      (candidate) => candidate.node === traffic.node,
    );
    if (!session) {
      fail(`relay traffic references unmanaged session node ${traffic.node}`);
    }
    if (
      traffic.endpointId !== session.endpointId
    ) {
      fail(
        `relay traffic endpoint does not match managed session node ${traffic.node}`,
      );
    }
    const relayRegistrationKey = [
      traffic.node,
      traffic.ownerInstance,
      traffic.ownerIncarnation,
      traffic.connectionId,
      traffic.ownerEpoch,
    ].join(":");
    const relayRegistration = epochs.ownerRegistrationsByTerm.get(
      relayRegistrationKey,
    );
    if (
      !relayRegistration ||
      relayRegistration.timestampNs > traffic.sessionConnectedNs
    ) {
      fail(
        `relay traffic has no causal durable owner registration for node ${traffic.node}`,
      );
    }
  }
  const relayProofCommandIds = new Set();
  for (const traffic of relay.relayTraffic) {
    const command = trace.commands.get(traffic.commandId);
    const effects = trace.effects.get(traffic.effectIdempotencyKey) ?? [];
    const enqueuedNs =
      command?.enqueuedAtMicros === undefined
        ? null
        : BigInt(command.enqueuedAtMicros) * 1000n;
    const dispatchedNs =
      command?.dispatchedAtMicros === null ||
      command?.dispatchedAtMicros === undefined
        ? null
        : BigInt(command.dispatchedAtMicros) * 1000n;
    const isPreFaultProof =
      typeof traffic.precedesFailureTimestampNs === "bigint";
    const isPostFaultProof = typeof traffic.failureTimestampNs === "bigint";
    const commandTimingBound = isPreFaultProof
      ? enqueuedNs !== null &&
        dispatchedNs !== null &&
        traffic.relayBDisabledNs !== null &&
        enqueuedNs > traffic.relayBDisabledNs &&
        dispatchedNs > traffic.relayBDisabledNs &&
        enqueuedNs <= dispatchedNs &&
        dispatchedNs <= traffic.resultObservedNs &&
        traffic.resultObservedNs <= traffic.timestampNs &&
        traffic.timestampNs <= traffic.precedesFailureTimestampNs
      : isPostFaultProof &&
        enqueuedNs !== null &&
        dispatchedNs !== null &&
        traffic.relayBStartedNs !== null &&
        enqueuedNs > traffic.relayBStartedNs &&
        dispatchedNs > traffic.relayBStartedNs &&
        enqueuedNs > traffic.failureTimestampNs &&
        dispatchedNs > traffic.failureTimestampNs;
    if (
      !command ||
      command.idempotencyKey !== traffic.commandIdempotencyKey ||
      command.dispatchedAtMicros === null ||
      command.firstResultAtMicros === null ||
      !commandTimingBound ||
      BigInt(command.firstResultAtMicros) * 1000n !==
        traffic.resultObservedNs ||
      traffic.resultObservedNs > traffic.timestampNs ||
      effects.length !== 1 ||
      effects[0].commandId !== traffic.commandId ||
      effects[0].effectId !== traffic.effectId ||
      BigInt(effects[0].timestampMicros) * 1000n >
        traffic.resultObservedNs
    ) {
      fail(
        `relay authenticated traffic is not exactly bound to its successful command and durable effect for node ${traffic.node}`,
      );
    }
    command.proofOnly = true;
    relayProofCommandIds.add(traffic.commandId);
  }
  if (sessions.bulkDisconnectAt !== bulkDisconnect.timestamp) {
    fail("agent session reconnect storm does not match the timeline bulk disconnect");
  }
  if (
    compareRfc3339(
      reconnectCompleted.timestamp,
      bulkDisconnect.timestamp,
      "timeline reconnect completion",
      "timeline bulk disconnect",
    ) <= 0 ||
    compareRfc3339(
      reconnectCompleted.timestamp,
      sessions.snapshotAt,
      "timeline reconnect completion",
      "agent session snapshot",
    ) >= 0
  ) {
    fail("timeline reconnect completion must fall strictly between the storm and session snapshot");
  }
  const sessionNodes = new Set();
  const scenarioOwnerTakeovers = new Map(
    completedOwnerTakeovers(epochs)
      .filter(
        (pair) =>
          pair.expiryTimestampNs >= ownerPaused.timestampNs &&
          pair.successorTimestampNs > pair.expiryTimestampNs &&
          pair.successorTimestampNs <= ownerAcquired.timestampNs,
      )
      .map((pair) => [pair.node, pair]),
  );
  for (const session of sessions.sessions) {
    sessionNodes.add(session.node);
    const scenarioTakeover = scenarioOwnerTakeovers.get(session.node);
    if (!scenarioTakeover) {
      fail(
        `agent session inventory node ${session.node} has no completed lease-expiry connection-owner takeover within the owner failover timeline`,
      );
    }
    const latestOwner = epochs.ownerLatest.get(session.node);
    if (!latestOwner) {
      fail(
        `agent session inventory node ${session.node} has no registered connection-owner epoch`,
      );
    }
    if (session.ownerEpoch !== latestOwner.epoch) {
      fail(
        `agent session inventory owner_epoch ${session.ownerEpoch} does not match latest connection-owner epoch ${latestOwner.epoch} for node ${session.node}`,
      );
    }
    if (!epochs.ownerActive.get(session.node)?.has(session.ownerEpoch)) {
      fail(
        `agent session inventory owner_epoch ${session.ownerEpoch} is not active for node ${session.node}`,
      );
    }
    if (session.reconnectOwnerEpoch > session.ownerEpoch) {
      fail(
        `agent session inventory reconnect owner epoch exceeds the final owner epoch for node ${session.node}`,
      );
    }
    if (session.reconnectOwnerEpoch <= scenarioTakeover.successorEpoch) {
      fail(
        `agent session inventory reconnect owner epoch does not advance beyond the owner failover epoch for node ${session.node}`,
      );
    }
    const reconnectRegistrationKey = [
      session.node,
      session.reconnectOwnerInstance,
      session.reconnectOwnerIncarnation,
      session.reconnectConnectionId,
      session.reconnectOwnerEpoch,
    ].join(":");
    const reconnectRegistration = epochs.ownerRegistrationsByTerm.get(
      reconnectRegistrationKey,
    );
    if (!reconnectRegistration) {
      fail(
        `agent session inventory reconnect tuple has no durable owner registration for node ${session.node}`,
      );
    }
    if (session.reconnectedNs < reconnectRegistration.timestampNs) {
      fail(
        `agent session inventory transport reconnect predates the durable owner registration for node ${session.node}`,
      );
    }
    if (
      compareRfc3339(
        reconnectRegistration.timestamp,
        bulkDisconnect.timestamp,
        "durable reconnect registration",
        "timeline bulk disconnect",
      ) <= 0 ||
      compareRfc3339(
        reconnectRegistration.timestamp,
        reconnectCompleted.timestamp,
        "durable reconnect registration",
        "timeline reconnect completion",
      ) > 0
    ) {
      fail(
        `agent session inventory reconnect registration falls outside the timeline storm for node ${session.node}`,
      );
    }
    if (
      session.reconnectedNs <= bulkDisconnect.timestampNs ||
      session.reconnectedNs > reconnectCompleted.timestampNs
    ) {
      fail(
        `agent session inventory transport reconnect falls outside the timeline storm for node ${session.node}`,
      );
    }
    if (session.reconnectOwnerEpoch === session.ownerEpoch) {
      if (
        session.reconnectOwnerInstance !== session.ownerInstance ||
        session.reconnectOwnerIncarnation !== session.ownerIncarnation ||
        session.reconnectConnectionId !== session.connectionId
      ) {
        fail(
          `agent session inventory same-epoch reconnect tuple differs from the final owner for node ${session.node}`,
        );
      }
    } else if (
      compareRfc3339(
        session.connectedAt,
        reconnectRegistration.timestamp,
        "final session connected_at",
        "durable reconnect registration",
      ) < 0
    ) {
      fail(
        `agent session inventory replacement connection predates the reconnect registration for node ${session.node}`,
      );
    }
    const cutOwner = authority.owners.get(session.node);
    if (!cutOwner) {
      fail(`authority cut omits session owner node ${session.node}`);
    }
    const transport = authority.afterObservations.get(session.node);
    if (!transport) {
      fail(`authority cut transport bracket omits session node ${session.node}`);
    }
    if (
      transport.endpointId !== session.endpointId ||
      transport.agentInstanceId !== session.agentInstanceId ||
      transport.connectedAt !== session.connectedAt ||
      transport.sessionExpiresAt !== session.sessionExpiresAt ||
      transport.ownerInstance !== session.ownerInstance ||
      transport.ownerIncarnation !== session.ownerIncarnation ||
      transport.connectionId !== session.connectionId ||
      transport.ownerEpoch !== session.ownerEpoch
    ) {
      fail(
        `authority cut transport tuple does not match agent session node ${session.node}`,
      );
    }
    if (
      cutOwner.instance !== latestOwner.instance ||
      cutOwner.incarnation !== latestOwner.incarnation ||
      cutOwner.connectionId !== latestOwner.connectionId ||
      cutOwner.epoch !== latestOwner.epoch ||
      cutOwner.leaseUntil !== latestOwner.leaseUntil
    ) {
      fail(
        `authority cut owner tuple does not match latest epoch for node ${session.node}`,
      );
    }
    if (
      cutOwner.instance !== session.ownerInstance ||
      cutOwner.incarnation !== session.ownerIncarnation ||
      cutOwner.connectionId !== session.connectionId ||
      cutOwner.epoch !== session.ownerEpoch ||
      cutOwner.leaseUntil !== session.ownerLeaseUntil
    ) {
      fail(`authority cut owner tuple does not match session node ${session.node}`);
    }
  }
  for (const node of authority.owners.keys()) {
    if (!sessionNodes.has(node)) {
      fail(`authority cut contains owner node ${node} absent from sessions`);
    }
  }
  if (sessions.schedulerEpoch !== epochs.leaderMaxEpoch) {
    fail(
      `agent session inventory scheduler epoch ${sessions.schedulerEpoch} does not match latest scheduler epoch ${epochs.leaderMaxEpoch}`,
    );
  }
  if (!epochs.leaderActive.has(sessions.schedulerEpoch)) {
    fail(
      `agent session inventory scheduler epoch ${sessions.schedulerEpoch} is not active`,
    );
  }
  if (
    authority.scheduler.instance !== epochs.leaderLatest?.instance ||
    authority.scheduler.incarnation !== epochs.leaderLatest?.incarnation ||
    authority.scheduler.epoch !== epochs.leaderLatest?.epoch ||
    authority.scheduler.leaseUntil !== epochs.leaderLatest?.leaseUntil
  ) {
    fail("authority cut scheduler tuple does not match latest epoch");
  }
  if (
    authority.scheduler.instance !== sessions.schedulerInstance ||
    authority.scheduler.incarnation !== sessions.schedulerIncarnation ||
    authority.scheduler.epoch !== sessions.schedulerEpoch ||
    authority.scheduler.leaseUntil !== sessions.schedulerLeaseUntil
  ) {
    fail("authority cut scheduler tuple does not match agent sessions");
  }
  const liveSchedulerTerm = epochs.leaderTerms.get(authority.scheduler.epoch);
  if (
    !liveSchedulerTerm ||
    liveSchedulerTerm.instance !== authority.scheduler.instance ||
    liveSchedulerTerm.incarnation !== authority.scheduler.incarnation ||
    !liveSchedulerTerm.maintenanceCompletions.some(
      (completion) =>
        completion.maintenanceId === BigInt(authority.scheduler.maintenanceId) &&
        completion.markerCompletedAtNs ===
          authority.scheduler.maintenanceCompletedAtNs &&
        completion.timestampNs <= authority.cutNs,
    )
  ) {
    fail(
      "authority cut live scheduler term has no exact-term maintenance completion",
    );
  }

  // Telemetry must cover exactly the authorized connected session
  // population; submitting a fresh-looking subset cannot stand in for
  // the full fleet.
  const telemetry = parsed.get("telemetry_snapshot");
  const telemetryIds = new Set(telemetry.agents.map((agent) => agent.agent_id));
  for (const agentId of telemetryIds) {
    if (!sessions.authorizedConnectedIds.has(agentId)) {
      fail(
        `telemetry snapshot reports agent ${agentId} absent from the authorized connected session population`,
      );
    }
  }
  for (const agentId of sessions.authorizedConnectedIds) {
    if (!telemetryIds.has(agentId)) {
      fail(`telemetry snapshot omits the authorized connected agent ${agentId}`);
    }
  }

  // The accepted-write population is one identity chain: the successful
  // enqueue requests, outbox rows, and audit writes must describe the
  // exact same command set.
  const http = parsed.get("http_samples");
  const outbox = parsed.get("outbox_snapshot");
  const audit = parsed.get("audit_correlation");
  const acceptedIds = http.okEnqueueRequestIds;
  const outboxIds = new Set(outbox.rows.map((row) => row.command_id));
  for (const commandId of acceptedIds) {
    if (!outboxIds.has(commandId)) {
      fail(`outbox snapshot is missing the accepted enqueue ${commandId}`);
    }
  }
  for (const commandId of outboxIds) {
    if (!acceptedIds.has(commandId)) {
      fail(`outbox snapshot contains the unaccepted enqueue ${commandId}`);
    }
  }
  const auditIds = new Set(audit.writes.map((write) => write.write_id));
  for (const commandId of acceptedIds) {
    if (!auditIds.has(commandId)) {
      fail(`audit correlation is missing the accepted enqueue ${commandId}`);
    }
  }
  for (const writeId of auditIds) {
    if (!acceptedIds.has(writeId)) {
      fail(`audit correlation contains the unaccepted enqueue ${writeId}`);
    }
  }
  for (const write of audit.writes) {
    const acceptedRequestId =
      http.okEnqueueAttemptRequestIdByCommand.get(write.write_id);
    if (
      write.intent_recorded &&
      write.intent_request_id !== acceptedRequestId
    ) {
      fail(
        `audit intent for accepted enqueue ${write.write_id} does not retain the terminal HTTP request_id`,
      );
    }
    if (
      write.result_recorded &&
      write.result_request_id !== acceptedRequestId
    ) {
      fail(
        `audit result for accepted enqueue ${write.write_id} does not retain the terminal HTTP request_id`,
      );
    }
  }
  // The command trace's enqueued population must be exactly the accepted
  // write population: an accepted command that vanishes from the trace
  // would silently leave the dispatch ratio denominator.
  const traceIds = new Set(trace.commands.keys());
  for (const commandId of acceptedIds) {
    if (!traceIds.has(commandId)) {
      fail(`command trace is missing the accepted enqueue ${commandId}`);
    }
    if (
      trace.commands.get(commandId).idempotencyKey !==
      http.okEnqueueIdempotencyByCommand.get(commandId)
    ) {
      fail(
        `command trace enqueue ${commandId} does not retain the HTTP idempotency key`,
      );
    }
  }
  for (const commandId of traceIds) {
    if (!acceptedIds.has(commandId) && !relayProofCommandIds.has(commandId)) {
      fail(`command trace contains the unaccepted enqueue ${commandId}`);
    }
  }
  for (const commandId of relayProofCommandIds) {
    if (acceptedIds.has(commandId)) {
      fail(`relay proof command ${commandId} contaminates the bounded HTTP population`);
    }
  }
  if (
    trace.inflightSnapshot.expectedCount !== sessionNodes.size ||
    trace.inflightSnapshot.nodeIds.size !== sessionNodes.size ||
    [...sessionNodes].some(
      (nodeId) => !trace.inflightSnapshot.nodeIds.has(nodeId),
    )
  ) {
    fail(
      "command trace inflight snapshot does not cover the exact managed session population",
    );
  }
  return parsed;
}

// Recomputes every derivation from raw artifact bytes. The fixture generator
// and the verifier share this path so evidence and artifacts can never drift.
export function computeG6Derivations({
  sloText,
  artifactEntries,
  environmentId,
  candidateSha,
  startedAt,
  finishedAt,
}) {
  const slo = parseSlo(sloText);
  const standardArtifacts = new Map();
  const entriesByName = new Map();
  for (const entry of artifactEntries) {
    entriesByName.set(entry.name, entry);
    if (entry.kind === "harness_log") continue;
    if (standardArtifacts.has(entry.kind)) {
      fail(`artifact set must declare exactly one ${entry.kind} artifact`);
    }
    standardArtifacts.set(entry.kind, entry);
  }
  for (const kind of structuredArtifactKinds) {
    if (!standardArtifacts.has(kind)) {
      fail(`artifact set must declare exactly one ${kind} artifact`);
    }
  }
  const parsed = parseAllStructuredArtifacts(
    standardArtifacts,
    entriesByName,
    {
      environment: { environment_id: environmentId },
      candidate_sha: candidateSha,
      started_at: startedAt,
      finished_at: finishedAt,
    },
  );
  const results = new Map();
  for (const metric of Object.values(slo.metrics)) {
    if (metric.derivation === undefined) continue;
    const registry = derivationRegistry.get(metric.derivation);
    results.set(
      metric.derivation,
      registry.compute(parsed.get(registry.kind)),
    );
  }
  return results;
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
  for (const kind of structuredArtifactKinds) {
    if (!standardArtifacts.has(kind)) {
      fail(`evidence must declare exactly one ${kind} artifact`);
    }
  }
  const parsedArtifacts = parseAllStructuredArtifacts(
    standardArtifacts,
    verifiedArtifacts,
    evidence,
  );

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
    let derivation = null;
    if (contract.derivation !== undefined) {
      const registry = derivationRegistry.get(contract.derivation);
      const artifact = standardArtifacts.get(registry.kind);
      if (measurement.source_artifact_digest !== artifact.digest) {
        fail(
          `evidence measurement ${name} must reference the ${registry.kind} artifact`,
        );
      }
      const computed = registry.compute(parsedArtifacts.get(registry.kind));
      if (measurement.actual !== computed.value) {
        fail(
          `evidence measurement ${name} does not match the artifact-derived value`,
        );
      }
      if (measurement.sample_count !== computed.sampleCount) {
        fail(
          `evidence measurement ${name} sample_count does not match the artifact-derived value`,
        );
      }
      derivation = contract.derivation;
    }
    const passed = metricPass(measurement.actual, contract);
    measurementResults[name] = {
      actual: measurement.actual,
      limit: contract.limit,
      comparison: contract.comparison,
      unit: contract.unit,
      sample_count: measurement.sample_count,
      source_artifact_digest: measurement.source_artifact_digest,
      derivation,
      passed,
    };
    if (!passed) failureReasons.push(`metric failed: ${name}`);
  }

  const timelineArtifact = standardArtifacts.get("timeline");
  const timelineEvents = parsedArtifacts.get("timeline");
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
  const declaredMetrics = Object.values(slo.metrics).filter(
    (metric) => metric.declared_by_harness === true,
  );
  if (declaredMetrics.length > 0) {
    failureReasons.push("final pass requires verified metric producers");
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
    schema_version: "ocservia.g6-verdict.v2",
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
