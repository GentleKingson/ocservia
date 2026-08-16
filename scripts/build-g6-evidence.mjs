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
import { join } from "node:path";
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

if (authority !== "engineering" && authority !== "production_readiness") {
  throw new Error("authority must be engineering or production_readiness");
}

function fail(message) {
  throw new Error(message);
}

function readText(...parts) {
  return readFileSync(join(...parts), "utf8");
}

function requireFile(...parts) {
  const path = join(...parts);
  try {
    if (readFileSync(path, "utf8").length > 0) return path;
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

function parseStamp(value, label) {
  const parsed = Date.parse(value);
  if (!Number.isFinite(parsed)) fail(`${label} is not a timestamp: ${value}`);
  return parsed;
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

function canonicalJson(value) {
  return `${JSON.stringify(value, null, 2)}\n`;
}

function jsonl(records) {
  return `${records.map((record) => JSON.stringify(record)).join("\n")}\n`;
}

// ---------------------------------------------------------------------------
// Inputs.
// ---------------------------------------------------------------------------

requireFile(runDir, "state", "read-log.jsonl");
requireFile(runDir, "state", "enqueue-log.jsonl");
requireFile(runDir, "state", "resource-samples.csv");
requireFile(runDir, "state", "era2-sessions.tsv");
requireFile(runDir, "state", "all-nodes.tsv");
requireFile(runDir, "state", "promoted-at");
requireFile(runDir, "state", "evidence", "final-sessions.json");
requireFile(runDir, "state", "evidence", "snapshot-taken-at");
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
requireFile(peerDir, "isolation", "isolation.json");
requireFile(peerDir, "isolation", "active-load.json");
requireFile(peerDir, "isolation", "outage-declared-at");
requireFile(peerDir, "isolation", "isolated-at");
requireFile(peerDir, "isolation", "isolated-primary-writes.jsonl");
requireFile(peerDir, "pitr", "pitr-report.json");
requireFile(peerDir, "evidence", "instances.tsv");
requireFile(peerDir, "relay-a-failed-at");
requireFile(peerDir, "rejoin-at");
requireFile(peerDir, "post-rejoin-probes.jsonl");

const readLog = readJSONL(runDir, "state", "read-log.jsonl");
const enqueueLog = readJSONL(runDir, "state", "enqueue-log.jsonl");
const commands = readJSONL(runDir, "state", "evidence", "commands.jsonl");
const attempts = readJSONL(runDir, "state", "evidence", "attempts.jsonl");
const outboxRows = readJSONL(runDir, "state", "evidence", "outbox.jsonl");
const auditRows = readJSONL(runDir, "state", "evidence", "audit.jsonl");
const telemetryRows = readJSONL(runDir, "state", "evidence", "telemetry.jsonl");
const markerRows = readJSONL(runDir, "state", "evidence", "markers.jsonl");
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
const finalSessions = JSON.parse(
  readText(runDir, "state", "evidence", "final-sessions.json"),
);
const snapshotTakenAt = normalizeStamp(
  readText(runDir, "state", "evidence", "snapshot-taken-at").trim(),
  "snapshot-taken-at",
);
const promotedAt = normalizeStamp(
  readText(runDir, "state", "promoted-at").trim(),
  "promoted-at",
);
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
const rejoinBoundaryAt = normalizeStamp(
  readText(peerDir, "rejoin-at").trim(),
  "rejoin-at",
);
const rejoinProbes = readJSONL(peerDir, "post-rejoin-probes.jsonl");
if (
  rejoinProbes.length < 3 ||
  rejoinProbes.some(
    (probe) =>
      probe.accepted !== false ||
      parseStamp(probe.at, "post-rejoin probe") <
        parseStamp(rejoinBoundaryAt, "rejoin"),
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
const relayAFailedAt = normalizeStamp(
  readText(peerDir, "relay-a-failed-at").trim(),
  "relay-a-failed-at",
);
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
// HTTP samples: the bounded-window measurement population. Accepted
// enqueues carry the command id as the request id; failed attempts carry
// the idempotency key so the success ratio keeps an honest denominator.
// ---------------------------------------------------------------------------

const okCommandIds = new Set();
const httpLines = [
  "timestamp,kind,status,latency_seconds,request_id,environment_id,candidate_sha",
];
const latencyText = (value) => {
  if (typeof value !== "number" || !Number.isFinite(value) || value < 0) {
    fail(`an http latency sample is not a non-negative number: ${value}`);
  }
  // fixed-point form so no sample ever reaches the CSV in exponent notation
  return value.toFixed(6);
};
for (const sample of readLog) {
  httpLines.push(
    [
      sample.at,
      "read",
      sample.status === 200 ? "ok" : "error",
      latencyText(sample.latency_seconds),
      "",
      environmentId,
      candidateSha,
    ].join(","),
  );
}
for (const sample of enqueueLog) {
  const ok =
    sample.status >= 200 && sample.status < 300 && sample.command_id !== "";
  if (ok) okCommandIds.add(sample.command_id);
  httpLines.push(
    [
      sample.at,
      "enqueue",
      ok ? "ok" : "error",
      latencyText(sample.latency_seconds),
      ok ? sample.command_id : sample.idempotency_key,
      environmentId,
      candidateSha,
    ].join(","),
  );
}
if (okCommandIds.size === 0) fail("no accepted enqueue was recorded");
const httpSamplesText = `${httpLines.join("\n")}\n`;

// ---------------------------------------------------------------------------
// Command trace, outbox snapshot, and audit correlation over the same
// accepted-write population.
// ---------------------------------------------------------------------------

const commandById = new Map(commands.map((command) => [command.id, command]));
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
  population.push(command);
}
population.sort((left, right) =>
  left.created_at === right.created_at
    ? left.id.localeCompare(right.id)
    : left.created_at.localeCompare(right.created_at),
);

const firstAttemptByCommand = new Map();
for (const attempt of attempts) {
  const current = firstAttemptByCommand.get(attempt.command_id);
  if (!current || attempt.attempt_number < current.attempt_number) {
    firstAttemptByCommand.set(attempt.command_id, attempt);
  }
}

// journal effects keyed by the binary command id (hex, dashes stripped)
const effectsByCommandHex = new Map();
const effectsDirectory = join(runDir, "state", "evidence", "effects");
for (const entry of readdirSync(effectsDirectory)) {
  if (!entry.endsWith(".tsv")) continue;
  const service = entry.replace(/\.tsv$/, "");
  for (const line of readText(effectsDirectory, entry).split("\n")) {
    if (line.length === 0) continue;
    const [keyHex, commandHex, executedAt] = line.trim().split(/\s+/);
    if (!keyHex || !commandHex || !executedAt) {
      fail(`malformed effect record in ${entry}: ${line}`);
    }
    if (effectsByCommandHex.has(commandHex)) {
      fail(`two agents hold an effect for command hex ${commandHex}`);
    }
    effectsByCommandHex.set(commandHex, {
      keyHex,
      effectId: `fx-${service}-${keyHex}`,
      executedAt: `${new Date(Number(executedAt) * 1000)
        .toISOString()
        .slice(0, 19)}Z`,
    });
  }
}

const outcomeOf = (state) => {
  if (state === "succeeded") return "success";
  if (state === "failed") return "failed";
  if (state === "unknown") return "unknown";
  return fail(`command state ${state} has no trace outcome mapping`);
};

const traceRecords = [];
const traceBasetime = population[0].created_at;
traceRecords.push({
  stampMs: parseStamp(traceBasetime, "trace baseline"),
  rank: 0,
  record: {
    record_type: "profile",
    dispatch_bound_seconds: 10,
  },
});
for (const command of population) {
  const commandHex = command.id.replaceAll("-", "");
  const enqueuedMs = parseStamp(command.created_at, "command created_at");
  traceRecords.push({
    stampMs: enqueuedMs,
    rank: 1,
    record: { record_type: "enqueued", command_id: command.id },
  });
  const attempt = firstAttemptByCommand.get(command.id);
  if (attempt) {
    traceRecords.push({
      stampMs: parseStamp(attempt.started_at, "attempt started_at"),
      rank: 2,
      record: { record_type: "dispatched", command_id: command.id },
    });
  }
  const effect = effectsByCommandHex.get(commandHex);
  if (command.state === "succeeded" && !effect) {
    fail(`successful synthetic command ${command.id} has no durable effect`);
  }
  if (effect) {
    const effectMs = parseStamp(effect.executedAt, "effect executed_at");
    if (effectMs < enqueuedMs) {
      fail(`effect for ${command.id} predates its enqueue`);
    }
    traceRecords.push({
      stampMs: effectMs,
      rank: 3,
      record: {
        record_type: "effect",
        idempotency_key: effect.keyHex,
        effect_id: effect.effectId,
      },
    });
  }
  traceRecords.push({
    stampMs: parseStamp(command.updated_at, "command updated_at"),
    rank: 4,
    record: {
      record_type: "result",
      command_id: command.id,
      outcome: outcomeOf(command.state),
    },
  });
}
for (const commandHex of effectsByCommandHex.keys()) {
  if (
    !population.some((command) => command.id.replaceAll("-", "") === commandHex)
  ) {
    fail(
      `durable effect ${commandHex} does not match an accepted synthetic command`,
    );
  }
}
traceRecords.sort(
  (left, right) =>
    left.stampMs - right.stampMs ||
    left.rank - right.rank ||
    JSON.stringify(left.record).localeCompare(JSON.stringify(right.record)),
);
const commandTraceText = jsonl(
  traceRecords.map((entry, index) => ({
    sequence: index + 1,
    timestamp: isoOf(entry.stampMs),
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
    result: false,
  };
  if (row.result === "intent") current.intent = true;
  if (row.result === "succeeded" || row.result === "failed") {
    current.result = true;
  }
  auditByCommand.set(row.command_id, current);
}
const auditWrites = [];
for (const command of population) {
  const record = auditByCommand.get(command.id);
  auditWrites.push({
    write_id: command.id,
    intent_recorded: record?.intent === true,
    result_recorded: record?.result === true,
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

const finalByNode = new Map(
  finalSessions.observations.map((observation) => [
    observation.node_id,
    observation,
  ]),
);
const bulkDisconnectAt = timelineStamp("bulk_disconnect_injected");
const sessions = [];
for (const node of nodes) {
  const started = era2Sessions.get(node.nodeId);
  const final = finalByNode.get(node.nodeId);
  if (!started) fail(`no era-2 session start for node ${node.nodeId}`);
  if (!final?.found) fail(`no final session for node ${node.nodeId}`);
  const startedAt = normalizeStamp(started, "era-2 session start");
  const reconnectedAt = normalizeStamp(
    final.connected_at,
    "final session connected_at",
  );
  if (
    parseStamp(startedAt, "session start") >=
    parseStamp(bulkDisconnectAt, "storm")
  ) {
    fail(`session for ${node.name} does not predate the reconnect storm`);
  }
  if (
    parseStamp(reconnectedAt, "reconnect") <
    parseStamp(bulkDisconnectAt, "storm")
  ) {
    fail(`session for ${node.name} reconnects before the storm`);
  }
  sessions.push({
    agent_id: node.name,
    node: node.nodeId,
    authorized: true,
    connected: true,
    session_started_at: startedAt,
    reconnected_at: reconnectedAt,
  });
}
const agentSessionsText = canonicalJson({
  environment_id: environmentId,
  candidate_sha: candidateSha,
  snapshot_taken_at: snapshotTakenAt,
  sessions,
  reconnect_storm: { bulk_disconnect_at: bulkDisconnectAt },
});

// ---------------------------------------------------------------------------
// PostgreSQL recovery and relay transitions.
// ---------------------------------------------------------------------------

const outageDeclaredAt = normalizeStamp(
  readText(peerDir, "isolation", "outage-declared-at").trim(),
  "outage-declared-at",
);
const isolatedAt = normalizeStamp(
  readText(peerDir, "isolation", "isolated-at").trim(),
  "isolated-at",
);
const outageMs = parseStamp(outageDeclaredAt, "outage");
const activeLoadCapturedMs = parseStamp(
  activeLoad.captured_at,
  "active-load capture",
);
if (activeLoadCapturedMs > outageMs) {
  fail("the active-load snapshot was captured after the database outage");
}
for (const command of activeLoad.commands) {
  const telemetryMs = parseStamp(
    command.last_telemetry_at,
    "active-load telemetry",
  );
  if (
    telemetryMs > activeLoadCapturedMs ||
    activeLoadCapturedMs - telemetryMs > 90_000
  ) {
    fail(
      `active-load telemetry for ${command.command_id} is not live at failure injection`,
    );
  }
}
const promotedMs = parseStamp(promotedAt, "promotion");
if (
  isolatedWrites.length < 3 ||
  isolatedWrites.some(
    (write) =>
      write.accepted !== false ||
      parseStamp(write.at, "dual-primary probe") < promotedMs,
  )
) {
  fail("former-primary write probes do not prove fencing after promotion");
}
const acknowledged = [];
const presentTxids = [];
for (const marker of markerRows) {
  if (marker.id !== "pitr-marker-a" && marker.id !== "pitr-marker-b") continue;
  if (parseStamp(marker.written_at, "marker") >= outageMs) {
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

const relayObservation = finalByNode.get(
  nodes.find((node) => node.name.startsWith("g6-fd-a-"))?.nodeId ?? "",
);
if (!relayObservation) fail("no cross-failure-domain agent for relay evidence");
const relayTransitionsText = jsonl([
  {
    sequence: 1,
    timestamp: relayAFailedAt,
    environment_id: environmentId,
    candidate_sha: candidateSha,
    event_type: "relay_failed",
    relay: "relay-a",
  },
  {
    sequence: 2,
    timestamp: timelineStamp("relay_b_active"),
    environment_id: environmentId,
    candidate_sha: candidateSha,
    event_type: "path_active",
    session_id: relayObservation.node_id,
    path: "relay",
    relay: "relay-b",
    authenticated: true,
  },
]);

// ---------------------------------------------------------------------------
// Epoch events from the authoritative fencing and leadership history.
// Each watcher line is one full-table snapshot row; a per-node epoch change
// between consecutive sightings records the predecessor's lease expiry and
// the successor's registration. Renewal sightings carry the lease-bound
// evidence for scheduler commits.
// ---------------------------------------------------------------------------

const epochEvents = [];
const rankOf = { expired: 0, registered: 1, acquired: 1, commit: 2, accept: 3 };

// Watcher lines end in two RFC 3339 stamps whose colons forbid a naive
// split; anchor on the trailing timestamps and split the leading fields.
const trailingStamps =
  /^(.*):(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z):(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z)$/;

const ownerLines = readText(runDir, "outbox", "fencing-history.jsonl")
  .split("\n")
  .filter((line) => line.length > 0);
const ownerState = new Map();
for (const line of ownerLines) {
  const match = trailingStamps.exec(line);
  const head = match?.[1]?.split(":") ?? [];
  if (!match || head.length !== 5) {
    fail(`malformed fencing history line: ${line}`);
  }
  const [nodeHex, instance, , , epochText] = head;
  const leaseUntil = match[2];
  const updatedAt = match[3];
  const epoch = Number(epochText);
  if (!Number.isInteger(epoch) || epoch < 1) {
    fail(`fencing history carries an invalid epoch: ${line}`);
  }
  const current = ownerState.get(nodeHex);
  if (!current) {
    epochEvents.push({
      stampMs: parseStamp(updatedAt, "fencing updated_at"),
      rank: rankOf.registered,
      record: {
        subject: "connection_owner",
        event_type: "owner_registered",
        node: nodeHex,
        instance,
        epoch,
      },
    });
    ownerState.set(nodeHex, { epoch, leaseUntil, instance });
  } else if (current.epoch !== epoch) {
    epochEvents.push({
      stampMs: parseStamp(current.leaseUntil, "fencing lease_until"),
      rank: rankOf.expired,
      record: {
        subject: "connection_owner",
        event_type: "owner_lease_expired",
        node: nodeHex,
        epoch: current.epoch,
      },
    });
    epochEvents.push({
      stampMs: parseStamp(updatedAt, "fencing updated_at"),
      rank: rankOf.registered,
      record: {
        subject: "connection_owner",
        event_type: "owner_registered",
        node: nodeHex,
        instance,
        epoch,
      },
    });
    ownerState.set(nodeHex, { epoch, leaseUntil, instance });
  } else {
    current.leaseUntil = leaseUntil;
    current.instance = instance;
  }
}

const [staleNodeHex, staleInstance, , , staleEpochText] = ownerTerms[0];
if (staleTransportProbe.status !== "rejected") {
  fail("the stale-transport probe did not record a rejection");
}
if (staleAgentProbe.status !== "rejected") {
  fail("the stale-agent probe did not record a rejection");
}
epochEvents.push({
  stampMs: parseStamp(timelineStamp("stale_transport_rejected"), "timeline"),
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
epochEvents.push({
  stampMs: parseStamp(timelineStamp("stale_agent_rejected"), "timeline"),
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

const leaderLines = readText(runDir, "outbox", "leadership-history.jsonl")
  .split("\n")
  .filter((line) => line.length > 0);
let leaderCurrent = null;
for (const line of leaderLines) {
  const match = trailingStamps.exec(line);
  const head = match?.[1]?.split(":") ?? [];
  if (!match || head.length !== 3) {
    fail(`malformed leadership history line: ${line}`);
  }
  const [instance, , epochText] = head;
  const leaseUntil = match[2];
  const updatedAt = match[3];
  const epoch = Number(epochText);
  if (!Number.isInteger(epoch) || epoch < 1) {
    fail(`leadership history carries an invalid epoch: ${line}`);
  }
  if (!leaderCurrent) {
    leaderCurrent = { instance, epoch, leaseUntil, lastCommitAt: updatedAt };
    epochEvents.push({
      stampMs: parseStamp(updatedAt, "leadership updated_at"),
      rank: rankOf.acquired,
      record: {
        subject: "scheduler",
        event_type: "leader_acquired",
        instance,
        epoch,
      },
    });
  } else if (leaderCurrent.epoch !== epoch) {
    epochEvents.push({
      stampMs: parseStamp(leaderCurrent.leaseUntil, "leadership lease_until"),
      rank: rankOf.expired,
      record: {
        subject: "scheduler",
        event_type: "leader_lease_expired",
        epoch: leaderCurrent.epoch,
      },
    });
    epochEvents.push({
      stampMs: parseStamp(leaderCurrent.lastCommitAt, "leadership renewal"),
      rank: rankOf.commit,
      record: {
        subject: "scheduler",
        event_type: "leader_commit",
        instance: leaderCurrent.instance,
        epoch: leaderCurrent.epoch,
        accepted: true,
      },
    });
    leaderCurrent = { instance, epoch, leaseUntil, lastCommitAt: updatedAt };
    epochEvents.push({
      stampMs: parseStamp(updatedAt, "leadership updated_at"),
      rank: rankOf.acquired,
      record: {
        subject: "scheduler",
        event_type: "leader_acquired",
        instance,
        epoch,
      },
    });
  } else {
    leaderCurrent.leaseUntil = leaseUntil;
    leaderCurrent.lastCommitAt = updatedAt;
  }
}
if (leaderCurrent) {
  epochEvents.push({
    stampMs: parseStamp(leaderCurrent.lastCommitAt, "leadership renewal"),
    rank: rankOf.commit,
    record: {
      subject: "scheduler",
      event_type: "leader_commit",
      instance: leaderCurrent.instance,
      epoch: leaderCurrent.epoch,
      accepted: true,
    },
  });
}
const staleTerm = readText(runDir, "state", "stale-scheduler-term").trim();
const [staleLeaderInstance, , staleLeaderEpochText] = staleTerm.split(":");
epochEvents.push({
  stampMs: parseStamp(
    timelineStamp("stale_scheduler_commit_rejected"),
    "timeline",
  ),
  rank: rankOf.accept,
  record: {
    subject: "scheduler",
    event_type: "leader_commit",
    instance: staleLeaderInstance,
    epoch: Number(staleLeaderEpochText),
    accepted: false,
  },
});

epochEvents.sort(
  (left, right) =>
    left.stampMs - right.stampMs ||
    left.rank - right.rank ||
    JSON.stringify(left.record).localeCompare(JSON.stringify(right.record)),
);
const epochEventsText = jsonl(
  epochEvents.map((entry, index) => ({
    sequence: index + 1,
    timestamp: isoOf(entry.stampMs),
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
for (const instance of instances) {
  if (!/^sha256:[0-9a-f]{64}$/.test(instance.component_digest)) {
    fail(`instance ${instance.instance_id} has no image digest`);
  }
  const existing = componentDigests.get(instance.component);
  if (existing === undefined) {
    componentDigests.set(instance.component, instance.component_digest);
  } else if (existing !== instance.component_digest) {
    fail(
      `component ${instance.component} has different digests across failure domains`,
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

const rejoinAt = normalizeStamp(
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
  ...epochEvents.map((entry) => isoOf(entry.stampMs)),
  ...traceRecords.map((entry) => isoOf(entry.stampMs)),
  outageDeclaredAt,
  isolatedAt,
  promotedAt,
  relayAFailedAt,
  rejoinAt,
  pitrReport.marker_a.written_at,
  pitrReport.restore_point_created_at,
  pitrReport.marker_b.written_at,
  pitrReport.restore.restored_at,
  bulkDisconnectAt,
  snapshotTakenAt,
];
const windowStartMs = Math.min(
  ...timestampedStamps.map((stamp) => parseStamp(stamp, "window stamp")),
);
const windowEndMs = Math.max(
  ...timestampedStamps.map((stamp) => parseStamp(stamp, "window stamp")),
);
if (windowEndMs <= windowStartMs) fail("the evidence window is empty");
const startedAt = isoOf(windowStartMs);
const finishedAt = isoOf(windowEndMs);

const harnessSummary = [
  `run-id: ${runId}`,
  `environment-id: ${environmentId}`,
  `authority: ${authority}`,
  `failure-domain-class: ${failureDomainClass}`,
  `candidate-sha: ${candidateSha}`,
  `evidence-window: ${startedAt} .. ${finishedAt}`,
  `managed-nodes: ${nodes.length}`,
  `accepted-window-enqueues: ${okCommandIds.size}`,
  `timeline-events: ${timelineRecords.length}`,
  `epoch-events: ${epochEvents.length}`,
  `topology-instances: ${instances.length}`,
  "",
].join("\n");

mkdirSync(outDir, { recursive: true });
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

const sloText = readFileSync(values.slo, "utf8");
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
