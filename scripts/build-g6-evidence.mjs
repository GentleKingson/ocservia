#!/usr/bin/env node

// Assembles the formal G6 readiness evidence bundle from the frozen run
// state of both failure domains. The builder is a pure transcriber: every
// structured artifact is derived from a trusted producer (the authoritative
// database tables, the per-agent durable journals, the live transportd
// session inventory, the fenced-probe outputs, and the runner clocks), and
// every measurement written into evidence.json is recomputed from the
// artifact bytes by the shared verifier library itself, so the bundle can
// never drift from the derivation contract.
//
//   node scripts/build-g6-evidence.mjs \
//     --run-dir <harness work dir> --peer-dir <downloaded peer evidence> \
//     --out-dir <bundle dir> --slo docs/acceptance/g6-slo.yaml \
//     --environment-id g6-... --candidate-sha <sha> \
//     --authority engineering|production_readiness \
//     --failure-domain-class multi_host --run-id <id>

import {
  copyFileSync,
  mkdirSync,
  readdirSync,
  readFileSync,
  writeFileSync,
} from "node:fs";
import { join, relative, resolve, sep } from "node:path";
import {
  computeG6Derivations,
  parseSlo,
  sha256Digest,
} from "./g6-contract-lib.mjs";

const usage =
  "usage: build-g6-evidence.mjs --run-dir DIR --peer-dir DIR --out-dir DIR --slo FILE --environment-id ID --candidate-sha SHA --authority AUTHORITY --failure-domain-class CLASS --run-id ID";

const values = {};
for (let index = 2; index < process.argv.length; index += 2) {
  const option = process.argv[index];
  const value = process.argv[index + 1];
  if (!option?.startsWith("--") || !value) throw new Error(usage);
  values[option.slice(2)] = value;
}
for (const name of [
  "run-dir",
  "peer-dir",
  "out-dir",
  "slo",
  "environment-id",
  "candidate-sha",
  "authority",
  "failure-domain-class",
  "run-id",
]) {
  if (!values[name]) throw new Error(usage);
}

const runDir = values["run-dir"];
const peerDir = values["peer-dir"];
const outDir = values["out-dir"];
const environmentId = values["environment-id"];
const candidateSha = values["candidate-sha"];
const authority = values["authority"];
const failureDomainClass = values["failure-domain-class"];
const runId = values["run-id"];

mkdirSync(outDir, { recursive: true });

const sourceInventory = new Map();
const sourceRoots = [
  ["run", resolve(runDir)],
  ["peer", resolve(peerDir)],
];

function sourceInventoryName(path) {
  const resolvedPath = resolve(path);
  if (resolvedPath === resolve(values.slo)) return "contract/g6-slo.yaml";
  for (const [label, root] of sourceRoots) {
    if (resolvedPath === root || resolvedPath.startsWith(`${root}${sep}`)) {
      return `${label}/${relative(root, resolvedPath).split(sep).join("/")}`;
    }
  }
  return undefined;
}

function recordSource(path, content) {
  const name = sourceInventoryName(path);
  if (!name) return;
  sourceInventory.set(name, {
    path: name,
    bytes: Buffer.byteLength(content),
    digest: sha256Digest(content),
  });
  writeFileSync(
    join(outDir, "builder-source-inventory.json"),
    `${JSON.stringify(
      {
        schema_version: "ocservia.g6-builder-source-inventory.v1",
        sources: [...sourceInventory.values()].sort((left, right) =>
          left.path < right.path ? -1 : left.path > right.path ? 1 : 0,
        ),
      },
      null,
      2,
    )}\n`,
  );
}

if (authority !== "engineering" && authority !== "production_readiness") {
  throw new Error("authority must be engineering or production_readiness");
}

function fail(message, details = {}) {
  writeFileSync(
    join(outDir, "builder-error.json"),
    `${JSON.stringify(
      {
        schema_version: "ocservia.g6-evidence-build-error.v1",
        phase: "build",
        status: "failed",
        reason: message,
        ...details,
      },
      null,
      2,
    )}\n`,
  );
  throw new Error(message);
}

function readText(...parts) {
  const path = join(...parts);
  const content = readFileSync(path, "utf8");
  recordSource(path, content);
  return content;
}

function requireFile(...parts) {
  const path = join(...parts);
  try {
    const content = readFileSync(path, "utf8");
    if (content.length > 0) {
      recordSource(path, content);
      return path;
    }
  } catch {
    // fall through to the shared failure below
  }
  return fail(`required input is missing or empty: ${path}`);
}

function readJSONL(...parts) {
  return readText(...parts)
    .split("\n")
    .filter((line) => line.length > 0)
    .map((line, index) => {
      try {
        return JSON.parse(line);
      } catch {
        return fail(`${join(...parts)} line ${index + 1} is not valid JSON`);
      }
    });
}

function isoOf(value) {
  return `${new Date(value).toISOString().slice(0, 19)}Z`;
}

function preciseIsoOfMicros(value) {
  const micros = value % 1000000n;
  const secondsMs = Number((value - micros) / 1000n);
  return `${new Date(secondsMs).toISOString().slice(0, 19)}.${micros
    .toString()
    .padStart(6, "0")}Z`;
}

function preciseIsoOfNanoseconds(value) {
  const nanos = value % 1000000000n;
  const secondsMs = Number((value - nanos) / 1000000n);
  return `${new Date(secondsMs).toISOString().slice(0, 19)}.${nanos
    .toString()
    .padStart(9, "0")}Z`;
}

function parseStamp(value, label) {
  const parsed = Date.parse(value);
  if (!Number.isFinite(parsed)) fail(`${label} is not a timestamp: ${value}`);
  return parsed;
}

function compareRfc3339(left, right, leftLabel, rightLabel) {
  const pattern =
    /^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(?:\.(\d+))?(Z|[+-]\d{2}:\d{2})$/;
  const parts = (value, label) => {
    const match = pattern.exec(value);
    if (!match) fail(`${label} is not an RFC 3339 timestamp: ${value}`);
    parseStamp(value, label);
    return {
      wholeSecondMs: Date.parse(`${match[1]}${match[3]}`),
      fraction: match[2] ?? "",
    };
  };
  const leftParts = parts(left, leftLabel);
  const rightParts = parts(right, rightLabel);
  if (leftParts.wholeSecondMs !== rightParts.wholeSecondMs) {
    return Math.sign(leftParts.wholeSecondMs - rightParts.wholeSecondMs);
  }
  const width = Math.max(
    leftParts.fraction.length,
    rightParts.fraction.length,
  );
  const leftFraction = leftParts.fraction.padEnd(width, "0");
  const rightFraction = rightParts.fraction.padEnd(width, "0");
  return leftFraction < rightFraction ? -1 : leftFraction > rightFraction ? 1 : 0;
}

// Docker's RFC 3339 timestamps may carry nanosecond precision; normalize to
// whole seconds so every emitted stamp matches the strict artifact pattern.
function normalizeStamp(value, label) {
  const match = /^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(?:\.\d+)?Z$/.exec(
    value,
  );
  if (!match) fail(`${label} is not an RFC 3339 UTC timestamp: ${value}`);
  return `${match[1]}Z`;
}

function normalizePreciseStamp(value, label) {
  const match =
    /^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(\.\d+)?Z$/.exec(value);
  if (!match) fail(`${label} is not an RFC 3339 UTC timestamp: ${value}`);
  return `${match[1]}${match[2] ?? ""}Z`;
}

function utcStampMicros(value, label) {
  const match =
    /^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(?:\.(\d{1,6}))?Z$/.exec(
      value,
    );
  if (!match) fail(`${label} is not a microsecond RFC 3339 UTC timestamp`);
  const seconds = Date.parse(`${match[1]}Z`);
  if (!Number.isFinite(seconds)) fail(`${label} is not a timestamp`);
  return (
    BigInt(seconds) * 1000n +
    BigInt((match[2] ?? "").padEnd(6, "0"))
  );
}

function utcStampNanoseconds(value, label) {
  const match =
    /^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(?:\.(\d{1,9}))?Z$/.exec(
      value,
    );
  if (!match) fail(`${label} is not a nanosecond RFC 3339 UTC timestamp`);
  const seconds = Date.parse(`${match[1]}Z`);
  if (!Number.isFinite(seconds)) fail(`${label} is not a timestamp`);
  return (
    BigInt(seconds) * 1000000n +
    BigInt((match[2] ?? "").padEnd(9, "0"))
  );
}

function normalizeUUIDIdentity(value, label) {
  if (/^[0-9a-f]{32}$/.test(value ?? "")) return value;
  if (
    /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/.test(
      value ?? "",
    )
  ) {
    return value.replaceAll("-", "");
  }
  return fail(`${label} is not a UUID identity`);
}

// Journal effect keys are public run-scoped correlation identifiers, but their
// bare 32-hex form trips the generic secret heuristic. Publishing them under
// an explicit public tag keeps every builder/verifier comparison intact while
// letting the pinned scan exempt exactly the tagged shape.
function journalKeyIdentifier(keyHex) {
  if (!/^[0-9a-f]{32}$/.test(keyHex ?? "")) {
    fail(`journal effect key is not a 32-hex identity: ${keyHex}`);
  }
  return `g6-journal-key-${keyHex}`;
}

function normalizeEndpointIdentity(value, label) {
  if (/^[0-9a-f]{64}$/.test(value ?? "")) return value;
  return fail(
    `${label} is not a 64-character lowercase hexadecimal endpoint id`,
  );
}

function positiveDecimalString(value, label) {
  if (/^[1-9][0-9]*$/.test(value ?? "")) return value;
  return fail(`${label} is not a canonical positive decimal string`);
}

function frozenHistoryLines(value, label) {
  if (
    !Array.isArray(value) ||
    value.length === 0 ||
    value.some(
      (line) =>
        typeof line !== "string" || line.length === 0 || /[\r\n]/.test(line),
    )
  ) {
    fail(`${label} must be a non-empty array of non-empty single-line strings`);
  }
  return value;
}

function publishedHistoryLines(content, label) {
  const body = content.endsWith("\n") ? content.slice(0, -1) : content;
  const lines = body.split("\n");
  if (lines.length === 0 || lines.some((line) => line.length === 0)) {
    fail(`${label} must contain non-empty lines`);
  }
  return lines;
}

function requireFrozenHistoryMatch(frozen, published, label) {
  if (
    frozen.length !== published.length ||
    frozen.some((line, index) => line !== published[index])
  ) {
    fail(`${label} does not match the frozen final authority cut`);
  }
}

function canonicalJson(value) {
  return `${JSON.stringify(value, null, 2)}\n`;
}

function jsonl(records) {
  return `${records.map((record) => JSON.stringify(record)).join("\n")}\n`;
}

function csvCell(value, label) {
  const text = String(value);
  if (/[\r\n]/.test(text)) {
    fail(`${label} must be a single-line CSV value`);
  }
  return /[",]/.test(text) ? `"${text.replaceAll('"', '""')}"` : text;
}

function csvRow(values, label) {
  return values.map((value) => csvCell(value, label)).join(",");
}

// ---------------------------------------------------------------------------
// Inputs.
// ---------------------------------------------------------------------------

requireFile(values.slo);
requireFile(runDir, "state", "read-log.jsonl");
requireFile(runDir, "state", "enqueue-log.jsonl");
requireFile(runDir, "state", "resource-samples.csv");
requireFile(runDir, "state", "era2-sessions.tsv");
requireFile(runDir, "state", "reconnect-sessions.json");
requireFile(runDir, "state", "owner-b-terms.tsv");
requireFile(runDir, "state", "owner-replacement-sessions.json");
requireFile(runDir, "state", "scheduler-replacement-term");
requireFile(runDir, "state", "scheduler-maintenance-observation.json");
requireFile(runDir, "state", "relay-b-node-id");
requireFile(runDir, "state", "relay-b-before-command.json");
requireFile(runDir, "state", "relay-b-observation.json");
requireFile(runDir, "state", "relay-b-active-at");
requireFile(runDir, "state", "relay-b-started.json");
requireFile(runDir, "state", "relay-command-proof.json");
requireFile(runDir, "state", "relay-dispatch-proof.json");
requireFile(runDir, "outbox", "relay-pre-fault", "relay-a-before-command.json");
requireFile(runDir, "outbox", "relay-pre-fault", "relay-a-observation.json");
requireFile(runDir, "outbox", "relay-pre-fault", "relay-a-command-proof.json");
requireFile(runDir, "outbox", "relay-pre-fault", "relay-a-dispatch-proof.json");
requireFile(runDir, "outbox", "relay-pre-fault", "observed-at");
requireFile(runDir, "outbox", "relay-pre-fault", "node-id");
requireFile(
  runDir,
  "outbox",
  "relay-pre-fault",
  "relay-a-only-readiness.json",
);
requireFile(runDir, "outbox", "relay-pre-fault", "relay-b-disabled.json");
requireFile(runDir, "state", "all-nodes.tsv");
requireFile(runDir, "state", "promoted-at");
requireFile(runDir, "state", "window-ended-at");
requireFile(runDir, "state", "evidence", "final-sessions.json");
requireFile(runDir, "state", "evidence", "final-sessions-before.json");
requireFile(runDir, "state", "evidence", "final-sessions-after.json");
requireFile(runDir, "state", "evidence", "final-sessions-before-complete-at");
requireFile(runDir, "state", "evidence", "final-sessions-after-start-at");
requireFile(runDir, "state", "final-authority-cut.json");
requireFile(runDir, "state", "evidence", "snapshot-taken-at");
requireFile(runDir, "state", "window-opening-active.json");
requireFile(runDir, "state", "evidence", "commands.jsonl");
requireFile(runDir, "state", "evidence", "attempts.jsonl");
requireFile(runDir, "state", "evidence", "outbox.jsonl");
requireFile(runDir, "state", "evidence", "audit.jsonl");
requireFile(runDir, "state", "evidence", "telemetry.jsonl");
requireFile(runDir, "state", "evidence", "markers.jsonl");
requireFile(runDir, "state", "evidence", "instances.tsv");
requireFile(runDir, "outbox", "timeline.jsonl");
requireFile(runDir, "outbox", "fencing-history.jsonl");
requireFile(runDir, "outbox", "leadership-history.jsonl");
requireFile(runDir, "outbox", "scheduler-maintenance-history.jsonl");
requireFile(peerDir, "isolation", "isolation.json");
requireFile(peerDir, "isolation", "active-load.json");
requireFile(peerDir, "isolation", "outage-declared-at");
requireFile(peerDir, "isolation", "isolated-at");
requireFile(peerDir, "isolation", "rto-started-at");
requireFile(peerDir, "isolation", "isolated-primary-writes.jsonl");
requireFile(peerDir, "pitr", "pitr-report.json");
requireFile(peerDir, "evidence", "instances.tsv");
requireFile(peerDir, "relay-a-failed-at");
requireFile(peerDir, "relay-fault-cut.json");
requireFile(peerDir, "rejoin-at");
requireFile(peerDir, "post-rejoin-probes.jsonl");
requireFile(peerDir, "final-freeze-at");
requireFile(peerDir, "freeze-received-at");
requireFile(peerDir, "evidence", "snapshot-taken-at");

const readLog = readJSONL(runDir, "state", "read-log.jsonl");
const enqueueLog = readJSONL(runDir, "state", "enqueue-log.jsonl");
const commands = readJSONL(runDir, "state", "evidence", "commands.jsonl");
const attempts = readJSONL(runDir, "state", "evidence", "attempts.jsonl");
const outboxRows = readJSONL(runDir, "state", "evidence", "outbox.jsonl");
const auditRows = readJSONL(runDir, "state", "evidence", "audit.jsonl");
const telemetryRows = readJSONL(runDir, "state", "evidence", "telemetry.jsonl");
const markerRows = readJSONL(runDir, "state", "evidence", "markers.jsonl");
const windowOpeningActive = JSON.parse(
  readText(runDir, "state", "window-opening-active.json"),
);
const nodes = readText(runDir, "state", "all-nodes.tsv")
  .split("\n")
  .filter((line) => line.length > 0)
  .map((line) => line.split("\t"))
  .map(([name, nodeId, endpoint]) => ({ name, nodeId, endpoint }));
const era2Sessions = new Map(
  readText(runDir, "state", "era2-sessions.tsv")
    .split("\n")
    .filter((line) => line.length > 0)
    .map((line) => {
      const [nodeId, connectedAt] = line.split("\t");
      return [nodeId, connectedAt];
    }),
);
const reconnectSessions = JSON.parse(
  readText(runDir, "state", "reconnect-sessions.json"),
);
const ownerBTermsText = readText(runDir, "state", "owner-b-terms.tsv");
const ownerReplacementSessions = JSON.parse(
  readText(runDir, "state", "owner-replacement-sessions.json"),
);
const schedulerReplacementTermText = readText(
  runDir,
  "state",
  "scheduler-replacement-term",
).trim();
const schedulerReplacementTermParts = schedulerReplacementTermText.split(":");
if (schedulerReplacementTermParts.length !== 3) {
  fail("scheduler replacement term is malformed");
}
const [
  schedulerReplacementInstance,
  schedulerReplacementIncarnationText,
  schedulerReplacementEpochText,
] = schedulerReplacementTermParts;
if (!/^[a-z0-9][a-z0-9._-]{0,127}$/.test(schedulerReplacementInstance)) {
  fail("scheduler replacement instance has an invalid identifier");
}
const schedulerReplacementIncarnation = positiveDecimalString(
  schedulerReplacementIncarnationText,
  "scheduler replacement incarnation",
);
const schedulerReplacementEpoch = Number(schedulerReplacementEpochText);
if (!Number.isInteger(schedulerReplacementEpoch) || schedulerReplacementEpoch < 1) {
  fail("scheduler replacement epoch must be positive");
}
let schedulerMaintenanceObservation;
try {
  schedulerMaintenanceObservation = JSON.parse(
    readText(runDir, "state", "scheduler-maintenance-observation.json"),
  );
} catch {
  fail("scheduler maintenance observation is not valid JSON");
}
const schedulerObservationFields = [
  "committed_observed_at",
  "epoch",
  "incarnation",
  "instance_id",
  "maintenance_id",
  "marker_completed_at",
];
if (
  !schedulerMaintenanceObservation ||
  typeof schedulerMaintenanceObservation !== "object" ||
  Array.isArray(schedulerMaintenanceObservation) ||
  JSON.stringify(Object.keys(schedulerMaintenanceObservation).sort()) !==
    JSON.stringify(schedulerObservationFields)
) {
  fail("scheduler maintenance observation fields do not match the closed shape");
}
const observedSchedulerMaintenanceId = positiveDecimalString(
  schedulerMaintenanceObservation.maintenance_id,
  "observed scheduler maintenance id",
);
const observedSchedulerIncarnation = positiveDecimalString(
  schedulerMaintenanceObservation.incarnation,
  "observed scheduler maintenance incarnation",
);
if (
  schedulerMaintenanceObservation.instance_id !== schedulerReplacementInstance ||
  observedSchedulerIncarnation !== schedulerReplacementIncarnation ||
  schedulerMaintenanceObservation.epoch !== schedulerReplacementEpoch
) {
  fail("scheduler maintenance observation does not match the replacement exact term");
}
const observedSchedulerMarkerAt = normalizePreciseStamp(
  schedulerMaintenanceObservation.marker_completed_at,
  "observed scheduler marker completed_at",
);
const schedulerMaintenanceCommittedObservedAt = normalizePreciseStamp(
  schedulerMaintenanceObservation.committed_observed_at,
  "scheduler maintenance committed_observed_at",
);
const observedSchedulerMarkerAtMicros = utcStampMicros(
  observedSchedulerMarkerAt,
  "observed scheduler marker completed_at",
);
const schedulerMaintenanceCommittedObservedAtMicros = utcStampMicros(
  schedulerMaintenanceCommittedObservedAt,
  "scheduler maintenance committed_observed_at",
);
if (
  observedSchedulerMarkerAtMicros >
  schedulerMaintenanceCommittedObservedAtMicros
) {
  fail("scheduler maintenance marker completed after it was observed committed");
}
const relayBNodeId = normalizeUUIDIdentity(
  readText(runDir, "state", "relay-b-node-id").trim(),
  "relay-b-node-id",
);
const relayBProbe = JSON.parse(
  readText(runDir, "state", "relay-b-observation.json"),
);
const relayBBeforeProbe = JSON.parse(
  readText(runDir, "state", "relay-b-before-command.json"),
);
const relayPreFaultProbe = JSON.parse(
  readText(runDir, "outbox", "relay-pre-fault", "relay-a-observation.json"),
);
const relayPreFaultBeforeProbe = JSON.parse(
  readText(runDir, "outbox", "relay-pre-fault", "relay-a-before-command.json"),
);
const relayPreFaultCommandProof = JSON.parse(
  readText(runDir, "outbox", "relay-pre-fault", "relay-a-command-proof.json"),
);
const relayPreFaultDispatchProof = JSON.parse(
  readText(runDir, "outbox", "relay-pre-fault", "relay-a-dispatch-proof.json"),
);
const relayPreFaultObservedAt = normalizePreciseStamp(
  readText(runDir, "outbox", "relay-pre-fault", "observed-at").trim(),
  "pre-fault relay-a observation boundary",
);
const relayPreFaultNodeId = normalizeUUIDIdentity(
  readText(runDir, "outbox", "relay-pre-fault", "node-id").trim(),
  "pre-fault relay-a observation node",
);
const relayTopologyReadiness = JSON.parse(
  readText(
    runDir,
    "outbox",
    "relay-pre-fault",
    "relay-a-only-readiness.json",
  ),
);
const relayBDisabled = JSON.parse(
  readText(runDir, "outbox", "relay-pre-fault", "relay-b-disabled.json"),
);
const relayBStarted = JSON.parse(
  readText(runDir, "state", "relay-b-started.json"),
);
const relayBActiveAtSource = normalizePreciseStamp(
  readText(runDir, "state", "relay-b-active-at").trim(),
  "relay-b active database boundary",
);
const relayCommandProof = JSON.parse(
  readText(runDir, "state", "relay-command-proof.json"),
);
const relayDispatchProof = JSON.parse(
  readText(runDir, "state", "relay-dispatch-proof.json"),
);
const beforeFinalSessions = JSON.parse(
  readText(runDir, "state", "evidence", "final-sessions-before.json"),
);
const afterFinalSessions = JSON.parse(
  readText(runDir, "state", "evidence", "final-sessions-after.json"),
);
const finalSessions = afterFinalSessions;
const finalAuthorityCut = JSON.parse(
  readText(runDir, "state", "final-authority-cut.json"),
);
const frozenOwnerHistory = frozenHistoryLines(
  finalAuthorityCut?.owner_history,
  "final authority cut owner_history",
);
const frozenSchedulerHistory = frozenHistoryLines(
  finalAuthorityCut?.scheduler_history,
  "final authority cut scheduler_history",
);
const frozenSchedulerMaintenanceHistory = frozenHistoryLines(
  finalAuthorityCut?.scheduler_maintenance_history,
  "final authority cut scheduler_maintenance_history",
);
const beforeFinalSessionsCompleteAt = normalizePreciseStamp(
  readText(
    runDir,
    "state",
    "evidence",
    "final-sessions-before-complete-at",
  ).trim(),
  "final-sessions-before-complete-at",
);
const afterFinalSessionsStartAt = normalizePreciseStamp(
  readText(
    runDir,
    "state",
    "evidence",
    "final-sessions-after-start-at",
  ).trim(),
  "final-sessions-after-start-at",
);
const snapshotTakenAt = normalizePreciseStamp(
  readText(runDir, "state", "evidence", "snapshot-taken-at").trim(),
  "snapshot-taken-at",
);
const authorityCutAt = normalizePreciseStamp(
  finalAuthorityCut.cut_at,
  "final authority cut_at",
);
if (
  schedulerMaintenanceCommittedObservedAtMicros >
  utcStampMicros(authorityCutAt, "authority cut")
) {
  fail("scheduler maintenance completion was first observed after the final authority cut");
}
if (authorityCutAt !== snapshotTakenAt) {
  fail("the final authority cut does not match snapshot-taken-at");
}
const promotedAt = normalizePreciseStamp(
  readText(runDir, "state", "promoted-at").trim(),
  "promoted-at",
);
const rtoStartedAt = normalizePreciseStamp(
  readText(peerDir, "isolation", "rto-started-at").trim(),
  "database RTO start",
);
const windowEndedAt = normalizePreciseStamp(
  readText(runDir, "state", "window-ended-at").trim(),
  "window-ended-at",
);
const finalFreezeAt = normalizePreciseStamp(
  readText(peerDir, "final-freeze-at").trim(),
  "final-freeze-at",
);
const freezeReceivedAt = normalizePreciseStamp(
  readText(peerDir, "freeze-received-at").trim(),
  "freeze-received-at",
);
const peerSnapshotTakenAt = normalizePreciseStamp(
  readText(peerDir, "evidence", "snapshot-taken-at").trim(),
  "fd-a snapshot-taken-at",
);
if (
  compareRfc3339(
    finalFreezeAt,
    windowEndedAt,
    "final freeze",
    "window end",
  ) < 0 ||
  compareRfc3339(
    peerSnapshotTakenAt,
    freezeReceivedAt,
    "fd-a snapshot",
    "fd-a freeze receipt",
  ) < 0
) {
  fail("fd-a final evidence was not frozen after the bounded window");
}
const pitrReport = JSON.parse(readText(peerDir, "pitr", "pitr-report.json"));
const isolation = JSON.parse(readText(peerDir, "isolation", "isolation.json"));
const isolatedWrites = readJSONL(
  peerDir,
  "isolation",
  "isolated-primary-writes.jsonl",
);
const activeLoad = JSON.parse(
  readText(peerDir, "isolation", "active-load.json"),
);
if (
  !Array.isArray(activeLoad.commands) ||
  activeLoad.commands.length < 50 ||
  !Number.isInteger(activeLoad.queued_outbox_count) ||
  activeLoad.queued_outbox_count < 50
) {
  fail("the database failure boundary has fewer than fifty active commands");
}
const rejoinBoundaryAt = normalizePreciseStamp(
  readText(peerDir, "rejoin-at").trim(),
  "rejoin-at",
);
const rejoinProbes = readJSONL(peerDir, "post-rejoin-probes.jsonl");
if (
  rejoinProbes.length < 3 ||
  rejoinProbes.some(
    (probe) =>
      probe.accepted !== false ||
      compareRfc3339(
        probe.at,
        rejoinBoundaryAt,
        "post-rejoin probe",
        "rejoin",
      ) < 0,
  )
) {
  fail("the rejoined former primary was not proven read-only after rejoin");
}
const activeLoadIds = new Set();
for (const command of activeLoad.commands) {
  if (
    typeof command.command_id !== "string" ||
    activeLoadIds.has(command.command_id) ||
    !["dispatched", "accepted", "running"].includes(command.command_state) ||
    command.attempt_state !== "sent" ||
    command.attempt_finished !== true ||
    typeof command.last_telemetry_at !== "string"
  ) {
    fail("the database failure active-load snapshot is invalid");
  }
  activeLoadIds.add(command.command_id);
}
const relayAFailedAt = normalizePreciseStamp(
  readText(peerDir, "relay-a-failed-at").trim(),
  "relay-a-failed-at",
);
const relayFaultCut = JSON.parse(readText(peerDir, "relay-fault-cut.json"));
const timelineRecords = readJSONL(runDir, "outbox", "timeline.jsonl");
const timeline = new Map(
  timelineRecords.map((event) => [event.event_id, event]),
);
const staleTransportProbe = JSON.parse(
  readText(runDir, "state", "stale-transport-probe.json"),
);
const staleAgentProbe = JSON.parse(
  readText(runDir, "state", "stale-agent-probe.json"),
);
const ownerTerms = readText(runDir, "state", "owner-a-terms.tsv")
  .split("\n")
  .filter((line) => line.length > 0)
  .map((line) => line.split(":"));

function timelineStamp(eventId) {
  const event = timeline.get(eventId);
  if (!event) fail(`the timeline is missing the event ${eventId}`);
  return event.timestamp;
}

// ---------------------------------------------------------------------------
// HTTP samples retain every wire attempt while preserving the logical
// idempotency-key population. The independent verifier groups attempts by
// key, validates the stale-revision retry chain, derives success once per
// logical enqueue, and uses the terminal accepted POST's wire latency for the
// frozen durable-enqueue latency SLO.
// ---------------------------------------------------------------------------

const okCommandIds = new Set();
const okCommandKeyById = new Map();
const okCommandRequestIdById = new Map();
const httpLines = [
  "timestamp,kind,status,latency_seconds,request_id,idempotency_key,attempt_ordinal,attempt_limit,requested_revision,http_status,problem_type,problem_detail,command_id,environment_id,candidate_sha",
];
const latencyText = (value) => {
  if (typeof value !== "number" || !Number.isFinite(value) || value < 0) {
    fail(`an http latency sample is not a non-negative number: ${value}`);
  }
  // fixed-point form so no sample ever reaches the CSV in exponent notation
  return value.toFixed(6);
};
for (const sample of readLog) {
  httpLines.push(csvRow(
    [
      sample.at,
      "read",
      sample.status === 200 ? "ok" : "error",
      latencyText(sample.latency_seconds),
      "",
      "",
      "",
      "",
      "",
      sample.status,
      "",
      "",
      "",
      environmentId,
      candidateSha,
    ],
    "read HTTP sample",
  ));
}
for (const sample of enqueueLog) {
  const ok =
    sample.status >= 200 && sample.status < 300 && sample.command_id !== "";
  if (ok) {
    if (okCommandIds.has(sample.command_id)) {
      fail(`accepted enqueue command id ${sample.command_id} is repeated`);
    }
    okCommandIds.add(sample.command_id);
    okCommandKeyById.set(sample.command_id, sample.idempotency_key);
    okCommandRequestIdById.set(
      sample.command_id,
      sample.attempt_request_id,
    );
  }
  httpLines.push(csvRow(
    [
      sample.at,
      "enqueue",
      ok ? "ok" : "error",
      latencyText(sample.latency_seconds),
      sample.attempt_request_id,
      sample.idempotency_key,
      sample.attempt_ordinal,
      sample.attempt_limit,
      sample.requested_revision,
      sample.status,
      sample.problem_type,
      sample.problem_detail,
      sample.command_id,
      environmentId,
      candidateSha,
    ],
    "enqueue HTTP attempt",
  ));
}
if (okCommandIds.size === 0) fail("no accepted enqueue was recorded");
const httpSamplesText = `${httpLines.join("\n")}\n`;

// ---------------------------------------------------------------------------
// Command trace, outbox snapshot, and audit correlation over the same
// accepted-write population.
// ---------------------------------------------------------------------------

const commandById = new Map(commands.map((command) => [command.id, command]));
if (commandById.size !== commands.length) {
  fail("the run-wide command dump contains duplicate command ids");
}
for (const commandId of activeLoadIds) {
  if (!commandById.has(commandId)) {
    fail(
      `active-load command ${commandId} is absent from the final command dump`,
    );
  }
}
const population = [];
for (const commandId of okCommandIds) {
  const command = commandById.get(commandId);
  if (!command) {
    fail(`the accepted enqueue ${commandId} is absent from the commands dump`);
  }
  if (command.idempotency_key !== okCommandKeyById.get(commandId)) {
    fail(
      `the accepted enqueue ${commandId} does not retain its logical idempotency key`,
    );
  }
  population.push(command);
}
population.sort((left, right) => {
  const stampOrder = compareRfc3339(
    left.created_at,
    right.created_at,
    "left command created_at",
    "right command created_at",
  );
  return stampOrder || left.id.localeCompare(right.id);
});

const relayCommandId = relayCommandProof?.command_id;
const relayCommand = commandById.get(relayCommandId);
if (
  !relayCommand ||
  okCommandIds.has(relayCommandId) ||
  relayCommandProof?.idempotency_key !== `g6-relay-failover-${runId}` ||
  relayCommand.idempotency_key !== relayCommandProof.idempotency_key ||
  normalizeUUIDIdentity(relayCommand.node_id, "relay command durable node") !==
    normalizeUUIDIdentity(relayCommandProof?.node_id, "relay command proof node") ||
  relayCommand.state !== "succeeded" ||
  relayCommandProof?.command_state !== "succeeded" ||
  relayCommandProof?.result_state !== "succeeded" ||
  relayCommandProof?.result_count !== 1
) {
  fail("relay failover command proof does not bind one successful durable command");
}
const relayCommandResultObservedAt = normalizePreciseStamp(
  relayCommandProof.result_observed_at,
  "relay command result observation",
);
const relayAgentResultCompletedAt = normalizePreciseStamp(
  relayCommandProof.agent_result_completed_at,
  "relay Agent result completion",
);
const relayCommandObservedAt = normalizePreciseStamp(
  relayCommandProof.observed_at,
  "relay command database observation",
);
const relayPreFaultCommandId = relayPreFaultCommandProof?.command_id;
const relayPreFaultCommand = commandById.get(relayPreFaultCommandId);
if (
  !relayPreFaultCommand ||
  okCommandIds.has(relayPreFaultCommandId) ||
  relayPreFaultCommandProof?.idempotency_key !==
    `g6-relay-pre-fault-${runId}` ||
  relayPreFaultCommand.idempotency_key !==
    relayPreFaultCommandProof.idempotency_key ||
  normalizeUUIDIdentity(
    relayPreFaultCommand.node_id,
    "pre-fault relay command durable node",
  ) !==
    normalizeUUIDIdentity(
      relayPreFaultCommandProof?.node_id,
      "pre-fault relay command proof node",
    ) ||
  relayPreFaultCommand.state !== "succeeded" ||
  relayPreFaultCommandProof?.command_state !== "succeeded" ||
  relayPreFaultCommandProof?.result_state !== "succeeded" ||
  relayPreFaultCommandProof?.result_count !== 1
) {
  fail(
    "pre-fault relay command proof does not bind one successful durable command",
  );
}
const relayPreFaultCommandResultObservedAt = normalizePreciseStamp(
  relayPreFaultCommandProof.result_observed_at,
  "pre-fault relay command result observation",
);
const relayPreFaultAgentResultCompletedAt = normalizePreciseStamp(
  relayPreFaultCommandProof.agent_result_completed_at,
  "pre-fault relay Agent result completion",
);
const relayPreFaultCommandObservedAt = normalizePreciseStamp(
  relayPreFaultCommandProof.observed_at,
  "pre-fault relay command database observation",
);
if (
  compareRfc3339(
    relayCommand.updated_at,
    relayCommandResultObservedAt,
    "relay command terminal update",
    "relay command result observation",
  ) !== 0 ||
  compareRfc3339(
    relayAgentResultCompletedAt,
    relayCommandObservedAt,
    "relay Agent result completion",
    "relay command database observation",
  ) > 0 ||
  compareRfc3339(
    relayCommandResultObservedAt,
    relayCommandObservedAt,
    "relay command result observation",
    "relay command database observation",
  ) > 0
) {
  fail("relay failover command proof has non-causal result timestamps");
}
if (
  compareRfc3339(
    relayPreFaultCommand.updated_at,
    relayPreFaultCommandResultObservedAt,
    "pre-fault relay command terminal update",
    "pre-fault relay command result observation",
  ) !== 0 ||
  compareRfc3339(
    relayPreFaultAgentResultCompletedAt,
    relayPreFaultCommandObservedAt,
    "pre-fault relay Agent result completion",
    "pre-fault relay command database observation",
  ) > 0 ||
  compareRfc3339(
    relayPreFaultCommandResultObservedAt,
    relayPreFaultCommandObservedAt,
    "pre-fault relay command result observation",
    "pre-fault relay command database observation",
  ) > 0
) {
  fail("pre-fault relay command proof has non-causal result timestamps");
}
const tracePopulation = [
  ...population,
  relayPreFaultCommand,
  relayCommand,
].sort((left, right) => {
  const stampOrder = compareRfc3339(
    left.created_at,
    right.created_at,
    "left trace command created_at",
    "right trace command created_at",
  );
  return stampOrder || left.id.localeCompare(right.id);
});

const firstAttemptByCommand = new Map();
const firstSentAttemptByCommand = new Map();
for (const attempt of attempts) {
  const current = firstAttemptByCommand.get(attempt.command_id);
  if (!current || attempt.attempt_number < current.attempt_number) {
    firstAttemptByCommand.set(attempt.command_id, attempt);
  }
  const sent = firstSentAttemptByCommand.get(attempt.command_id);
  if (
    attempt.state === "sent" &&
    (!sent || attempt.attempt_number < sent.attempt_number)
  ) {
    firstSentAttemptByCommand.set(attempt.command_id, attempt);
  }
}

const openingSnapshotAt = normalizePreciseStamp(
  windowOpeningActive?.captured_at,
  "window opening inflight snapshot captured_at",
);
if (
  !Number.isInteger(windowOpeningActive?.expected_count) ||
  windowOpeningActive.expected_count !== nodes.length ||
  windowOpeningActive.result_count !== 0 ||
  !Array.isArray(windowOpeningActive.commands) ||
  windowOpeningActive.commands.length !== nodes.length
) {
  fail("the window opening inflight snapshot is not the exact managed population");
}
const managedNodeIds = new Set(
  nodes.map((node) =>
    normalizeUUIDIdentity(node.nodeId, `managed node ${node.name}`),
  ),
);
const openingCommandIds = new Set();
const openingNodeIds = new Set();
const openingSnapshotCommands = [];
for (const [index, snapshotCommand] of windowOpeningActive.commands.entries()) {
  const label = `window opening inflight snapshot command ${index + 1}`;
  if (
    !snapshotCommand ||
    typeof snapshotCommand !== "object" ||
    Array.isArray(snapshotCommand) ||
    Object.keys(snapshotCommand).sort().join(",") !==
      "command_id,node_id,sent_attempt_count,state" ||
    typeof snapshotCommand.command_id !== "string" ||
    !["dispatched", "accepted", "running", "unknown"].includes(
      snapshotCommand.state,
    ) ||
    !Number.isInteger(snapshotCommand.sent_attempt_count) ||
    snapshotCommand.sent_attempt_count < 1
  ) {
    fail(`${label} is malformed`);
  }
  const command = commandById.get(snapshotCommand.command_id);
  const nodeId = normalizeUUIDIdentity(snapshotCommand.node_id, `${label} node_id`);
  if (
    !command ||
    !okCommandIds.has(snapshotCommand.command_id) ||
    !command.idempotency_key.startsWith(`g6-window-${runId}-opening-`) ||
    normalizeUUIDIdentity(command.node_id, `${label} durable node_id`) !== nodeId
  ) {
    fail(`${label} does not bind an accepted opening-wave command`);
  }
  if (openingCommandIds.has(command.id) || openingNodeIds.has(nodeId)) {
    fail("the window opening inflight snapshot repeats a command or managed node");
  }
  const attempt = firstSentAttemptByCommand.get(command.id);
  if (
    !attempt ||
    compareRfc3339(
      attempt.started_at,
      openingSnapshotAt,
      `${label} first dispatch`,
      "window opening inflight snapshot",
    ) > 0 ||
    compareRfc3339(
      command.updated_at,
      openingSnapshotAt,
      `${label} terminal result`,
      "window opening inflight snapshot",
    ) <= 0
  ) {
    fail(`${label} was not transport-accepted and result-free at the snapshot boundary`);
  }
  openingCommandIds.add(command.id);
  openingNodeIds.add(nodeId);
  openingSnapshotCommands.push({
    command_id: command.id,
    node_id: nodeId,
    state: snapshotCommand.state,
  });
}
if (
  openingNodeIds.size !== managedNodeIds.size ||
  [...managedNodeIds].some((nodeId) => !openingNodeIds.has(nodeId))
) {
  fail("the window opening inflight snapshot does not cover every managed node");
}
openingSnapshotCommands.sort((left, right) =>
  left.node_id.localeCompare(right.node_id),
);

// journal effects keyed by the binary command id (hex, dashes stripped)
const effectsByCommandHex = new Map();
const effectsDirectory = join(runDir, "state", "evidence", "effects");
const effectEntries = readdirSync(effectsDirectory)
  .filter((entry) => entry.endsWith(".tsv"))
  .sort();
const expectedEffectEntries = nodes
  .map((node) => `${node.name.replace(/^g6-/, "agent-")}.tsv`)
  .sort();
if (
  effectEntries.length !== expectedEffectEntries.length ||
  effectEntries.some((entry, index) => entry !== expectedEffectEntries[index])
) {
  fail("the final effect snapshot does not cover exactly all managed Agents");
}
for (const entry of effectEntries) {
  const service = entry.replace(/\.tsv$/, "");
  for (const line of readText(effectsDirectory, entry).split("\n")) {
    if (line.length === 0) continue;
    const [rawKeyHex, rawCommandHex, executedAt] = line.trim().split(/\s+/);
    if (
      !/^[0-9a-f]+$/i.test(rawKeyHex ?? "") ||
      !/^[0-9a-f]{32}$/i.test(rawCommandHex ?? "") ||
      !/^[0-9]+$/.test(executedAt ?? "")
    ) {
      fail(`malformed effect record in ${entry}: ${line}`);
    }
    const keyHex = rawKeyHex.toLowerCase();
    const commandHex = rawCommandHex.toLowerCase();
    if (effectsByCommandHex.has(commandHex)) {
      fail(`two agents hold an effect for command hex ${commandHex}`);
    }
    effectsByCommandHex.set(commandHex, {
      keyHex,
      effectId: `fx-${service}-${keyHex}`,
      executedSecond: BigInt(executedAt),
      observedAt: null,
    });
  }
}

const runCommandByHex = new Map(
  commands.map((command) => [
    command.id.replaceAll("-", "").toLowerCase(),
    command,
  ]),
);
for (const [commandHex, effect] of effectsByCommandHex) {
  const command = runCommandByHex.get(commandHex);
  if (!command) {
    fail(
      `durable effect ${commandHex} does not match a run-wide synthetic command`,
    );
  }
  const effectBucketStartNs = effect.executedSecond * 1_000_000_000n;
  const effectBucketEndNs = effectBucketStartNs + 1_000_000_000n;
  const commandCreatedNs = utcStampNanoseconds(
    command.created_at,
    "command created_at",
  );
  const commandUpdatedNs = utcStampNanoseconds(
    command.updated_at,
    "command updated_at",
  );
  // Agent journals intentionally store Unix seconds. Treat that value as a
  // coarse bucket rather than fabricating an exact .000000 execution time.
  // The controller terminal update is the precise durable observation that
  // the effect already exists, and therefore supplies the public trace stamp.
  if (effectBucketEndNs <= commandCreatedNs) {
    fail(`effect for ${command.id} predates its enqueue`);
  }
  if (effectBucketStartNs > commandUpdatedNs) {
    fail(`effect for ${command.id} postdates its terminal observation`);
  }
  effect.observedAt = command.updated_at;
}
for (const command of commands) {
  const commandHex = command.id.replaceAll("-", "").toLowerCase();
  if (command.state === "succeeded" && !effectsByCommandHex.has(commandHex)) {
    fail(`successful synthetic command ${command.id} has no durable effect`);
  }
}
const relayCommandEffect = effectsByCommandHex.get(
  relayCommand.id.replaceAll("-", "").toLowerCase(),
);
if (!relayCommandEffect) {
  fail("relay failover command has no durable Agent effect");
}
const relayPreFaultCommandEffect = effectsByCommandHex.get(
  relayPreFaultCommand.id.replaceAll("-", "").toLowerCase(),
);
if (!relayPreFaultCommandEffect) {
  fail("pre-fault relay command has no durable Agent effect");
}

const outcomeOf = (command) => {
  const { state } = command;
  if (state === "succeeded") return "success";
  if (state === "failed" || state === "rejected") return "failed";
  if (state === "unknown") return "unknown";
  return fail(`command state ${state} has no trace outcome mapping`, {
    path: `run/state/evidence/commands.jsonl#command_id=${command.id}/state`,
    expected: "succeeded, failed, rejected, or unknown",
    actual: state,
  });
};

const traceRecords = [];
// The relay pre-fault proof can precede the bounded HTTP population. Anchor
// the mandatory leading profile to the first command represented in the
// complete trace, not only to the first command admitted during the window.
const traceBasetime = tracePopulation[0].created_at;
traceRecords.push({
  stampMicros: utcStampMicros(traceBasetime, "trace baseline"),
  rank: 0,
  record: {
    record_type: "profile",
    dispatch_bound_seconds: 10,
  },
});
traceRecords.push({
  stampMicros: utcStampMicros(
    openingSnapshotAt,
    "window opening inflight snapshot",
  ),
  rank: 3,
  record: {
    record_type: "inflight_snapshot",
    expected_count: nodes.length,
    result_count: 0,
    commands: openingSnapshotCommands,
  },
});
for (const command of tracePopulation) {
  const commandHex = command.id.replaceAll("-", "").toLowerCase();
  const enqueuedMicros = utcStampMicros(
    command.created_at,
    "command created_at",
  );
  traceRecords.push({
    stampMicros: enqueuedMicros,
    rank: 1,
    record: {
      record_type: "enqueued",
      command_id: command.id,
      idempotency_key: command.idempotency_key,
    },
  });
  const attempt = firstAttemptByCommand.get(command.id);
  if (attempt) {
    traceRecords.push({
      stampMicros: utcStampMicros(
        attempt.started_at,
        "attempt started_at",
      ),
      rank: 2,
      record: { record_type: "dispatched", command_id: command.id },
    });
  }
  const effect = effectsByCommandHex.get(commandHex);
  if (effect) {
    traceRecords.push({
      stampMicros: utcStampMicros(
        effect.observedAt,
        "effect terminal observation",
      ),
      rank: 4,
      record: {
        record_type: "effect",
        command_id: command.id,
        idempotency_key: journalKeyIdentifier(effect.keyHex),
        effect_id: effect.effectId,
      },
    });
  }
  traceRecords.push({
    stampMicros: utcStampMicros(
      command.updated_at,
      "command updated_at",
    ),
    rank: 5,
    record: {
      record_type: "result",
      command_id: command.id,
      outcome: outcomeOf(command),
    },
  });
}
traceRecords.sort(
  (left, right) =>
    (left.stampMicros < right.stampMicros
      ? -1
      : left.stampMicros > right.stampMicros
        ? 1
        : 0) ||
    left.rank - right.rank ||
    JSON.stringify(left.record).localeCompare(JSON.stringify(right.record)),
);
const commandTraceText = jsonl(
  traceRecords.map((entry, index) => ({
    sequence: index + 1,
    timestamp: preciseIsoOfMicros(entry.stampMicros),
    environment_id: environmentId,
    candidate_sha: candidateSha,
    ...entry.record,
  })),
);

const outboxByCommand = new Map(outboxRows.map((row) => [row.command_id, row]));
const outboxSnapshotRows = [];
for (const command of population) {
  const row = outboxByCommand.get(command.id);
  if (!row) {
    fail(`the accepted enqueue ${command.id} has no outbox row`);
  }
  let state;
  if (row.published_at !== "") state = "terminal";
  else if (command.state === "unknown") state = "unknown_reconciling";
  else if (row.locked) state = "reconciliation_active";
  else state = "pending";
  outboxSnapshotRows.push({
    command_id: command.id,
    created_at: row.created_at,
    due_at: row.available_at,
    state,
  });
}
const outboxSnapshotText = canonicalJson({
  environment_id: environmentId,
  candidate_sha: candidateSha,
  snapshot_taken_at: snapshotTakenAt,
  rows: outboxSnapshotRows,
});

const auditByCommand = new Map();
for (const row of auditRows) {
  const current = auditByCommand.get(row.command_id) ?? {
    intent: false,
    intentRequestId: "",
    result: false,
    resultRequestId: "",
  };
  const acceptedRequestId = okCommandRequestIdById.get(row.command_id);
  if (row.result === "intent") {
    if (acceptedRequestId !== undefined && row.request_id !== acceptedRequestId) {
      fail(
        `audit intent for accepted enqueue ${row.command_id} does not retain terminal HTTP request_id ${acceptedRequestId}`,
      );
    }
    current.intent = true;
    current.intentRequestId = row.request_id;
  }
  if (row.result === "succeeded" || row.result === "failed") {
    if (acceptedRequestId !== undefined && row.request_id !== acceptedRequestId) {
      fail(
        `audit result for accepted enqueue ${row.command_id} does not retain terminal HTTP request_id ${acceptedRequestId}`,
      );
    }
    current.result = true;
    current.resultRequestId = row.request_id;
  }
  auditByCommand.set(row.command_id, current);
}
const auditWrites = [];
for (const command of population) {
  const record = auditByCommand.get(command.id);
  auditWrites.push({
    write_id: command.id,
    intent_recorded: record?.intent === true,
    intent_request_id: record?.intentRequestId ?? "",
    result_recorded: record?.result === true,
    result_request_id: record?.resultRequestId ?? "",
  });
}
const auditCorrelationText = canonicalJson({
  environment_id: environmentId,
  candidate_sha: candidateSha,
  writes: auditWrites,
});

// ---------------------------------------------------------------------------
// Telemetry snapshot and the agent-session inventory.
// ---------------------------------------------------------------------------

const telemetryAgents = telemetryRows.map((row) => ({
  agent_id: row.agent_id,
  last_telemetry_at: row.last_telemetry_at,
}));
const expectedAgentIds = new Set(nodes.map((node) => node.name));
const telemetryIds = new Set(telemetryAgents.map((agent) => agent.agent_id));
for (const agentId of expectedAgentIds) {
  if (!telemetryIds.has(agentId)) {
    fail(`telemetry snapshot omits the managed node ${agentId}`);
  }
}
const telemetrySnapshotText = canonicalJson({
  environment_id: environmentId,
  candidate_sha: candidateSha,
  snapshot_taken_at: snapshotTakenAt,
  freshness_bound_seconds: 90,
  agents: telemetryAgents,
});

function publicTransportObservation(observation, label) {
  if (!observation || observation.found !== true) {
    fail(`${label} did not find a live session`);
  }
  const node = normalizeUUIDIdentity(observation.node_id, `${label} node_id`);
  const endpointId = normalizeEndpointIdentity(
    observation.endpoint_id,
    `${label} endpoint_id`,
  );
  const agentInstanceId = normalizeUUIDIdentity(
    observation.agent_instance_id,
    `${label} agent_instance_id`,
  );
  const ownerFenceId = normalizeUUIDIdentity(
    observation.owner_fence_id,
    `${label} owner_fence_id`,
  );
  if (
    !/^[a-z0-9][a-z0-9._-]{0,127}$/.test(
      observation.owner_instance_id ?? "",
    )
  ) {
    fail(`${label} owner_instance_id is invalid`);
  }
  const ownerIncarnation = positiveDecimalString(
    observation.owner_incarnation,
    `${label} owner_incarnation`,
  );
  const connectionId = normalizeUUIDIdentity(
    observation.connection_id,
    `${label} connection_id`,
  );
  if (!Number.isInteger(observation.owner_epoch) || observation.owner_epoch < 1) {
    fail(`${label} owner_epoch is invalid`);
  }
  if (
    !Number.isSafeInteger(observation.authorization_revision) ||
    observation.authorization_revision < 0
  ) {
    fail(`${label} authorization_revision is invalid`);
  }
  if (
    !Array.isArray(observation.negotiated_capabilities) ||
    observation.negotiated_capabilities.length === 0 ||
    observation.negotiated_capabilities.some(
      (capability) =>
        typeof capability !== "string" ||
        !/^[a-z0-9][a-z0-9._-]{0,127}$/.test(capability),
    ) ||
    new Set(observation.negotiated_capabilities).size !==
      observation.negotiated_capabilities.length
  ) {
    fail(`${label} negotiated_capabilities are invalid`);
  }
  return {
    node,
    endpoint_id: endpointId,
    agent_instance_id: agentInstanceId,
    connected_at: normalizePreciseStamp(
      observation.connected_at,
      `${label} connected_at`,
    ),
    session_expires_at: normalizePreciseStamp(
      observation.session_expires_at,
      `${label} session_expires_at`,
    ),
    owner_fence_id: ownerFenceId,
    owner_instance: observation.owner_instance_id,
    owner_incarnation: ownerIncarnation,
    connection_id: connectionId,
    owner_epoch: observation.owner_epoch,
    owner_lease_until: normalizePreciseStamp(
      observation.owner_lease_until,
      `${label} owner_lease_until`,
    ),
    authorization_revision: observation.authorization_revision,
    negotiated_capabilities: [...observation.negotiated_capabilities].sort(),
  };
}

function publicTransportInventory(inventory, label) {
  if (inventory?.all_matched !== true || !Array.isArray(inventory.observations)) {
    fail(`${label} is not a complete matched transport inventory`);
  }
  const byNode = new Map();
  for (const [index, observation] of inventory.observations.entries()) {
    const publicObservation = publicTransportObservation(
      observation,
      `${label} observation ${index + 1}`,
    );
    if (byNode.has(publicObservation.node)) {
      fail(`${label} repeats node ${publicObservation.node}`);
    }
    byNode.set(publicObservation.node, publicObservation);
  }
  return [...byNode.values()].sort((left, right) =>
    left.node.localeCompare(right.node),
  );
}

const beforeTransportObservations = publicTransportInventory(
  beforeFinalSessions,
  "before-cut final session inventory",
);
const afterTransportObservations = publicTransportInventory(
  afterFinalSessions,
  "after-cut final session inventory",
);
const reconnectTransportObservations = publicTransportInventory(
  reconnectSessions,
  "bulk reconnect session inventory",
);
if (
  ownerReplacementSessions?.mode !== "node_connection" ||
  ownerReplacementSessions?.expected_path !== "any" ||
  ownerReplacementSessions?.all_matched !== true ||
  !Array.isArray(ownerReplacementSessions?.observations) ||
  ownerReplacementSessions.observations.some(
    (observation) =>
      observation?.matched !== true ||
      !observation?.negotiated_capabilities?.includes("ocserv.fencing.v2"),
  )
) {
  fail("replacement-owner session inventory is not a complete authenticated probe");
}
const ownerReplacementTransportObservations = publicTransportInventory(
  ownerReplacementSessions,
  "replacement-owner session inventory",
);
const ownerBTerms = new Map();
for (const [index, line] of ownerBTermsText.trimEnd().split("\n").entries()) {
  const fields = line.split("\t");
  if (fields.length !== 6) {
    fail(`replacement owner term ${index + 1} is malformed`);
  }
  const [nodeText, instance, incarnation, connectionText, epochText, registeredText] =
    fields;
  const node = normalizeUUIDIdentity(nodeText, "replacement owner term node");
  if (ownerBTerms.has(node)) {
    fail(`replacement owner terms repeat node ${node}`);
  }
  if (!/^[a-z0-9][a-z0-9._-]{0,127}$/.test(instance)) {
    fail(`replacement owner term instance is invalid for node ${node}`);
  }
  const connectionId = normalizeUUIDIdentity(
    connectionText,
    `replacement owner term ${node} connection_id`,
  );
  const epoch = Number(epochText);
  if (!/^[1-9][0-9]*$/.test(incarnation) || !Number.isSafeInteger(epoch) || epoch < 1) {
    fail(`replacement owner term authority is invalid for node ${node}`);
  }
  const registeredAt = normalizePreciseStamp(
    registeredText,
    `replacement owner term ${node} registered_at`,
  );
  ownerBTerms.set(node, {
    node,
    instance,
    incarnation,
    connectionId,
    epoch,
    registeredAt,
  });
}
if (
  ownerBTerms.size !== nodes.length ||
  ownerReplacementTransportObservations.length !== nodes.length
) {
  fail("replacement-owner evidence must cover exactly all managed nodes");
}
const ownerAcquiredAt = normalizePreciseStamp(
  timelineStamp("owner_b_acquired"),
  "owner_b_acquired timeline boundary",
);
const ownerReplacementByTerm = new Map();
for (const observation of ownerReplacementTransportObservations) {
  const managed = nodes.find(
    (node) =>
      normalizeUUIDIdentity(node.nodeId, `managed node ${node.name}`) ===
      observation.node,
  );
  const term = ownerBTerms.get(observation.node);
  if (!managed || !term) {
    fail(`replacement-owner session references unmanaged node ${observation.node}`);
  }
  if (
    observation.endpoint_id !==
      normalizeEndpointIdentity(managed.endpoint, `managed node ${managed.name} endpoint`) ||
    observation.owner_instance !== term.instance ||
    observation.owner_incarnation !== term.incarnation ||
    observation.connection_id !== term.connectionId ||
    observation.owner_epoch !== term.epoch ||
    compareRfc3339(
      observation.connected_at,
      term.registeredAt,
      "replacement-owner connected_at",
      "durable owner registration",
    ) < 0 ||
    compareRfc3339(
      observation.connected_at,
      ownerAcquiredAt,
      "replacement-owner connected_at",
      "owner_b_acquired boundary",
    ) > 0
  ) {
    fail(`replacement-owner session is not bound to durable authority for node ${observation.node}`);
  }
  const key = [
    observation.node,
    term.instance,
    term.incarnation,
    term.connectionId,
    term.epoch,
  ].join(":");
  ownerReplacementByTerm.set(key, {
    ...term,
    connectedAt: observation.connected_at,
  });
}
const reconnectByNode = new Map(
  reconnectTransportObservations.map((observation) => [
    observation.node,
    observation,
  ]),
);
if (reconnectByNode.size !== nodes.length) {
  fail("the bulk reconnect session inventory must cover exactly all managed nodes");
}
for (const node of nodes) {
  const nodeIdentity = normalizeUUIDIdentity(
    node.nodeId,
    `managed node ${node.name}`,
  );
  const reconnect = reconnectByNode.get(nodeIdentity);
  if (!reconnect) {
    fail(`the bulk reconnect session inventory omits managed node ${node.name}`);
  }
  if (
    reconnect.endpoint_id !==
    normalizeEndpointIdentity(node.endpoint, `managed node ${node.name} endpoint`)
  ) {
    fail(`the bulk reconnect session endpoint disagrees with managed node ${node.name}`);
  }
}
if (
  !(
    utcStampMicros(beforeFinalSessionsCompleteAt, "before inventory boundary") <
      utcStampMicros(authorityCutAt, "authority cut") &&
    utcStampMicros(authorityCutAt, "authority cut") <
      utcStampMicros(afterFinalSessionsStartAt, "after inventory boundary")
  )
) {
  fail("the transport inventory boundaries do not strictly bracket the authority cut");
}

const finalByNode = new Map(
  finalSessions.observations.map((observation) => [observation.node_id, observation]),
);
if (!Array.isArray(finalAuthorityCut.owners)) {
  fail("the final authority cut owners must be an array");
}
const authorityOwnerByNode = new Map();
const authorityCutOwners = [];
for (const owner of finalAuthorityCut.owners) {
  if (
    !/^[0-9a-f]{32}$/.test(owner.node_hex ?? "") ||
    !/^[a-z0-9][a-z0-9._-]{0,127}$/.test(owner.owner_instance_id ?? "") ||
    typeof owner.owner_incarnation !== "string" ||
    typeof owner.connection_id !== "string" ||
    !Number.isInteger(owner.owner_epoch) ||
    owner.owner_epoch < 1 ||
    typeof owner.history !== "string" ||
    owner.history.length === 0
  ) {
    fail("the final authority cut contains an invalid owner");
  }
  if (authorityOwnerByNode.has(owner.node_hex)) {
    fail(`the final authority cut repeats owner node ${owner.node_hex}`);
  }
  const authorityOwner = {
    instance: owner.owner_instance_id,
    incarnation: positiveDecimalString(
      owner.owner_incarnation,
      `final authority owner ${owner.node_hex} incarnation`,
    ),
    connectionId: normalizeUUIDIdentity(
      owner.connection_id,
      `final authority owner ${owner.node_hex} connection_id`,
    ),
    epoch: owner.owner_epoch,
    leaseUntil: preciseIsoOfMicros(
      utcStampMicros(
        owner.lease_until,
        `final authority owner ${owner.node_hex} lease_until`,
      ),
    ),
  };
  authorityOwnerByNode.set(owner.node_hex, authorityOwner);
  authorityCutOwners.push({
    node: owner.node_hex,
    instance: authorityOwner.instance,
    incarnation: authorityOwner.incarnation,
    connection_id: authorityOwner.connectionId,
    epoch: authorityOwner.epoch,
    lease_until: authorityOwner.leaseUntil,
  });
}
if (
  !finalAuthorityCut.leader ||
  !/^[a-z0-9][a-z0-9._-]{0,127}$/.test(
    finalAuthorityCut.leader.instance_id ?? "",
  ) ||
  typeof finalAuthorityCut.leader.incarnation !== "string" ||
  !Number.isInteger(finalAuthorityCut.leader.epoch) ||
  finalAuthorityCut.leader.epoch < 1 ||
  typeof finalAuthorityCut.leader.history !== "string" ||
  finalAuthorityCut.leader.history.length === 0
) {
  fail("the final authority cut contains an invalid scheduler leader");
}
const schedulerAuthority = {
  instance: finalAuthorityCut.leader.instance_id,
  incarnation: positiveDecimalString(
    finalAuthorityCut.leader.incarnation,
    "final authority scheduler incarnation",
  ),
  epoch: finalAuthorityCut.leader.epoch,
  lease_until: preciseIsoOfMicros(
    utcStampMicros(
      finalAuthorityCut.leader.lease_until,
      "final authority scheduler lease_until",
    ),
  ),
};
const authorityCutDocument = {
  environment_id: environmentId,
  candidate_sha: candidateSha,
  cut_at: authorityCutAt,
  transport_bracket: {
    before_complete_at: beforeFinalSessionsCompleteAt,
    after_start_at: afterFinalSessionsStartAt,
    before: beforeTransportObservations,
    after: afterTransportObservations,
  },
  owners: authorityCutOwners.sort((left, right) =>
    left.node.localeCompare(right.node),
  ),
  scheduler: {
    instance: schedulerAuthority.instance,
    incarnation: schedulerAuthority.incarnation,
    epoch: schedulerAuthority.epoch,
    lease_until: schedulerAuthority.lease_until,
  },
};
let authorityCutText;
const bulkDisconnectAt = timelineStamp("bulk_disconnect_injected");
const reconnectCompletedAt = timelineStamp("reconnect_completed");
if (
  compareRfc3339(
    reconnectCompletedAt,
    authorityCutAt,
    "reconnect completion",
    "authority cut",
  ) >= 0
) {
  fail("the reconnect completion does not precede the authority cut");
}
const sessions = [];
for (const node of nodes) {
  const started = era2Sessions.get(node.nodeId);
  const final = finalByNode.get(node.nodeId);
  const nodeIdentity = normalizeUUIDIdentity(node.nodeId, `managed node ${node.name}`);
  const reconnect = reconnectByNode.get(nodeIdentity);
  const authority = authorityOwnerByNode.get(
    node.nodeId.replaceAll("-", "").toLowerCase(),
  );
  if (!started) fail(`no era-2 session start for node ${node.nodeId}`);
  if (!final?.found) fail(`no final session for node ${node.nodeId}`);
  if (!reconnect) fail(`no reconnect session for node ${node.nodeId}`);
  if (!authority) fail(`no final owner authority for node ${node.nodeId}`);
  const finalEndpointId = normalizeEndpointIdentity(
    final.endpoint_id,
    `final session ${node.nodeId} endpoint_id`,
  );
  const expectedEndpointId = normalizeEndpointIdentity(
    node.endpoint,
    `managed node ${node.nodeId} endpoint_id`,
  );
  if (finalEndpointId !== expectedEndpointId) {
    fail(`final session endpoint disagrees with managed node ${node.nodeId}`);
  }
  const finalAgentInstanceId = normalizeUUIDIdentity(
    final.agent_instance_id,
    `final session ${node.nodeId} agent_instance_id`,
  );
  const finalConnectionId = normalizeUUIDIdentity(
    final.connection_id,
    `final session ${node.nodeId} connection_id`,
  );
  if (
    reconnect.endpoint_id !== finalEndpointId ||
    reconnect.agent_instance_id !== finalAgentInstanceId
  ) {
    fail(`bulk reconnect session binding disagrees with final agent ${node.name}`);
  }
  if (final.owner_epoch !== authority.epoch) {
    fail(`final session owner epoch disagrees with authority for ${node.nodeId}`);
  }
  if (
    final.owner_instance_id !== authority.instance ||
    final.owner_incarnation !== authority.incarnation ||
    finalConnectionId !== authority.connectionId
  ) {
    fail(`final session owner tuple disagrees with authority for ${node.nodeId}`);
  }
  const startedAt = normalizePreciseStamp(started, "era-2 session start");
  const connectedAt = normalizePreciseStamp(
    final.connected_at,
    "final session connected_at",
  );
  const sessionExpiresAt = normalizePreciseStamp(
    final.session_expires_at,
    "final session session_expires_at",
  );
  const observedOwnerLeaseUntil = normalizePreciseStamp(
    final.owner_lease_until,
    "final session owner_lease_until",
  );
  if (
    compareRfc3339(
      startedAt,
      bulkDisconnectAt,
      "session start",
      "storm",
    ) >= 0
  ) {
    fail(`session for ${node.name} does not predate the reconnect storm`);
  }
  if (
    compareRfc3339(
      reconnect.connected_at,
      bulkDisconnectAt,
      "bulk reconnect session connected_at",
      "storm",
    ) <= 0
  ) {
    fail(`bulk reconnect session for ${node.name} connects before the storm`);
  }
  if (
    compareRfc3339(
      reconnect.connected_at,
      reconnectCompletedAt,
      "bulk reconnect session connected_at",
      "reconnect completion",
    ) > 0
  ) {
    fail(`bulk reconnect session for ${node.name} connects after reconnect completion`);
  }
  if (
    compareRfc3339(
      connectedAt,
      authorityCutAt,
      "final session connected_at",
      "authority cut",
    ) > 0
  ) {
    fail(`final session for ${node.name} connects after the authority cut`);
  }
  if (
    compareRfc3339(
      sessionExpiresAt,
      authorityCutAt,
      "final session expiry",
      "authority cut",
    ) <= 0
  ) {
    fail(`final session for ${node.name} expires at the authority cut`);
  }
  if (
    compareRfc3339(
      observedOwnerLeaseUntil,
      authorityCutAt,
      "observed owner lease",
      "authority cut",
    ) <= 0
  ) {
    fail(`final owner lease for ${node.name} expires at the authority cut`);
  }
  sessions.push({
    agent_id: node.name,
    node: node.nodeId,
    endpoint_id: finalEndpointId,
    agent_instance_id: finalAgentInstanceId,
    authorized: true,
    connected: true,
    owner_instance: final.owner_instance_id,
    owner_incarnation: final.owner_incarnation,
    connection_id: finalConnectionId,
    owner_epoch: final.owner_epoch,
    owner_lease_until: authority.leaseUntil,
    session_started_at: startedAt,
    connected_at: connectedAt,
    session_expires_at: sessionExpiresAt,
    // The transport observation is the first proof that the replacement
    // session is actually live. PostgreSQL owner acquisition is causal but
    // happens before the transport registry accepts the session.
    reconnected_at: reconnect.connected_at,
    reconnect_owner_instance: reconnect.owner_instance,
    reconnect_owner_incarnation: reconnect.owner_incarnation,
    reconnect_connection_id: reconnect.connection_id,
    reconnect_owner_epoch: reconnect.owner_epoch,
  });
}
if (authorityOwnerByNode.size !== sessions.length) {
  fail("the final authority cut owner population differs from managed nodes");
}
let agentSessionsText;

// ---------------------------------------------------------------------------
// PostgreSQL recovery and relay transitions.
// ---------------------------------------------------------------------------

const outageDeclaredAt = normalizePreciseStamp(
  readText(peerDir, "isolation", "outage-declared-at").trim(),
  "outage-declared-at",
);
const isolatedAt = normalizePreciseStamp(
  readText(peerDir, "isolation", "isolated-at").trim(),
  "isolated-at",
);
const outageNs = utcStampNanoseconds(outageDeclaredAt, "outage");
const activeLoadCapturedAt = normalizePreciseStamp(
  activeLoad.captured_at,
  "active-load capture",
);
const activeLoadCapturedNs = utcStampNanoseconds(
  activeLoadCapturedAt,
  "active-load capture",
);
if (activeLoadCapturedNs > outageNs) {
  fail("the active-load snapshot was captured after the database outage", {
    path: "peer/isolation/active-load.json#/captured_at",
    expected: `<= ${outageDeclaredAt}`,
    actual: activeLoadCapturedAt,
  });
}
for (const command of activeLoad.commands) {
  const telemetryNs = utcStampNanoseconds(
    command.last_telemetry_at,
    "active-load telemetry",
  );
  if (
    telemetryNs > activeLoadCapturedNs ||
    activeLoadCapturedNs - telemetryNs > 90_000_000_000n
  ) {
    fail(
      `active-load telemetry for ${command.command_id} is not live at failure injection`,
    );
  }
}
const promotedNs = utcStampNanoseconds(promotedAt, "promotion");
const rtoStartedNs = utcStampNanoseconds(rtoStartedAt, "database RTO start");
if (rtoStartedNs >= promotedNs) {
  fail("database RTO boundaries are not ordered on the promoted database clock");
}
if (
  isolatedWrites.length < 3 ||
  isolatedWrites.some(
    (write) =>
      write.accepted !== false ||
      utcStampNanoseconds(write.at, "dual-primary probe") < promotedNs,
  )
) {
  fail("former-primary write probes do not prove fencing after promotion");
}
const acknowledged = [];
const presentTxids = [];
for (const marker of markerRows) {
  if (marker.id !== "pitr-marker-a" && marker.id !== "pitr-marker-b") continue;
  if (utcStampNanoseconds(marker.written_at, "marker") >= outageNs) {
    fail(`marker ${marker.id} was not acknowledged before the outage`);
  }
  acknowledged.push({ txid: marker.txid, acknowledged_at: marker.written_at });
  presentTxids.push(marker.txid);
}
if (acknowledged.length < 2) {
  fail("the promoted primary does not hold both acknowledged PITR markers");
}
const postgresRecoveryText = canonicalJson({
  environment_id: environmentId,
  candidate_sha: candidateSha,
  outage_declared_at: outageDeclaredAt,
  rto_started_at: rtoStartedAt,
  service_restored_at: promotedAt,
  acknowledged,
  failover: {
    old_primary: "postgres-fd-a",
    new_primary: "postgres-fd-b",
    isolated_at: isolatedAt,
    promoted_at: promotedAt,
    isolated_primary_writes: isolatedWrites.map((write) => ({
      at: write.at,
      accepted: write.accepted,
    })),
  },
  recovery: {
    restored_at: pitrReport.restore.restored_at,
    present_txids: presentTxids,
  },
});

const relayTopologyFields = [
  "agent_default_network_connected",
  "agent_service",
  "candidate_sha",
  "environment_id",
  "network_internal",
  "network_name",
  "node_id",
  "relay_alias",
  "schema_version",
  "topology_ready_at",
];
const relayStateFields = [
  "candidate_sha",
  "environment_id",
  "node_id",
  "relay",
  "schema_version",
  "state",
];
const relayTopologyReadyAt = normalizePreciseStamp(
  relayTopologyReadiness?.topology_ready_at,
  "relay-a-only topology ready_at",
);
const relayBDisabledAt = normalizePreciseStamp(
  relayBDisabled?.disabled_at,
  "relay-b disabled_at",
);
const relayBStartedAt = normalizePreciseStamp(
  relayBStarted?.started_at,
  "relay-b started_at",
);
if (
  !relayTopologyReadiness ||
  typeof relayTopologyReadiness !== "object" ||
  Array.isArray(relayTopologyReadiness) ||
  JSON.stringify(Object.keys(relayTopologyReadiness).sort()) !==
    JSON.stringify(relayTopologyFields) ||
  relayTopologyReadiness.schema_version !==
    "ocservia.g6-relay-topology.v1" ||
  relayTopologyReadiness.environment_id !== environmentId ||
  relayTopologyReadiness.candidate_sha !== candidateSha ||
  normalizeUUIDIdentity(
    relayTopologyReadiness.node_id,
    "relay-a-only topology node",
  ) !== relayBNodeId ||
  relayTopologyReadiness.agent_service !== "agent-fd-a-01" ||
  typeof relayTopologyReadiness.network_name !== "string" ||
  !relayTopologyReadiness.network_name.endsWith("_relay-a-only") ||
  relayTopologyReadiness.network_internal !== true ||
  relayTopologyReadiness.agent_default_network_connected !== false ||
  relayTopologyReadiness.relay_alias !== "relay-a"
) {
  fail("relay-a proof is not bound to the controlled internal topology");
}
for (const [marker, state, timeField, label] of [
  [relayBDisabled, "stopped", "disabled_at", "disabled"],
  [relayBStarted, "healthy", "started_at", "started"],
]) {
  if (
    !marker ||
    typeof marker !== "object" ||
    Array.isArray(marker) ||
    JSON.stringify(
      Object.keys(marker)
        .filter((field) => field !== timeField)
        .sort(),
    ) !== JSON.stringify(relayStateFields) ||
    Object.keys(marker).length !== relayStateFields.length + 1 ||
    marker.schema_version !== "ocservia.g6-relay-state.v1" ||
    marker.environment_id !== environmentId ||
    marker.candidate_sha !== candidateSha ||
    normalizeUUIDIdentity(marker.node_id, `relay-b ${label} node`) !==
      relayBNodeId ||
    marker.relay !== "relay-b" ||
    marker.state !== state
  ) {
    fail(`relay-b ${label} marker is invalid or substituted`);
  }
}

const relayManagedNode = nodes.find(
  (node) => normalizeUUIDIdentity(node.nodeId, "managed relay node") === relayBNodeId,
);
if (!relayManagedNode || !relayManagedNode.name.startsWith("g6-fd-a-")) {
  fail("relay-b observation does not select a managed cross-failure-domain agent");
}
const oneRelayObservation = (probe, relay, label) => {
  if (
    probe?.mode !== "node_connection" ||
    probe?.expected_path !== "relay" ||
    probe?.all_matched !== true ||
    !Array.isArray(probe?.observations) ||
    probe.observations.length !== 1 ||
    probe.observations[0]?.matched !== true ||
    probe.observations[0]?.path !== "relay" ||
    typeof probe.observations[0]?.path_detail !== "string" ||
    !probe.observations[0].path_detail.includes(relay)
  ) {
    fail(`${label} is not one complete matched ${relay} probe`);
  }
  return probe.observations[0];
};
const relayPreRawObservation = oneRelayObservation(
  relayPreFaultProbe,
  "relay-a",
  "pre-fault relay observation",
);
const relayPreBeforeRawObservation = oneRelayObservation(
  relayPreFaultBeforeProbe,
  "relay-a",
  "pre-fault pre-command relay observation",
);
const relayBeforeRawObservation = oneRelayObservation(
  relayBBeforeProbe,
  "relay-b",
  "pre-command relay observation",
);
const relayRawObservation = oneRelayObservation(
  relayBProbe,
  "relay-b",
  "post-command relay observation",
);
const relayTuple = (observation) =>
  [
    "node_id",
    "endpoint_id",
    "owner_fence_id",
    "owner_instance_id",
    "owner_incarnation",
    "connection_id",
    "owner_epoch",
    "path",
    "path_detail",
  ].map((field) => observation[field]);
if (
  JSON.stringify(relayTuple(relayPreBeforeRawObservation)) !==
  JSON.stringify(relayTuple(relayPreRawObservation))
) {
  fail("relay-a observations do not retain one exact session around the command");
}
if (
  JSON.stringify(relayTuple(relayBeforeRawObservation)) !==
  JSON.stringify(relayTuple(relayRawObservation))
) {
  fail("relay-b observations do not retain one exact session around the command");
}
const relayObservation = publicTransportObservation(
  relayRawObservation,
  "relay-b observation",
);
if (
  relayObservation.node !== relayBNodeId ||
  relayPreFaultNodeId !== relayBNodeId ||
  normalizeUUIDIdentity(
    relayCommandProof.node_id,
    "relay command proof node",
  ) !== relayBNodeId ||
  normalizeUUIDIdentity(
    relayPreFaultCommandProof.node_id,
    "pre-fault relay command proof node",
  ) !== relayBNodeId ||
  relayObservation.endpoint_id !==
    normalizeEndpointIdentity(relayManagedNode.endpoint, "managed relay endpoint") ||
  !relayObservation.negotiated_capabilities.includes("ocserv.fencing.v2")
) {
  fail("relay-b observation is not bound to the chosen managed node and fence");
}
if (
  relayPreFaultDispatchProof?.event_type !== "command_frame_written" ||
  relayPreFaultDispatchProof?.command_id !==
    relayPreFaultCommand.id.replaceAll("-", "") ||
  normalizeUUIDIdentity(
    relayPreFaultDispatchProof?.node_id,
    "pre-fault relay dispatch node",
  ) !== relayBNodeId ||
  relayPreFaultDispatchProof?.path !== "relay" ||
  typeof relayPreFaultDispatchProof?.path_detail !== "string" ||
  !relayPreFaultDispatchProof.path_detail.includes("relay-a") ||
  normalizeUUIDIdentity(
    relayPreFaultDispatchProof?.owner_fence_id,
    "pre-fault relay dispatch owner fence",
  ) !== normalizeUUIDIdentity(
    relayPreRawObservation.owner_fence_id,
    "pre-fault relay owner fence",
  ) ||
  normalizeUUIDIdentity(
    relayPreFaultDispatchProof?.connection_id,
    "pre-fault relay dispatch connection",
  ) !== normalizeUUIDIdentity(
    relayPreRawObservation.connection_id,
    "pre-fault relay connection",
  ) ||
  relayPreFaultDispatchProof?.owner_epoch !==
    relayPreRawObservation.owner_epoch
) {
  fail(
    "pre-fault relay dispatch proof is not the exact observed relay-a fenced session",
  );
}
if (
  relayDispatchProof?.event_type !== "command_frame_written" ||
  relayDispatchProof?.command_id !== relayCommand.id.replaceAll("-", "") ||
  normalizeUUIDIdentity(relayDispatchProof?.node_id, "relay dispatch node") !==
    relayBNodeId ||
  relayDispatchProof?.path !== "relay" ||
  typeof relayDispatchProof?.path_detail !== "string" ||
  !relayDispatchProof.path_detail.includes("relay-b") ||
  normalizeUUIDIdentity(
    relayDispatchProof?.owner_fence_id,
    "relay dispatch owner fence",
  ) !== relayObservation.owner_fence_id ||
  normalizeUUIDIdentity(
    relayDispatchProof?.connection_id,
    "relay dispatch connection",
  ) !== relayObservation.connection_id ||
  relayDispatchProof?.owner_epoch !== relayObservation.owner_epoch
) {
  fail("relay dispatch proof is not the exact observed relay-b fenced session");
}
const relayPreObservation = publicTransportObservation(
  relayPreRawObservation,
  "pre-fault relay-a observation",
);
const relayFaultCutFields = [
  "authority_lease_until",
  "connection_id",
  "cut_at",
  "node_id",
  "owner_epoch",
  "owner_incarnation",
  "owner_instance",
];
if (
  !relayFaultCut ||
  typeof relayFaultCut !== "object" ||
  Array.isArray(relayFaultCut) ||
  JSON.stringify(Object.keys(relayFaultCut).sort()) !==
    JSON.stringify(relayFaultCutFields)
) {
  fail("relay fault cut does not have the closed authority shape");
}
const relayFaultCutAt = normalizePreciseStamp(
  relayFaultCut.cut_at,
  "relay fault authority cut",
);
const relayAuthorityLeaseUntil = normalizePreciseStamp(
  relayFaultCut.authority_lease_until,
  "relay fault authority lease",
);
if (
  relayPreObservation.node !== relayBNodeId ||
  relayPreObservation.endpoint_id !== relayObservation.endpoint_id ||
  !relayPreObservation.negotiated_capabilities.includes("ocserv.fencing.v2")
) {
  fail("pre-fault relay-a observation is not bound to the chosen managed node");
}
if (
  relayFaultCutAt !== relayAFailedAt ||
  normalizeUUIDIdentity(relayFaultCut.node_id, "relay fault cut node") !==
    relayPreObservation.node ||
  relayFaultCut.owner_instance !== relayPreObservation.owner_instance ||
  relayFaultCut.owner_incarnation !== relayPreObservation.owner_incarnation ||
  normalizeUUIDIdentity(
    relayFaultCut.connection_id,
    "relay fault cut connection",
  ) !== relayPreObservation.connection_id ||
  !Number.isSafeInteger(relayFaultCut.owner_epoch) ||
  relayFaultCut.owner_epoch !== relayPreObservation.owner_epoch ||
  compareRfc3339(
    relayAuthorityLeaseUntil,
    relayFaultCutAt,
    "relay authority lease",
    "relay fault cut",
  ) <= 0 ||
  compareRfc3339(
    relayPreObservation.owner_lease_until,
    relayAFailedAt,
    "relay session lease",
    "relay conservative failure boundary",
  ) <= 0
) {
  fail("relay fault cut is not bound to one live pre-fault owner session");
}
const relayBActiveAt = relayBActiveAtSource;
if (
  compareRfc3339(
    relayTopologyReadyAt,
    relayBDisabledAt,
    "relay-a-only topology readiness",
    "relay-b disabled boundary",
  ) > 0 ||
  compareRfc3339(
    relayBDisabledAt,
    relayPreFaultCommand.created_at,
    "relay-b disabled boundary",
    "pre-fault relay command enqueue",
  ) >= 0 ||
  compareRfc3339(
    relayPreFaultCommandResultObservedAt,
    relayPreFaultObservedAt,
    "pre-fault relay command result",
    "pre-fault relay proof boundary",
  ) > 0 ||
  compareRfc3339(
    relayPreFaultCommandObservedAt,
    relayPreFaultObservedAt,
    "pre-fault relay command observation",
    "pre-fault relay proof boundary",
  ) > 0 ||
  compareRfc3339(
    relayPreFaultObservedAt,
    relayAFailedAt,
    "pre-fault relay-a observation",
    "relay-a failure",
  ) >= 0 ||
  compareRfc3339(
    relayBStartedAt,
    relayAFailedAt,
    "relay-b start",
    "relay fault cut",
  ) <= 0 ||
  compareRfc3339(
    relayCommand.created_at,
    relayBStartedAt,
    "relay-b command enqueue",
    "relay-b start",
  ) <= 0 ||
  compareRfc3339(
    relayCommand.created_at,
    relayAFailedAt,
    "relay-b command enqueue",
    "relay fault cut",
  ) <= 0 ||
  compareRfc3339(
    relayAFailedAt,
    relayCommandResultObservedAt,
    "relay-a failure",
    "relay command result",
  ) >= 0 ||
  compareRfc3339(
    relayAFailedAt,
    relayCommandObservedAt,
    "relay fault cut",
    "relay-b command observation",
  ) >= 0 ||
  compareRfc3339(
    relayAFailedAt,
    relayBActiveAt,
    "relay fault cut",
    "relay-b active boundary",
  ) >= 0 ||
  compareRfc3339(
    relayCommandObservedAt,
    relayBActiveAt,
    "relay command observation",
    "relay-b active boundary",
  ) > 0 ||
  compareRfc3339(
    relayObservation.connected_at,
    relayBActiveAt,
    "relay-b session connected_at",
    "relay-b active boundary",
  ) > 0 ||
  compareRfc3339(
    relayObservation.owner_lease_until,
    relayBActiveAt,
    "relay-b owner lease",
    "relay-b active boundary",
  ) <= 0 ||
  compareRfc3339(
    relayObservation.session_expires_at,
    relayBActiveAt,
    "relay-b session expiry",
    "relay-b active boundary",
  ) <= 0
) {
  fail("relay failover evidence has invalid authoritative database boundaries");
}
const relayOwnerTermKey = ownerRegistrationKey(
  relayObservation.node,
  relayObservation.owner_instance,
  relayObservation.owner_incarnation,
  relayObservation.connection_id,
  relayObservation.owner_epoch,
);
const relayTransitionsText = jsonl([
  {
    sequence: 1,
    timestamp: relayPreFaultObservedAt,
    environment_id: environmentId,
    candidate_sha: candidateSha,
    event_type: "path_active",
    session_id: relayPreObservation.node,
    path: "relay",
    relay: "relay-a",
    authenticated: true,
    endpoint_id: relayPreObservation.endpoint_id,
    path_detail: relayPreRawObservation.path_detail,
    owner_fence_id: relayPreObservation.owner_fence_id,
    owner_instance: relayPreObservation.owner_instance,
    owner_incarnation: relayPreObservation.owner_incarnation,
    connection_id: relayPreObservation.connection_id,
    owner_epoch: relayPreObservation.owner_epoch,
    authorization_revision: relayPreObservation.authorization_revision,
    negotiated_capabilities: relayPreObservation.negotiated_capabilities,
    session_connected_at: relayPreObservation.connected_at,
    owner_lease_until: relayPreObservation.owner_lease_until,
    session_expires_at: relayPreObservation.session_expires_at,
    topology_mode: "relay-a-only",
    topology_network_name: relayTopologyReadiness.network_name,
    topology_agent_service: relayTopologyReadiness.agent_service,
    topology_network_internal: relayTopologyReadiness.network_internal,
    topology_agent_default_network_connected:
      relayTopologyReadiness.agent_default_network_connected,
    topology_ready_at: relayTopologyReadyAt,
    relay_b_disabled_at: relayBDisabledAt,
    command_id: relayPreFaultCommand.id,
    command_idempotency_key: relayPreFaultCommand.idempotency_key,
    effect_idempotency_key: journalKeyIdentifier(
      relayPreFaultCommandEffect.keyHex,
    ),
    effect_id: relayPreFaultCommandEffect.effectId,
    result_observed_at: relayPreFaultCommandResultObservedAt,
  },
  {
    sequence: 2,
    timestamp: relayAFailedAt,
    environment_id: environmentId,
    candidate_sha: candidateSha,
    event_type: "relay_failed",
    relay: "relay-a",
    session_id: relayPreObservation.node,
    owner_instance: relayPreObservation.owner_instance,
    owner_incarnation: relayPreObservation.owner_incarnation,
    connection_id: relayPreObservation.connection_id,
    owner_epoch: relayPreObservation.owner_epoch,
    owner_lease_until: relayPreObservation.owner_lease_until,
    authority_lease_until: relayAuthorityLeaseUntil,
    fault_cut_at: relayFaultCutAt,
  },
  {
    sequence: 3,
    timestamp: relayBActiveAt,
    environment_id: environmentId,
    candidate_sha: candidateSha,
    event_type: "path_active",
    session_id: relayObservation.node,
    path: "relay",
    relay: "relay-b",
    authenticated: true,
    endpoint_id: relayObservation.endpoint_id,
    path_detail: relayDispatchProof.path_detail,
    owner_fence_id: relayObservation.owner_fence_id,
    owner_instance: relayObservation.owner_instance,
    owner_incarnation: relayObservation.owner_incarnation,
    connection_id: relayObservation.connection_id,
    owner_epoch: relayObservation.owner_epoch,
    authorization_revision: relayObservation.authorization_revision,
    negotiated_capabilities: relayObservation.negotiated_capabilities,
    session_connected_at: relayObservation.connected_at,
    owner_lease_until: relayObservation.owner_lease_until,
    session_expires_at: relayObservation.session_expires_at,
    relay_b_started_at: relayBStartedAt,
    command_id: relayCommand.id,
    command_idempotency_key: relayCommand.idempotency_key,
    effect_idempotency_key: journalKeyIdentifier(relayCommandEffect.keyHex),
    effect_id: relayCommandEffect.effectId,
    result_observed_at: relayCommandResultObservedAt,
  },
]);

// ---------------------------------------------------------------------------
// Epoch events from the authoritative fencing and leadership journals. Each
// row carries the durable history id assigned in the same transaction as the
// authority mutation. Timestamps are still the public event clock, while the
// history id preserves causality when several transitions land in one second.
// ---------------------------------------------------------------------------

const epochEvents = [];
const rankOf = {
  expired: 0,
  retired: 0,
  registered: 1,
  acquired: 1,
  commit: 2,
  accept: 3,
};

// Journal lines end in two microsecond RFC 3339 stamps whose colons forbid a
// naive split; anchor on the trailing timestamps and split the leading fields.
const trailingStamps =
  /^(.*):(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{6}Z):(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{6}Z)$/;

function historyIdOf(value, label) {
  return BigInt(positiveDecimalString(value, label));
}

function pushEpochEvent({
  stampMicros,
  stampNanoseconds,
  sourceKind = null,
  historyId = null,
  causeOrder = 0,
  rank,
  record,
}) {
  if ((stampMicros === undefined) === (stampNanoseconds === undefined)) {
    fail("epoch event must declare exactly one timestamp precision");
  }
  epochEvents.push({
    stampNanoseconds:
      stampNanoseconds === undefined ? stampMicros * 1000n : stampNanoseconds,
    preserveNanoseconds: stampNanoseconds !== undefined,
    sourceKind,
    historyId,
    causeOrder,
    rank,
    record,
  });
}

function epochEventTimestamp(entry) {
  return entry.preserveNanoseconds
    ? preciseIsoOfNanoseconds(entry.stampNanoseconds)
    : preciseIsoOfMicros(entry.stampNanoseconds / 1000n);
}

const ownerLines = publishedHistoryLines(
  readText(runDir, "outbox", "fencing-history.jsonl"),
  "published fencing history",
);
requireFrozenHistoryMatch(
  frozenOwnerHistory,
  ownerLines,
  "published fencing history",
);
const ownerState = new Map();
const ownerRegistrationStamps = new Map();
const consumedOwnerReplacementTerms = new Set();
let previousOwnerHistoryId = 0n;

function ownerRegistrationKey(node, instance, incarnation, connectionId, epoch) {
  return [node, instance, incarnation, connectionId, epoch].join(":");
}

function recordOwnerRegistration(
  node,
  instance,
  incarnation,
  connectionId,
  epoch,
  stampMicros,
) {
  const key = ownerRegistrationKey(
    node,
    instance,
    incarnation,
    connectionId,
    epoch,
  );
  if (ownerRegistrationStamps.has(key)) {
    fail(`fencing history repeats owner registration term ${key}`);
  }
  ownerRegistrationStamps.set(key, preciseIsoOfMicros(stampMicros));
}

function attachOwnerSessionCompletion(record, stampMicros) {
  const key = ownerRegistrationKey(
    record.node,
    record.instance,
    record.incarnation,
    record.connection_id,
    record.epoch,
  );
  const completion = ownerReplacementByTerm.get(key);
  if (!completion) return;
  if (
    utcStampMicros(completion.registeredAt, "replacement owner term registration") !==
    stampMicros
  ) {
    fail(`replacement owner term registration disagrees with durable history for node ${record.node}`);
  }
  record.session_connected_at = completion.connectedAt;
  consumedOwnerReplacementTerms.add(key);
}

for (const line of ownerLines) {
  const match = trailingStamps.exec(line);
  const head = match?.[1]?.split(":") ?? [];
  if (!match || head.length !== 6) {
    fail(`malformed fencing history line: ${line}`);
  }
  const [
    historyIdText,
    nodeText,
    instance,
    incarnationText,
    connectionIdText,
    epochText,
  ] = head;
  const leaseUntil = match[2];
  const updatedAt = match[3];
  const historyId = historyIdOf(historyIdText, "fencing history transition id");
  if (historyId <= previousOwnerHistoryId) {
    fail("fencing history transition ids must strictly increase");
  }
  previousOwnerHistoryId = historyId;
  const nodeHex = normalizeUUIDIdentity(nodeText, "fencing history node");
  const incarnation = positiveDecimalString(
    incarnationText,
    "fencing history incarnation",
  );
  const connectionId = normalizeUUIDIdentity(
    connectionIdText,
    "fencing history connection_id",
  );
  const epoch = Number(epochText);
  if (!Number.isInteger(epoch) || epoch < 1) {
    fail(`fencing history carries an invalid epoch: ${line}`);
  }
  const leaseUntilMicros = utcStampMicros(leaseUntil, "fencing lease_until");
  const updatedAtMicros = utcStampMicros(updatedAt, "fencing updated_at");
  const current = ownerState.get(nodeHex);
  if (!current) {
    if (epoch !== 1) {
      fail(`fencing history for node ${nodeHex} must begin at epoch 1`);
    }
    if (leaseUntilMicros <= updatedAtMicros) {
      fail(
        `fencing history observes expired epoch ${epoch} without a recorded acquisition for node ${nodeHex}`,
      );
    }
    const registrationRecord = {
      subject: "connection_owner",
      event_type: "owner_registered",
      node: nodeHex,
      instance,
      incarnation,
      connection_id: connectionId,
      epoch,
      lease_until: preciseIsoOfMicros(leaseUntilMicros),
    };
    attachOwnerSessionCompletion(registrationRecord, updatedAtMicros);
    pushEpochEvent({
      stampMicros: updatedAtMicros,
      sourceKind: "owner",
      historyId,
      rank: rankOf.registered,
      record: registrationRecord,
    });
    recordOwnerRegistration(
      nodeHex,
      instance,
      incarnation,
      connectionId,
      epoch,
      updatedAtMicros,
    );
    ownerState.set(nodeHex, {
      epoch,
      leaseUntil,
      leaseUntilMicros,
      instance,
      incarnation,
      connectionId,
      registrationRecord,
      lastUpdatedAtMicros: updatedAtMicros,
      active: true,
    });
  } else if (current.epoch !== epoch) {
    if (epoch !== current.epoch + 1) {
      fail(
        `fencing history for node ${nodeHex} must advance exactly from epoch ${current.epoch} to ${current.epoch + 1}`,
      );
    }
    if (updatedAtMicros < current.lastUpdatedAtMicros) {
      fail(`fencing history moves backwards for node ${nodeHex}`);
    }
    if (leaseUntilMicros <= updatedAtMicros) {
      fail(
        `fencing history observes expired epoch ${epoch} without a recorded acquisition for node ${nodeHex}`,
      );
    }
    let registrationCauseOrder = 0;
    if (current.active) {
      const naturallyExpired = current.leaseUntilMicros <= updatedAtMicros;
      if (
        !naturallyExpired &&
        (current.instance !== instance || current.incarnation !== incarnation)
      ) {
        fail(
          `fencing history replaces live owner epoch ${current.epoch} across instances for node ${nodeHex}`,
        );
      }
      pushEpochEvent({
        stampMicros: naturallyExpired
          ? current.leaseUntilMicros
          : updatedAtMicros,
        sourceKind: "owner",
        historyId,
        causeOrder: 0,
        rank: naturallyExpired ? rankOf.expired : rankOf.retired,
        record: {
          subject: "connection_owner",
          event_type: naturallyExpired
            ? "owner_lease_expired"
            : "owner_retired",
          node: nodeHex,
          epoch: current.epoch,
        },
      });
      registrationCauseOrder = 1;
    }
    const registrationRecord = {
      subject: "connection_owner",
      event_type: "owner_registered",
      node: nodeHex,
      instance,
      incarnation,
      connection_id: connectionId,
      epoch,
      lease_until: preciseIsoOfMicros(leaseUntilMicros),
    };
    attachOwnerSessionCompletion(registrationRecord, updatedAtMicros);
    pushEpochEvent({
      stampMicros: updatedAtMicros,
      sourceKind: "owner",
      historyId,
      causeOrder: registrationCauseOrder,
      rank: rankOf.registered,
      record: registrationRecord,
    });
    recordOwnerRegistration(
      nodeHex,
      instance,
      incarnation,
      connectionId,
      epoch,
      updatedAtMicros,
    );
    ownerState.set(nodeHex, {
      epoch,
      leaseUntil,
      leaseUntilMicros,
      instance,
      incarnation,
      connectionId,
      registrationRecord,
      lastUpdatedAtMicros: updatedAtMicros,
      active: true,
    });
  } else {
    if (
      current.instance !== instance ||
      current.incarnation !== incarnation ||
      current.connectionId !== connectionId
    ) {
      fail(
        `fencing history changes an owner tuple without a new epoch: ${line}`,
      );
    }
    if (updatedAtMicros < current.lastUpdatedAtMicros) {
      fail(`fencing history moves backwards for node ${nodeHex}`);
    }
    if (!current.active) {
      fail(
        `fencing history updates retired owner epoch ${epoch} for node ${nodeHex}`,
      );
    }
    if (leaseUntilMicros <= updatedAtMicros) {
      pushEpochEvent({
        stampMicros: updatedAtMicros,
        sourceKind: "owner",
        historyId,
        rank: rankOf.retired,
        record: {
          subject: "connection_owner",
          event_type: "owner_retired",
          node: nodeHex,
          epoch,
        },
      });
      current.active = false;
    } else {
      if (leaseUntilMicros < current.leaseUntilMicros) {
        fail(
          `fencing history shortens live owner epoch ${epoch} for node ${nodeHex}`,
        );
      }
      current.leaseUntil = leaseUntil;
      current.leaseUntilMicros = leaseUntilMicros;
      current.registrationRecord.lease_until =
        preciseIsoOfMicros(leaseUntilMicros);
    }
    current.lastUpdatedAtMicros = updatedAtMicros;
  }
}

if (consumedOwnerReplacementTerms.size !== ownerReplacementByTerm.size) {
  fail("durable owner history omits a replacement-owner session term");
}

const relayOwnerRegisteredAt = ownerRegistrationStamps.get(relayOwnerTermKey);
if (
  !relayOwnerRegisteredAt ||
  compareRfc3339(
    relayOwnerRegisteredAt ?? relayBActiveAt,
    relayObservation.connected_at,
    "relay-b durable owner registration",
    "relay-b session connected_at",
  ) > 0
) {
  fail("relay-b observation has no causal durable owner registration");
}

for (const session of sessions) {
  const node = normalizeUUIDIdentity(session.node, "agent session node");
  const key = ownerRegistrationKey(
    node,
    session.reconnect_owner_instance,
    session.reconnect_owner_incarnation,
    session.reconnect_connection_id,
    session.reconnect_owner_epoch,
  );
  const registrationAt = ownerRegistrationStamps.get(key);
  if (!registrationAt) {
    fail(`bulk reconnect tuple has no durable owner registration for node ${node}`);
  }
  if (
    compareRfc3339(
      registrationAt,
      bulkDisconnectAt,
      "bulk reconnect owner registration",
      "bulk disconnect",
    ) <= 0 ||
    compareRfc3339(
      registrationAt,
      reconnectCompletedAt,
      "bulk reconnect owner registration",
      "reconnect completion",
    ) > 0
  ) {
    fail(`bulk reconnect owner registration falls outside the storm for node ${node}`);
  }
  if (
    compareRfc3339(
      session.reconnected_at,
      registrationAt,
      "bulk reconnect transport connected_at",
      "bulk reconnect owner registration",
    ) < 0
  ) {
    fail(`bulk reconnect transport session predates owner registration for node ${node}`);
  }
  if (session.reconnect_owner_epoch > session.owner_epoch) {
    fail(`bulk reconnect owner epoch exceeds final authority for node ${node}`);
  }
  if (session.reconnect_owner_epoch === session.owner_epoch) {
    if (
      session.reconnect_owner_instance !== session.owner_instance ||
      session.reconnect_owner_incarnation !== session.owner_incarnation ||
      session.reconnect_connection_id !== session.connection_id
    ) {
      fail(`same-epoch bulk reconnect tuple differs from final authority for node ${node}`);
    }
  } else if (
    compareRfc3339(
      session.connected_at,
      registrationAt,
      "final session connected_at",
      "bulk reconnect owner registration",
    ) < 0
  ) {
    fail(`replacement final session predates the bulk reconnect for node ${node}`);
  }
}
agentSessionsText = canonicalJson({
  environment_id: environmentId,
  candidate_sha: candidateSha,
  snapshot_taken_at: snapshotTakenAt,
  sessions,
  scheduler_authority: schedulerAuthority,
  reconnect_storm: { bulk_disconnect_at: bulkDisconnectAt },
});

const [staleNodeHex, staleInstance, , , staleEpochText] = ownerTerms[0];
if (staleTransportProbe.status !== "rejected") {
  fail("the stale-transport probe did not record a rejection");
}
if (staleAgentProbe.status !== "rejected") {
  fail("the stale-agent probe did not record a rejection");
}
pushEpochEvent({
  stampNanoseconds: utcStampNanoseconds(
    timelineStamp("stale_transport_rejected"),
    "timeline",
  ),
  rank: rankOf.accept,
  record: {
    subject: "connection_owner",
    event_type: "owner_accept",
    node: staleNodeHex,
    instance: staleInstance,
    epoch: Number(staleEpochText),
    accepted: false,
  },
});
pushEpochEvent({
  stampNanoseconds: utcStampNanoseconds(
    timelineStamp("stale_agent_rejected"),
    "timeline",
  ),
  rank: rankOf.accept,
  record: {
    subject: "connection_owner",
    event_type: "owner_accept",
    node: staleNodeHex,
    instance: staleInstance,
    epoch: Number(staleEpochText),
    accepted: false,
  },
});

const leaderLines = publishedHistoryLines(
  readText(runDir, "outbox", "leadership-history.jsonl"),
  "published leadership history",
);
requireFrozenHistoryMatch(
  frozenSchedulerHistory,
  leaderLines,
  "published leadership history",
);
let leaderCurrent = null;
let previousLeaderHistoryId = 0n;
const leaderTerms = new Map();

function schedulerTermKey(instance, incarnation, epoch) {
  return `${instance}:${incarnation}:${epoch}`;
}

for (const line of leaderLines) {
  const match = trailingStamps.exec(line);
  const head = match?.[1]?.split(":") ?? [];
  if (!match || head.length !== 4) {
    fail(`malformed leadership history line: ${line}`);
  }
  const [historyIdText, instance, incarnationText, epochText] = head;
  const leaseUntil = match[2];
  const updatedAt = match[3];
  const historyId = historyIdOf(
    historyIdText,
    "leadership history transition id",
  );
  if (historyId <= previousLeaderHistoryId) {
    fail("leadership history transition ids must strictly increase");
  }
  previousLeaderHistoryId = historyId;
  const incarnation = positiveDecimalString(
    incarnationText,
    "leadership history incarnation",
  );
  const epoch = Number(epochText);
  if (!Number.isInteger(epoch) || epoch < 1) {
    fail(`leadership history carries an invalid epoch: ${line}`);
  }
  const leaseUntilMicros = utcStampMicros(leaseUntil, "leadership lease_until");
  const updatedAtMicros = utcStampMicros(updatedAt, "leadership updated_at");
  if (!leaderCurrent) {
    if (epoch !== 1) {
      fail("leadership history must begin at epoch 1");
    }
    if (leaseUntilMicros <= updatedAtMicros) {
      fail(
        `leadership history observes expired epoch ${epoch} without a recorded acquisition`,
      );
    }
    const acquisitionRecord = {
      subject: "scheduler",
      event_type: "leader_acquired",
      instance,
      incarnation,
      epoch,
      lease_until: preciseIsoOfMicros(leaseUntilMicros),
    };
    leaderCurrent = {
      instance,
      incarnation,
      epoch,
      acquiredAtMicros: updatedAtMicros,
      leaseUntil,
      leaseUntilMicros,
      maintenanceCount: 0,
      lastUpdatedAtMicros: updatedAtMicros,
      acquisitionRecord,
      active: true,
    };
    leaderTerms.set(
      schedulerTermKey(instance, incarnation, epoch),
      leaderCurrent,
    );
    pushEpochEvent({
      stampMicros: updatedAtMicros,
      sourceKind: "scheduler",
      historyId,
      rank: rankOf.acquired,
      record: acquisitionRecord,
    });
  } else if (leaderCurrent.epoch !== epoch) {
    if (epoch !== leaderCurrent.epoch + 1) {
      fail(
        `leadership history must advance exactly from epoch ${leaderCurrent.epoch} to ${leaderCurrent.epoch + 1}`,
      );
    }
    if (updatedAtMicros < leaderCurrent.lastUpdatedAtMicros) {
      fail("leadership history moves backwards");
    }
    if (leaseUntilMicros <= updatedAtMicros) {
      fail(
        `leadership history observes expired epoch ${epoch} without a recorded acquisition`,
      );
    }
    if (
      leaderCurrent.active &&
      leaderCurrent.leaseUntilMicros > updatedAtMicros
    ) {
      fail(
        `leadership history replaces live epoch ${leaderCurrent.epoch} before lease expiry`,
      );
    }
    let acquisitionCauseOrder = 0;
    if (leaderCurrent.active) {
      pushEpochEvent({
        stampMicros: leaderCurrent.leaseUntilMicros,
        sourceKind: "scheduler",
        historyId,
        causeOrder: 0,
        rank: rankOf.expired,
        record: {
          subject: "scheduler",
          event_type: "leader_lease_expired",
          epoch: leaderCurrent.epoch,
        },
      });
      acquisitionCauseOrder = 1;
    }
    const acquisitionRecord = {
      subject: "scheduler",
      event_type: "leader_acquired",
      instance,
      incarnation,
      epoch,
      lease_until: preciseIsoOfMicros(leaseUntilMicros),
    };
    leaderCurrent = {
      instance,
      incarnation,
      epoch,
      acquiredAtMicros: updatedAtMicros,
      leaseUntil,
      leaseUntilMicros,
      maintenanceCount: 0,
      lastUpdatedAtMicros: updatedAtMicros,
      acquisitionRecord,
      active: true,
    };
    leaderTerms.set(
      schedulerTermKey(instance, incarnation, epoch),
      leaderCurrent,
    );
    pushEpochEvent({
      stampMicros: updatedAtMicros,
      sourceKind: "scheduler",
      historyId,
      causeOrder: acquisitionCauseOrder,
      rank: rankOf.acquired,
      record: acquisitionRecord,
    });
  } else {
    if (
      leaderCurrent.instance !== instance ||
      leaderCurrent.incarnation !== incarnation
    ) {
      fail(
        `leadership history changes a leader tuple without a new epoch: ${line}`,
      );
    }
    if (updatedAtMicros < leaderCurrent.lastUpdatedAtMicros) {
      fail("leadership history moves backwards");
    }
    if (!leaderCurrent.active) {
      fail(`leadership history updates expired epoch ${epoch}`);
    }
    if (leaseUntilMicros <= updatedAtMicros) {
      pushEpochEvent({
        stampMicros: updatedAtMicros,
        sourceKind: "scheduler",
        historyId,
        rank: rankOf.expired,
        record: {
          subject: "scheduler",
          event_type: "leader_lease_expired",
          epoch,
        },
      });
      leaderCurrent.active = false;
    } else {
      if (leaseUntilMicros < leaderCurrent.leaseUntilMicros) {
        fail(`leadership history shortens live epoch ${epoch}`);
      }
      leaderCurrent.leaseUntil = leaseUntil;
      leaderCurrent.leaseUntilMicros = leaseUntilMicros;
      leaderCurrent.acquisitionRecord.lease_until =
        preciseIsoOfMicros(leaseUntilMicros);
    }
    leaderCurrent.lastUpdatedAtMicros = updatedAtMicros;
  }
}

if (
  !leaderCurrent ||
  !leaderCurrent.active ||
  leaderCurrent.instance !== schedulerAuthority.instance ||
  leaderCurrent.incarnation !== schedulerAuthority.incarnation ||
  leaderCurrent.epoch !== schedulerAuthority.epoch
) {
  fail("the frozen leadership journal does not match the live final authority term");
}
const authorityLeaderLeaseMicros = utcStampMicros(
  finalAuthorityCut.leader.lease_until,
  "final authority scheduler lease_until",
);
if (
  authorityLeaderLeaseMicros !== leaderCurrent.leaseUntilMicros ||
  authorityLeaderLeaseMicros <= utcStampMicros(authorityCutAt, "authority cut")
) {
  fail("the frozen leadership journal does not match the live final authority lease");
}
const finalLeaderHistory = leaderLines.at(-1);
if (
  finalAuthorityCut.leader.history !==
  finalLeaderHistory.slice(finalLeaderHistory.indexOf(":") + 1)
) {
  fail("the final authority leader row is not the last frozen leadership row");
}

const maintenanceLines = publishedHistoryLines(
  readText(runDir, "outbox", "scheduler-maintenance-history.jsonl"),
  "published scheduler maintenance history",
);
requireFrozenHistoryMatch(
  frozenSchedulerMaintenanceHistory,
  maintenanceLines,
  "published scheduler maintenance history",
);
const maintenanceLinePattern =
  /^([1-9][0-9]*):([^:]+):([0-9]+):([1-9][0-9]*):(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{6}Z)$/;
let previousMaintenanceId = 0n;
let previousMaintenanceAtMicros = null;
let liveSchedulerMaintenance = null;
let firstReplacementMaintenanceId = null;
let matchedSchedulerMaintenanceObservation = false;
for (const line of maintenanceLines) {
  const match = maintenanceLinePattern.exec(line);
  if (!match) {
    fail(`malformed scheduler maintenance history line: ${line}`);
  }
  const [, maintenanceIdText, instance, incarnationText, epochText, completedAt] =
    match;
  const maintenanceId = historyIdOf(
    maintenanceIdText,
    "scheduler maintenance id",
  );
  if (maintenanceId <= previousMaintenanceId) {
    fail("scheduler maintenance ids must strictly increase");
  }
  previousMaintenanceId = maintenanceId;
  const incarnation = positiveDecimalString(
    incarnationText,
    "scheduler maintenance incarnation",
  );
  const epoch = Number(epochText);
  if (!Number.isInteger(epoch) || epoch < 1) {
    fail(`scheduler maintenance carries an invalid epoch: ${line}`);
  }
  const completedAtMicros = utcStampMicros(
    completedAt,
    "scheduler maintenance completed_at",
  );
  if (completedAtMicros > utcStampMicros(authorityCutAt, "authority cut")) {
    fail(
      `scheduler maintenance ${maintenanceIdText} was committed after the final authority cut`,
    );
  }
  if (
    previousMaintenanceAtMicros !== null &&
    completedAtMicros < previousMaintenanceAtMicros
  ) {
    fail("scheduler maintenance history moves backwards");
  }
  previousMaintenanceAtMicros = completedAtMicros;
  const term = leaderTerms.get(schedulerTermKey(instance, incarnation, epoch));
  if (!term) {
    fail(
      `scheduler maintenance ${maintenanceIdText} references an unacquired exact term`,
    );
  }
  if (completedAtMicros < term.acquiredAtMicros) {
    fail(
      `scheduler maintenance ${maintenanceIdText} predates leadership epoch ${epoch}`,
    );
  }
  if (completedAtMicros >= term.leaseUntilMicros) {
    fail(
      `scheduler maintenance ${maintenanceIdText} was not committed under a live exact term`,
    );
  }
  term.maintenanceCount += 1;
  const isReplacementTerm =
    instance === schedulerReplacementInstance &&
    incarnation === schedulerReplacementIncarnation &&
    epoch === schedulerReplacementEpoch;
  if (isReplacementTerm && firstReplacementMaintenanceId === null) {
    firstReplacementMaintenanceId = maintenanceId;
  }
  const isObservedReplacementMaintenance =
    isReplacementTerm &&
    maintenanceIdText === observedSchedulerMaintenanceId &&
    completedAtMicros === observedSchedulerMarkerAtMicros;
  if (isObservedReplacementMaintenance) {
    if (matchedSchedulerMaintenanceObservation) {
      fail("scheduler maintenance observation matches multiple durable markers");
    }
    matchedSchedulerMaintenanceObservation = true;
  }
  const normalizedCompletedAt = normalizePreciseStamp(
    completedAt,
    "scheduler maintenance completed_at",
  );
  if (
    instance === schedulerAuthority.instance &&
    incarnation === schedulerAuthority.incarnation &&
    epoch === schedulerAuthority.epoch
  ) {
    liveSchedulerMaintenance = {
      maintenance_id: maintenanceIdText,
      maintenance_completed_at: normalizedCompletedAt,
    };
  }
  pushEpochEvent({
    stampMicros: isObservedReplacementMaintenance
      ? schedulerMaintenanceCommittedObservedAtMicros
      : completedAtMicros,
    sourceKind: "scheduler-maintenance",
    historyId: maintenanceId,
    rank: rankOf.commit,
    record: {
      subject: "scheduler",
      event_type: "leader_commit",
      instance,
      incarnation,
      epoch,
      maintenance_id: maintenanceIdText,
      marker_completed_at: normalizedCompletedAt,
      accepted: true,
    },
  });
}
for (const term of leaderTerms.values()) {
  if (term.maintenanceCount === 0) {
    fail(
      `leadership epoch ${term.epoch} has no exact-term fenced maintenance completion`,
    );
  }
}
if (!matchedSchedulerMaintenanceObservation) {
  fail(
    "scheduler maintenance observation does not match a durable replacement marker",
  );
}
if (firstReplacementMaintenanceId !== BigInt(observedSchedulerMaintenanceId)) {
  fail(
    "scheduler maintenance observation does not bind the first durable replacement marker",
  );
}
if (!liveSchedulerMaintenance) {
  fail("the live final authority term has no frozen maintenance completion");
}
Object.assign(authorityCutDocument.scheduler, liveSchedulerMaintenance);
authorityCutText = canonicalJson(authorityCutDocument);
const staleTerm = readText(runDir, "state", "stale-scheduler-term").trim();
const [staleLeaderInstance, staleLeaderIncarnation, staleLeaderEpochText] =
  staleTerm.split(":");
pushEpochEvent({
  stampNanoseconds: utcStampNanoseconds(
    timelineStamp("stale_scheduler_commit_rejected"),
    "timeline",
  ),
  rank: rankOf.accept,
  record: {
    subject: "scheduler",
    event_type: "leader_commit",
    instance: staleLeaderInstance,
    incarnation: positiveDecimalString(
      staleLeaderIncarnation,
      "stale scheduler incarnation",
    ),
    epoch: Number(staleLeaderEpochText),
    accepted: false,
  },
});

epochEvents.sort((left, right) => {
  if (left.stampNanoseconds < right.stampNanoseconds) return -1;
  if (left.stampNanoseconds > right.stampNanoseconds) return 1;
  if (
    left.sourceKind !== null &&
    left.sourceKind === right.sourceKind &&
    left.historyId !== null &&
    right.historyId !== null
  ) {
    if (left.historyId < right.historyId) return -1;
    if (left.historyId > right.historyId) return 1;
    if (left.causeOrder !== right.causeOrder) {
      return left.causeOrder - right.causeOrder;
    }
  }
  return (
    left.rank - right.rank ||
    JSON.stringify(left.record).localeCompare(JSON.stringify(right.record))
  );
});
const epochEventsText = jsonl(
  epochEvents.map((entry, index) => ({
    sequence: index + 1,
    timestamp: epochEventTimestamp(entry),
    environment_id: environmentId,
    candidate_sha: candidateSha,
    ...entry.record,
  })),
);

// ---------------------------------------------------------------------------
// Topology and release manifest from both failure-domain container
// inventories.
// ---------------------------------------------------------------------------

const componentOfService = (service) => {
  if (service === "postgres") return "postgres";
  if (service === "transportd") return "transportd";
  if (service === "relay") return "relay";
  if (service.startsWith("agent-")) return "agent";
  return "control-plane";
};
const roleOfService = (service, failureDomain) => {
  if (service === "postgres") {
    return failureDomain === "fd-a" ? "postgres_standby" : "postgres_primary";
  }
  if (service.startsWith("agent-")) return "agent";
  return service;
};

// Only long-running production services become topology instances; the
// one-shot bootstrap and probe containers carry no G6 role.
const topologyServices = new Set([
  "postgres",
  "api",
  "worker",
  "scheduler",
  "transportd",
  "relay",
]);

function readInstances(path, failureDomain) {
  const instances = [];
  for (const line of readText(path).split("\n")) {
    if (line.length === 0) continue;
    const [rawName, image, startedAt, finishedAt, service] = line.split("\t");
    if (!service) fail(`malformed instance line in ${path}: ${line}`);
    const isAgent = /^agent-fd-[ab]-\d+$/.test(service);
    if (!isAgent && !topologyServices.has(service)) continue;
    const started = normalizeStamp(startedAt, `${service} StartedAt`);
    const stopped =
      finishedAt && !finishedAt.startsWith("0001-01-01")
        ? normalizeStamp(finishedAt, `${service} FinishedAt`)
        : undefined;
    instances.push({
      instance_id: service.startsWith("agent-")
        ? service
        : `${service}-${failureDomain}`,
      fault_domain: failureDomain === "fd-a" ? "fd-alpha" : "fd-beta",
      role: roleOfService(service, failureDomain),
      component: componentOfService(service),
      component_digest: image,
      started_at: started,
      ...(stopped ? { stopped_at: stopped } : {}),
    });
  }
  return instances;
}

const localAlias = readText(runDir, "state", "evidence", "failure-domain.txt");
const localFailureDomain = /^failure_domain=(.+)$/m.exec(localAlias)?.[1];
const peerAlias = readText(peerDir, "evidence", "failure-domain.txt");
const peerFailureDomain = /^failure_domain=(.+)$/m.exec(peerAlias)?.[1];
if (localFailureDomain !== "fd-b" || peerFailureDomain !== "fd-a") {
  fail("the evidence bundle must be assembled on fd-b from fd-a peer state");
}
const instances = [
  ...readInstances(join(peerDir, "evidence", "instances.tsv"), "fd-a"),
  ...readInstances(join(runDir, "state", "evidence", "instances.tsv"), "fd-b"),
];
const instanceIds = new Set(instances.map((instance) => instance.instance_id));
if (instanceIds.size !== instances.length) {
  fail("topology instance identifiers are not unique");
}

const componentDigests = new Map();
const componentDigestSources = new Map();
for (const instance of instances) {
  if (!/^sha256:[0-9a-f]{64}$/.test(instance.component_digest)) {
    fail(`instance ${instance.instance_id} has no image digest`);
  }
  const existing = componentDigests.get(instance.component);
  if (existing === undefined) {
    componentDigests.set(instance.component, instance.component_digest);
    componentDigestSources.set(instance.component, instance.instance_id);
  } else if (existing !== instance.component_digest) {
    fail(
      `component ${instance.component} has different digests across failure domains`,
      {
        path: `topology.instances#instance_id=${instance.instance_id}/component_digest`,
        expected: existing,
        actual: instance.component_digest,
        expected_instance_id: componentDigestSources.get(instance.component),
      },
    );
  }
}
for (const required of [
  "control-plane",
  "transportd",
  "relay",
  "postgres",
  "agent",
]) {
  if (!componentDigests.has(required)) {
    fail(`the topology has no instance for component ${required}`);
  }
}

const rejoinAt = normalizePreciseStamp(
  readText(peerDir, "rejoin-at").trim(),
  "rejoin-at",
);
const manifestText = canonicalJson({
  candidate_sha: candidateSha,
  components: [...componentDigests.entries()]
    .sort((left, right) => left[0].localeCompare(right[0]))
    .map(([name, digest]) => ({ name, digest })),
});
const topologyText = canonicalJson({
  schema_version: "ocservia.g6-topology.v1",
  candidate_sha: candidateSha,
  release_manifest_digest: sha256Digest(manifestText),
  environment_id: environmentId,
  failure_domain_class: failureDomainClass,
  instances: instances
    .map((instance) => ({
      ...instance,
      candidate_sha: candidateSha,
    }))
    .sort((left, right) => left.instance_id.localeCompare(right.instance_id)),
});

// ---------------------------------------------------------------------------
// The evidence window and the assembled bundle.
// ---------------------------------------------------------------------------

const resourceSamplesText = readText(runDir, "state", "resource-samples.csv");
const timelineText = readText(runDir, "outbox", "timeline.jsonl");
const pitrReportText = readText(peerDir, "pitr", "pitr-report.json");

const artifactFiles = [
  ["resource-samples.csv", "text/csv", "resource_samples", resourceSamplesText],
  ["timeline.jsonl", "application/x-ndjson", "timeline", timelineText],
  [
    "epoch-events.jsonl",
    "application/x-ndjson",
    "epoch_events",
    epochEventsText,
  ],
  [
    "authority-cut.json",
    "application/json",
    "authority_cut",
    authorityCutText,
  ],
  [
    "command-trace.jsonl",
    "application/x-ndjson",
    "command_trace",
    commandTraceText,
  ],
  [
    "outbox-snapshot.json",
    "application/json",
    "outbox_snapshot",
    outboxSnapshotText,
  ],
  ["http-samples.csv", "text/csv", "http_samples", httpSamplesText],
  [
    "telemetry-snapshot.json",
    "application/json",
    "telemetry_snapshot",
    telemetrySnapshotText,
  ],
  [
    "audit-correlation.json",
    "application/json",
    "audit_correlation",
    auditCorrelationText,
  ],
  [
    "postgres-recovery.json",
    "application/json",
    "postgres_recovery",
    postgresRecoveryText,
  ],
  ["pitr-report.json", "application/json", "pitr_report", pitrReportText],
  [
    "agent-sessions.json",
    "application/json",
    "agent_sessions",
    agentSessionsText,
  ],
  [
    "relay-transitions.jsonl",
    "application/x-ndjson",
    "relay_transitions",
    relayTransitionsText,
  ],
];

const timestampedStamps = [
  ...timelineRecords.map((event) => event.timestamp),
  ...readLog.map((sample) => sample.at),
  ...enqueueLog.map((sample) => sample.at),
  ...population.flatMap((command) => [command.created_at, command.updated_at]),
  ...outboxSnapshotRows.flatMap((row) => [row.created_at, row.due_at]),
  ...telemetryAgents.map((agent) => agent.last_telemetry_at),
  ...sessions.flatMap((session) => [
    session.session_started_at,
    session.reconnected_at,
  ]),
  ...acknowledged.map((marker) => marker.acknowledged_at),
  ...isolatedWrites.map((write) => write.at),
  ...epochEvents.map(epochEventTimestamp),
  ...traceRecords.map((entry) => preciseIsoOfMicros(entry.stampMicros)),
  outageDeclaredAt,
  isolatedAt,
  promotedAt,
  relayAFailedAt,
  relayTopologyReadyAt,
  relayBDisabledAt,
  relayBStartedAt,
  rejoinAt,
  pitrReport.marker_a.written_at,
  pitrReport.restore_point_created_at,
  pitrReport.marker_b.written_at,
  pitrReport.restore.restored_at,
  bulkDisconnectAt,
  snapshotTakenAt,
  windowEndedAt,
  finalFreezeAt,
  freezeReceivedAt,
  peerSnapshotTakenAt,
];
const timestampedNanoseconds = timestampedStamps.map((stamp) =>
  utcStampNanoseconds(stamp, "window stamp"),
);
const windowStartNs = timestampedNanoseconds.reduce((earliest, value) =>
  value < earliest ? value : earliest,
);
const windowEndNs = timestampedNanoseconds.reduce((latest, value) =>
  value > latest ? value : latest,
);
if (windowEndNs <= windowStartNs) fail("the evidence window is empty");
const startedAt = preciseIsoOfNanoseconds(windowStartNs);
const finishedAt = preciseIsoOfNanoseconds(windowEndNs);

const harnessSummary = [
  `run-id: ${runId}`,
  `environment-id: ${environmentId}`,
  `authority: ${authority}`,
  `failure-domain-class: ${failureDomainClass}`,
  `candidate-sha: ${candidateSha}`,
  `evidence-window: ${startedAt} .. ${finishedAt}`,
  `managed-nodes: ${nodes.length}`,
  `accepted-window-enqueues: ${okCommandIds.size}`,
  `run-wide-synthetic-commands: ${commands.length}`,
  `durable-agent-effects: ${effectsByCommandHex.size}`,
  `timeline-events: ${timelineRecords.length}`,
  `epoch-events: ${epochEvents.length}`,
  `topology-instances: ${instances.length}`,
  "",
].join("\n");

for (const [name, , , content] of artifactFiles) {
  writeFileSync(join(outDir, name), content);
}
writeFileSync(join(outDir, "harness-run-summary.log"), harnessSummary);

const artifacts = artifactFiles.map(([name, mediaType, kind]) => ({
  name,
  digest: sha256Digest(readFileSync(join(outDir, name), "utf8")),
  media_type: mediaType,
  kind,
}));
artifacts.push({
  name: "harness-run-summary.log",
  digest: sha256Digest(harnessSummary),
  media_type: "text/plain",
  kind: "harness_log",
});
const digestByKind = new Map(
  artifacts.map((artifact) => [artifact.kind, artifact.digest]),
);

const sloText = readText(values.slo);
const slo = parseSlo(sloText);

const derivations = computeG6Derivations({
  sloText,
  artifactEntries: artifactFiles.map(([name, , kind]) => ({
    name,
    kind,
    bytes: Buffer.from(readFileSync(join(outDir, name), "utf8"), "utf8"),
  })),
  environmentId,
  candidateSha,
  startedAt,
  finishedAt,
});

const measurements = {};
for (const [name, metric] of Object.entries(slo.metrics)) {
  const derived = derivations.get(metric.derivation);
  if (!derived) fail(`no derivation computed for metric ${name}`);
  measurements[name] = {
    actual: derived.value,
    sample_count: derived.sampleCount,
    source_artifact_digest: digestByKind.get(metric.derivation.split(".")[0]),
  };
}

const observations = {};
for (const [name, contract] of Object.entries(slo.observations)) {
  const missing = contract.required_timeline_events.filter(
    (eventId) => !timeline.has(eventId),
  );
  if (missing.length > 0) {
    fail(
      `observation ${name} is missing timeline events: ${missing.join(", ")}`,
    );
  }
  observations[name] = {
    observed: true,
    timeline_event_ids: contract.required_timeline_events,
    source_artifact_digest: digestByKind.get("timeline"),
  };
}

const evidence = canonicalJson({
  schema_version: "ocservia.g6-evidence.v2",
  candidate_sha: candidateSha,
  release_manifest_digest: sha256Digest(manifestText),
  slo_contract_digest: sha256Digest(sloText),
  topology_digest: sha256Digest(topologyText),
  started_at: startedAt,
  finished_at: finishedAt,
  environment: {
    environment_id: environmentId,
    failure_domain_class: failureDomainClass,
    authority,
    ...(authority === "engineering"
      ? {
          limitations: [
            "engineering rehearsal: the authority fence withholds the final G6 pass by design",
          ],
        }
      : {}),
  },
  measurements,
  observations,
  artifacts,
});
writeFileSync(join(outDir, "evidence.json"), evidence);
writeFileSync(join(outDir, "topology.json"), topologyText);
writeFileSync(join(outDir, "release-manifest.json"), manifestText);

process.stdout.write(
  `assembled ${artifactFiles.length} structured artifacts, topology, manifest, and evidence for ${environmentId}\n`,
);
