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
const identifierPattern = /^[a-z0-9][a-z0-9._-]{0,127}$/;
const rfc3339Pattern =
  /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/;

// Structured artifact kinds the verifier parses and recomputes from. Every
// structured kind must appear exactly once in a final evidence bundle, while
// opaque harness_log files may appear any number of times (including zero).
export const structuredArtifactKinds = [
  "resource_samples",
  "timeline",
  "epoch_events",
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

function boolean(value, context) {
  if (typeof value !== "boolean") fail(`${context} must be a boolean`);
}

function identifier(value, context) {
  if (typeof value !== "string" || !identifierPattern.test(value)) {
    fail(`${context} has an invalid identifier`);
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

function requireWindow(parsedTimestamp, label, binding) {
  if (parsedTimestamp < binding.startedAtMs) {
    fail(`${label} timestamp precedes the evidence window`);
  }
  if (parsedTimestamp > binding.finishedAtMs) {
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
  let lastTimestamp;
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
    if (lastTimestamp !== undefined && parsed < lastTimestamp) {
      fail(`${kindLabel} artifact ${entry.name} timestamps must not decrease`);
    }
    requireWindow(parsed, `${label}`, binding);
    requireBinding(
      record.environment_id,
      record.candidate_sha,
      `${kindLabel} artifact ${entry.name} entry`,
      binding,
    );
    lastSequence = record.sequence;
    lastTimestamp = parsed;
    onRecord(record, parsed);
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

function csvNonNegativeInteger(text, context) {
  if (!/^\d+$/.test(text)) {
    fail(`${context} must be a non-negative integer`);
  }
  return Number(text);
}

function csvFiniteNumber(text, context) {
  const value = Number(text);
  if (!/^\d+(?:\.\d+)?$/.test(text) || !Number.isFinite(value)) {
    fail(`${context} must be a non-negative decimal number`);
  }
  return value;
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
  for (const component of resourceComponents) componentRows.set(component, []);
  for (const [index, line] of lines.slice(1).entries()) {
    const columns = line.split(",");
    if (columns.length !== header.length) {
      fail(`resource samples artifact ${entry.name} has a ragged row`);
    }
    const rowNumber = index + 2;
    const label = `resource samples artifact ${entry.name} row ${rowNumber}`;
    const parsedTimestamp = rfc3339(columns[0], `${label} timestamp`);
    requireWindow(parsedTimestamp, label, binding);
    requireBinding(columns[8], columns[9], label, binding);
    const component = columns[1];
    if (!resourceComponents.has(component)) {
      fail(`${label} has an unknown component: ${component}`);
    }
    if (!identifierPattern.test(columns[2])) {
      fail(`${label} has an invalid instance identifier`);
    }
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
  if (rows.length < 2) {
    fail(`resource samples artifact ${entry.name} needs at least two samples`);
  }
  const timestamps = rows.map((row) => row.timestampMs);
  timestamps.sort((left, right) => left - right);
  let maxGap = 0;
  for (let index = 1; index < timestamps.length; index += 1) {
    maxGap = Math.max(maxGap, timestamps[index] - timestamps[index - 1]);
  }
  return {
    rows,
    componentRows,
    sampleSpanSeconds: (timestamps.at(-1) - timestamps[0]) / 1000,
    maxSampleGapSeconds: maxGap / 1000,
  };
}

function parseTimeline(entry, binding) {
  const events = new Map();
  streamEventLines(
    entry,
    "timeline",
    () => ["event_id"],
    binding,
    (record, parsed) => {
      if (!eventPattern.test(record.event_id)) {
        fail(`timeline artifact ${entry.name} has an invalid event_id`);
      }
      if (events.has(record.event_id)) {
        fail(
          `timeline artifact ${entry.name} repeats event_id ${record.event_id}`,
        );
      }
      events.set(record.event_id, { sequence: record.sequence, timestampMs: parsed });
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
    ownerActive: new Map(),
    ownerExpired: new Set(),
    leaderMaxEpoch: 0,
    leaderActive: new Set(),
    leaderExpired: new Set(),
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
          return ["subject", "event_type", "node", "instance", "epoch"];
        case "owner_lease_expired":
        case "owner_retired":
          return ["subject", "event_type", "node", "epoch"];
        case "owner_accept":
          return ["subject", "event_type", "node", "instance", "epoch", "accepted"];
        case "leader_acquired":
          return ["subject", "event_type", "instance", "epoch"];
        case "leader_lease_expired":
          return ["subject", "event_type", "epoch"];
        case "leader_commit":
          return ["subject", "event_type", "instance", "epoch", "accepted"];
        default:
          return null;
      }
    },
    binding,
    (record, parsed) => {
      if (record.subject === "connection_owner") {
        identifier(record.node, "epoch event node");
        nonNegativeInteger(record.epoch, "epoch event epoch");
        if (record.epoch < 1) fail("epoch event epoch must be positive");
        const node = record.node;
        if (!state.ownerActive.has(node)) state.ownerActive.set(node, new Set());
        const active = state.ownerActive.get(node);
        const maxEpoch = state.ownerMaxEpoch.get(node) ?? 0;
        if (record.event_type === "owner_registered") {
          identifier(record.instance, "epoch event instance");
          state.ownerRegisteredCount += 1;
          if (record.epoch <= maxEpoch) {
            fail(
              `epoch event log ${entry.name} owner epochs must strictly increase per node`,
            );
          }
          state.ownerMaxEpoch.set(node, record.epoch);
          active.add(record.epoch);
          state.ownerRegistrations.push({ node, epoch: record.epoch, timestampMs: parsed });
        } else if (record.event_type === "owner_lease_expired") {
          if (!active.has(record.epoch)) {
            fail(
              `epoch event log ${entry.name} owner lease expiry must reference the active owner epoch`,
            );
          }
          active.delete(record.epoch);
          state.ownerExpired.add(`${node}:${record.epoch}`);
          state.ownerExpiries.push({ node, epoch: record.epoch, timestampMs: parsed });
        } else if (record.event_type === "owner_retired") {
          if (record.epoch > maxEpoch) {
            fail(
              `epoch event log ${entry.name} owner retirement must reference a registered epoch`,
            );
          }
          active.delete(record.epoch);
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
            (record.epoch < maxEpoch || state.ownerExpired.has(`${node}:${record.epoch}`))
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
          state.leaderAcquiredCount += 1;
          if (record.epoch <= state.leaderMaxEpoch) {
            fail(
              `epoch event log ${entry.name} scheduler epochs must strictly increase`,
            );
          }
          state.leaderMaxEpoch = record.epoch;
          state.leaderActive.add(record.epoch);
        } else if (record.event_type === "leader_lease_expired") {
          if (!state.leaderActive.has(record.epoch)) {
            fail(
              `epoch event log ${entry.name} scheduler lease expiry must reference the active leader epoch`,
            );
          }
          state.leaderActive.delete(record.epoch);
          state.leaderExpired.add(record.epoch);
          state.leaderExpiries.push({ epoch: record.epoch, timestampMs: parsed });
        } else if (record.event_type === "leader_commit") {
          identifier(record.instance, "epoch event instance");
          boolean(record.accepted, "epoch event accepted");
          state.leaderCommitCount += 1;
          if (record.epoch > state.leaderMaxEpoch) {
            fail(
              `epoch event log ${entry.name} scheduler commit references an unacquired epoch`,
            );
          }
          if (
            record.accepted &&
            (record.epoch < state.leaderMaxEpoch || state.leaderExpired.has(record.epoch))
          ) {
            state.staleSchedulerCommits += 1;
          }
          state.leaderCommits.push({ epoch: record.epoch, timestampMs: parsed, accepted: record.accepted });
        }
        state.maxConcurrentLeaders = Math.max(
          state.maxConcurrentLeaders,
          state.leaderActive.size,
        );
      }
    },
  );
  return state;
}

const commandOutcomes = new Set(["success", "failed", "unknown"]);

function parseCommandTrace(entry, binding) {
  const state = {
    dispatchBoundSeconds: null,
    commands: new Map(),
    effects: new Map(),
    effectIdSeen: new Set(),
    dispatchedCount: 0,
    resultCount: 0,
    unmatchedResultCount: 0,
    duplicateEffectCount: 0,
    inflight: 0,
    maxInflight: 0,
  };
  streamEventLines(
    entry,
    "command trace",
    (record) => {
      switch (record.record_type) {
        case "profile":
          return ["record_type", "dispatch_bound_seconds"];
        case "enqueued":
        case "dispatched":
          return ["record_type", "command_id"];
        case "effect":
          return ["record_type", "idempotency_key", "effect_id"];
        case "result":
          return ["record_type", "command_id", "outcome"];
        default:
          return null;
      }
    },
    binding,
    (record, parsed) => {
      const label = `command trace artifact ${entry.name}`;
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
        if (state.commands.has(record.command_id)) {
          fail(`${label} repeats command_id ${record.command_id}`);
        }
        state.commands.set(record.command_id, {
          enqueuedAtMs: parsed,
          dispatchedAtMs: null,
          firstResultAtMs: null,
        });
      } else if (record.record_type === "dispatched") {
        identifier(record.command_id, `${label} command_id`);
        const command = state.commands.get(record.command_id);
        if (!command) {
          fail(
            `${label} dispatch references unknown command ${record.command_id}`,
          );
        }
        state.dispatchedCount += 1;
        if (command.dispatchedAtMs === null) {
          command.dispatchedAtMs = parsed;
          state.inflight += 1;
          state.maxInflight = Math.max(state.maxInflight, state.inflight);
        }
      } else if (record.record_type === "effect") {
        identifier(record.idempotency_key, `${label} idempotency_key`);
        identifier(record.effect_id, `${label} effect_id`);
        if (state.effectIdSeen.has(record.effect_id)) {
          fail(`${label} repeats effect_id ${record.effect_id}`);
        }
        state.effectIdSeen.add(record.effect_id);
        const seen = state.effects.get(record.idempotency_key) ?? 0;
        if (seen > 0) state.duplicateEffectCount += 1;
        state.effects.set(record.idempotency_key, seen + 1);
      } else if (record.record_type === "result") {
        identifier(record.command_id, `${label} command_id`);
        if (!commandOutcomes.has(record.outcome)) {
          fail(`${label} result has an invalid outcome`);
        }
        state.resultCount += 1;
        const command = state.commands.get(record.command_id);
        if (!command || command.dispatchedAtMs === null) {
          state.unmatchedResultCount += 1;
          return;
        }
        if (command.firstResultAtMs === null) {
          command.firstResultAtMs = parsed;
          state.inflight -= 1;
        }
      }
    },
  );
  if (state.dispatchBoundSeconds === null) {
    fail(`command trace artifact ${entry.name} must declare a profile record`);
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
  requireWindow(snapshotMs, label, binding);
  if (!Array.isArray(doc.rows) || doc.rows.length === 0) {
    fail(`${label} must contain at least one row`);
  }
  const ids = new Set();
  for (const [index, row] of doc.rows.entries()) {
    const rowLabel = `${label} row ${index + 1}`;
    closed(row, ["command_id", "created_at", "due_at", "state"], rowLabel);
    identifier(row.command_id, `${rowLabel} command_id`);
    if (ids.has(row.command_id)) {
      fail(`${label} repeats command_id ${row.command_id}`);
    }
    ids.add(row.command_id);
    const createdMs = rfc3339(row.created_at, `${rowLabel} created_at`);
    requireWindow(createdMs, rowLabel, binding);
    const dueMs = rfc3339(row.due_at, `${rowLabel} due_at`);
    requireWindow(dueMs, rowLabel, binding);
    if (!outboxStates.has(row.state)) {
      fail(`${rowLabel} has an invalid state`);
    }
    if (createdMs > dueMs) {
      fail(`${rowLabel} must not be due before it was created`);
    }
  }
  return { snapshotMs, rows: doc.rows };
}

const httpSampleHeader = [
  "timestamp",
  "kind",
  "status",
  "latency_seconds",
  "environment_id",
  "candidate_sha",
];
const httpKinds = new Set(["read", "enqueue"]);
const httpStatuses = new Set(["ok", "error"]);

function parseHttpSamples(entry, binding) {
  const lines = splitArtifactLines(
    entry.bytes.toString("utf8"),
    entry.name,
    "http samples",
  );
  if (lines.length < 2) {
    fail(`http samples artifact ${entry.name} needs a header and samples`);
  }
  const header = lines[0].split(",");
  if (JSON.stringify(header) !== JSON.stringify(httpSampleHeader)) {
    fail(`http samples artifact ${entry.name} has an invalid header`);
  }
  const state = {
    reads: [],
    readSuccesses: 0,
    enqueues: [],
    enqueueSuccesses: 0,
    okEnqueueLatencies: [],
  };
  for (const [index, line] of lines.slice(1).entries()) {
    const columns = line.split(",");
    if (columns.length !== header.length) {
      fail(`http samples artifact ${entry.name} has a ragged row`);
    }
    const rowNumber = index + 2;
    const label = `http samples artifact ${entry.name} row ${rowNumber}`;
    const parsedTimestamp = rfc3339(columns[0], `${label} timestamp`);
    requireWindow(parsedTimestamp, label, binding);
    requireBinding(columns[4], columns[5], label, binding);
    if (!httpKinds.has(columns[1])) {
      fail(`${label} has an invalid kind`);
    }
    if (!httpStatuses.has(columns[2])) {
      fail(`${label} has an invalid status`);
    }
    const latency = csvFiniteNumber(columns[3], `${label} latency_seconds`);
    if (columns[1] === "read") {
      state.reads.push(latency);
      if (columns[2] === "ok") state.readSuccesses += 1;
    } else {
      state.enqueues.push(latency);
      if (columns[2] === "ok") {
        state.enqueueSuccesses += 1;
        state.okEnqueueLatencies.push(latency);
      }
    }
  }
  if (state.reads.length === 0) {
    fail(`http samples artifact ${entry.name} needs at least one read sample`);
  }
  if (state.enqueues.length === 0) {
    fail(`http samples artifact ${entry.name} needs at least one enqueue sample`);
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
  const snapshotMs = rfc3339(doc.snapshot_taken_at, `${label} snapshot_taken_at`);
  requireWindow(snapshotMs, label, binding);
  if (!Array.isArray(doc.agents) || doc.agents.length === 0) {
    fail(`${label} must contain at least one agent`);
  }
  const ids = new Set();
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
    requireWindow(telemetryMs, agentLabel, binding);
    if (telemetryMs > snapshotMs) {
      fail(`${agentLabel} must not be newer than the snapshot`);
    }
  }
  return { snapshotMs, boundSeconds: doc.freshness_bound_seconds, agents: doc.agents };
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
      ["write_id", "intent_recorded", "result_recorded"],
      writeLabel,
    );
    identifier(write.write_id, `${writeLabel} write_id`);
    if (ids.has(write.write_id)) {
      fail(`${label} repeats write_id ${write.write_id}`);
    }
    ids.add(write.write_id);
    boolean(write.intent_recorded, `${writeLabel} intent_recorded`);
    boolean(write.result_recorded, `${writeLabel} result_recorded`);
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
      "service_restored_at",
      "acknowledged",
      "failover",
      "recovery",
    ],
    label,
  );
  requireBinding(doc.environment_id, doc.candidate_sha, label, binding);
  const outageMs = rfc3339(doc.outage_declared_at, `${label} outage_declared_at`);
  requireWindow(outageMs, label, binding);
  const restoredServiceMs = rfc3339(
    doc.service_restored_at,
    `${label} service_restored_at`,
  );
  requireWindow(restoredServiceMs, label, binding);
  if (restoredServiceMs <= outageMs) {
    fail(`${label} must restore service after the declared outage`);
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
    requireWindow(ackMs, markerLabel, binding);
    if (ackMs > outageMs) {
      fail(`${markerLabel} must be acknowledged before the declared outage`);
    }
    acknowledged.push({ txid: marker.txid, acknowledgedAtMs: ackMs });
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
  requireWindow(isolatedMs, label, binding);
  const promotedMs = rfc3339(doc.failover.promoted_at, `${label} failover promoted_at`);
  requireWindow(promotedMs, label, binding);
  if (promotedMs < isolatedMs) {
    fail(`${label} must promote the new primary after isolating the old one`);
  }
  if (!Array.isArray(doc.failover.isolated_primary_writes) || doc.failover.isolated_primary_writes.length === 0) {
    fail(`${label} must probe the isolated former primary`);
  }
  let dualPrimaryWriteAccepts = 0;
  for (const [index, attempt] of doc.failover.isolated_primary_writes.entries()) {
    const attemptLabel = `${label} isolated primary write ${index + 1}`;
    closed(attempt, ["at", "accepted"], attemptLabel);
    const atMs = rfc3339(attempt.at, `${attemptLabel} at`);
    requireWindow(atMs, attemptLabel, binding);
    if (atMs < isolatedMs) {
      fail(`${attemptLabel} must be attempted after isolation`);
    }
    boolean(attempt.accepted, `${attemptLabel} accepted`);
    if (attempt.accepted) dualPrimaryWriteAccepts += 1;
  }
  object(doc.recovery, `${label} recovery`);
  closed(doc.recovery, ["restored_at", "present_txids"], `${label} recovery`);
  const restoredMs = rfc3339(doc.recovery.restored_at, `${label} recovery restored_at`);
  requireWindow(restoredMs, label, binding);
  if (restoredMs < outageMs) {
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
  const newestPresentMs = Math.max(
    ...presentMarkers.map((marker) => marker.acknowledgedAtMs),
  );
  const acknowledgedTransactionLoss = acknowledged.filter(
    (marker) => !present.has(marker.txid),
  ).length;
  return {
    rtoSeconds: (restoredServiceMs - outageMs) / 1000,
    rpoSeconds: (outageMs - newestPresentMs) / 1000,
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
    const writtenMs = rfc3339(marker.written_at, `${markerLabel} written_at`);
    requireWindow(writtenMs, markerLabel, binding);
    return writtenMs;
  };
  const markerAMs = markerFields(doc.marker_a, `${label} marker_a`);
  const restorePointMs = rfc3339(
    doc.restore_point_created_at,
    `${label} restore_point_created_at`,
  );
  requireWindow(restorePointMs, label, binding);
  const markerBMs = markerFields(doc.marker_b, `${label} marker_b`);
  object(doc.restore, `${label} restore`);
  closed(
    doc.restore,
    ["restored_at", "marker_a_present", "marker_b_present"],
    `${label} restore`,
  );
  const restoredMs = rfc3339(doc.restore.restored_at, `${label} restore restored_at`);
  requireWindow(restoredMs, label, binding);
  if (!(markerAMs < restorePointMs && restorePointMs < markerBMs)) {
    fail(
      `${label} marker order must be marker_a < restore point < marker_b`,
    );
  }
  if (restoredMs < markerBMs) {
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
      "reconnect_storm",
    ],
    label,
  );
  requireBinding(doc.environment_id, doc.candidate_sha, label, binding);
  const snapshotMs = rfc3339(doc.snapshot_taken_at, `${label} snapshot_taken_at`);
  requireWindow(snapshotMs, label, binding);
  if (!Array.isArray(doc.sessions) || doc.sessions.length === 0) {
    fail(`${label} must contain at least one session`);
  }
  const ids = new Set();
  for (const [index, session] of doc.sessions.entries()) {
    const sessionLabel = `${label} session ${index + 1}`;
    closed(
      session,
      ["agent_id", "node", "authorized", "connected", "session_started_at"],
      sessionLabel,
    );
    identifier(session.agent_id, `${sessionLabel} agent_id`);
    identifier(session.node, `${sessionLabel} node`);
    if (ids.has(session.agent_id)) {
      fail(`${label} repeats agent_id ${session.agent_id}`);
    }
    ids.add(session.agent_id);
    boolean(session.authorized, `${sessionLabel} authorized`);
    boolean(session.connected, `${sessionLabel} connected`);
    const startedMs = rfc3339(
      session.session_started_at,
      `${sessionLabel} session_started_at`,
    );
    requireWindow(startedMs, sessionLabel, binding);
    if (startedMs > snapshotMs) {
      fail(`${sessionLabel} must start before the snapshot`);
    }
  }
  object(doc.reconnect_storm, `${label} reconnect storm`);
  closed(
    doc.reconnect_storm,
    ["bulk_disconnect_at", "reconnect_completed_at"],
    `${label} reconnect storm`,
  );
  const disconnectMs = rfc3339(
    doc.reconnect_storm.bulk_disconnect_at,
    `${label} reconnect storm bulk_disconnect_at`,
  );
  requireWindow(disconnectMs, label, binding);
  const completedMs = rfc3339(
    doc.reconnect_storm.reconnect_completed_at,
    `${label} reconnect storm reconnect_completed_at`,
  );
  requireWindow(completedMs, label, binding);
  if (completedMs < disconnectMs) {
    fail(`${label} reconnect storm must complete after the bulk disconnect`);
  }
  return {
    agentIds: ids,
    sessions: doc.sessions,
    stormRecoverySeconds: (completedMs - disconnectMs) / 1000,
  };
}

const relayNames = new Set(["relay-a", "relay-b"]);
const pathNames = new Set(["direct", "relay"]);

function parseRelayTransitions(entry, binding) {
  const state = { failures: [], activations: [], pairCount: 0 };
  streamEventLines(
    entry,
    "relay transition log",
    (record) => {
      switch (record.event_type) {
        case "relay_failed":
        case "relay_active":
          return ["event_type", "relay"];
        case "path_active":
        case "path_failed":
          return ["event_type", "session_id", "path"];
        default:
          return null;
      }
    },
    binding,
    (record, parsed) => {
      const label = `relay transition artifact ${entry.name}`;
      if (record.event_type === "relay_failed" || record.event_type === "relay_active") {
        if (!relayNames.has(record.relay)) {
          fail(`${label} has an invalid relay name`);
        }
        (record.event_type === "relay_failed" ? state.failures : state.activations).push({
          relay: record.relay,
          timestampMs: parsed,
        });
      } else {
        identifier(record.session_id, `${label} session_id`);
        if (!pathNames.has(record.path)) {
          fail(`${label} has an invalid path name`);
        }
      }
    },
  );
  let worstTakeoverMs = null;
  for (const failure of state.failures) {
    const successor = state.activations.find(
      (activation) =>
        activation.relay !== failure.relay &&
        activation.timestampMs >= failure.timestampMs,
    );
    if (!successor) continue;
    state.pairCount += 1;
    const delta = successor.timestampMs - failure.timestampMs;
    if (worstTakeoverMs === null || delta > worstTakeoverMs) {
      worstTakeoverMs = delta;
    }
  }
  if (worstTakeoverMs === null) {
    fail(
      `relay transition artifact ${entry.name} must record a completed relay takeover`,
    );
  }
  return { takeoverSeconds: worstTakeoverMs / 1000, pairCount: state.pairCount };
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
    const value = ratio ? (end - baseline) / baseline : end - baseline;
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
function ownerTakeover(state) {
  const pairs = [];
  for (const expiry of state.ownerExpiries) {
    const successor = state.ownerRegistrations.find(
      (registration) =>
        registration.node === expiry.node &&
        registration.epoch > expiry.epoch &&
        registration.timestampMs >= expiry.timestampMs,
    );
    if (!successor) continue;
    pairs.push(successor.timestampMs - expiry.timestampMs);
  }
  if (pairs.length === 0) {
    fail("epoch event log must record a completed connection-owner takeover");
  }
  return {
    value: Math.max(...pairs) / 1000,
    sampleCount: pairs.length,
  };
}

function schedulerTakeover(state) {
  const pairs = [];
  for (const expiry of state.leaderExpiries) {
    const successor = state.leaderCommits.find(
      (commit) =>
        commit.epoch > expiry.epoch &&
        commit.accepted &&
        commit.timestampMs >= expiry.timestampMs,
    );
    if (!successor) continue;
    pairs.push(successor.timestampMs - expiry.timestampMs);
  }
  if (pairs.length === 0) {
    fail("epoch event log must record a completed scheduler takeover");
  }
  return {
    value: Math.max(...pairs) / 1000,
    sampleCount: pairs.length,
  };
}

function commandCompletionP99(state) {
  const completions = [];
  for (const command of state.commands.values()) {
    if (command.firstResultAtMs !== null) {
      completions.push((command.firstResultAtMs - command.enqueuedAtMs) / 1000);
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

function dispatchRatio(state) {
  if (state.dispatchedCount === 0) {
    fail("command trace must record at least one dispatched command");
  }
  let withinBound = 0;
  for (const command of state.commands.values()) {
    if (command.dispatchedAtMs === null) continue;
    if (
      (command.dispatchedAtMs - command.enqueuedAtMs) / 1000 <=
      state.dispatchBoundSeconds
    ) {
      withinBound += 1;
    }
  }
  return {
    value: withinBound / state.dispatchedCount,
    sampleCount: state.dispatchedCount,
  };
}

function outboxMetrics(snapshot) {
  const total = snapshot.rows.length;
  let pendingDue = 0;
  let unknown = 0;
  let terminalOrReconciled = 0;
  let oldestDueAgeSeconds = 0;
  for (const row of snapshot.rows) {
    if (row.state === "pending" && Date.parse(row.due_at) <= snapshot.snapshotMs) {
      pendingDue += 1;
      const age = (snapshot.snapshotMs - Date.parse(row.created_at)) / 1000;
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
  const boundMs = state.boundSeconds * 1000;
  let fresh = 0;
  for (const agent of state.agents) {
    if (state.snapshotMs - Date.parse(agent.last_telemetry_at) <= boundMs) {
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
      sampleCount: parsed.rows.length,
    }),
  }],
  ["resource_samples.max_sample_gap_seconds", {
    kind: "resource_samples",
    compute: (parsed) => ({
      value: parsed.maxSampleGapSeconds,
      sampleCount: parsed.rows.length,
    }),
  }],
  ["resource_samples.valid_sample_count", {
    kind: "resource_samples",
    compute: (parsed) => ({ value: parsed.rows.length, sampleCount: parsed.rows.length }),
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
    compute: (parsed) => ({
      value: parsed.maxInflight,
      sampleCount: parsed.dispatchedCount,
    }),
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
      sampleCount: parsed.dispatchedCount,
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
      sampleCount: 1,
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

// Parses every structured artifact exactly once with the evidence binding
// context and cross-checks artifacts against each other.
function parseAllStructuredArtifacts(standardArtifacts, verifiedArtifacts, evidence) {
  const binding = {
    environmentId: evidence.environment.environment_id,
    candidateSha: evidence.candidate_sha,
    startedAtMs: Date.parse(evidence.started_at),
    finishedAtMs: Date.parse(evidence.finished_at),
  };
  const parsed = new Map();
  for (const [kind, artifact] of standardArtifacts) {
    const entry = verifiedArtifacts.get(artifact.name);
    parsed.set(kind, parseStructuredArtifact(kind, entry, binding));
  }
  const telemetry = parsed.get("telemetry_snapshot");
  const sessions = parsed.get("agent_sessions");
  for (const agent of telemetry.agents) {
    if (!sessions.agentIds.has(agent.agent_id)) {
      fail(
        `telemetry snapshot references agent ${agent.agent_id} absent from the session inventory`,
      );
    }
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
