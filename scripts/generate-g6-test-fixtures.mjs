#!/usr/bin/env node

// Deterministically regenerates the public G6 verifier fixtures (the artifact
// bundle and the passing evidence document) from a fixed scenario. The
// measurement values and sample counts written into evidence-pass.json are
// computed by the verifier library itself (computeG6Derivations), so the
// positive fixture can never drift from the derivation contract.
//
//   node scripts/generate-g6-test-fixtures.mjs          rewrite fixtures
//   node scripts/generate-g6-test-fixtures.mjs --check  fail on any drift

import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import {
  computeG6Derivations,
  parseSlo,
  sha256Digest,
} from "./g6-contract-lib.mjs";

const root = fileURLToPath(new URL("../", import.meta.url));
const read = (path) => readFileSync(join(root, path), "utf8");
const artifactDirectory = join(root, "testdata/g6/artifacts");

const environmentId = "g6-12345678";
const candidateSha = "1111111111111111111111111111111111111111";
const startedAt = "2026-08-14T00:00:00Z";
const finishedAt = "2026-08-14T00:05:01Z";

function at(seconds) {
  const hh = String(Math.floor(seconds / 3600)).padStart(2, "0");
  const mm = String(Math.floor((seconds % 3600) / 60)).padStart(2, "0");
  const ss = String(seconds % 60).padStart(2, "0");
  return `2026-08-14T${hh}:${mm}:${ss}Z`;
}

const bindingFields = `${environmentId},${candidateSha}`;
const bindingRecord = (timestamp) => ({
  timestamp,
  environment_id: environmentId,
  candidate_sha: candidateSha,
});

function jsonl(records) {
  return `${records.map((r) => JSON.stringify(r)).join("\n")}\n`;
}

function canonicalJson(value) {
  return `${JSON.stringify(value, null, 2)}\n`;
}

function buildResourceSamples() {
  const header = `timestamp,component,instance,rss_bytes,fd_count,tasks,queue_depth,db_connections,environment_id,candidate_sha`;
  const lines = [header];
  for (let tick = 0; tick <= 60; tick += 1) {
    const timestamp = at(tick * 5);
    lines.push(
      `${timestamp},controller,api-1,${104857600 + tick * 1024},${48 + Math.floor(tick / 15)},${120 + Math.floor(tick / 5)},0,,${bindingFields}`,
    );
    lines.push(
      `${timestamp},transportd,node-1,${104857600 + tick * 2048},${40 + Math.floor(tick / 20)},${200 + Math.floor(tick / 8)},0,,${bindingFields}`,
    );
    lines.push(
      `${timestamp},agent,node-agent-03,${52428800 + tick * 512},${24 + Math.floor(tick / 30)},${60 + Math.floor(tick / 15)},0,,${bindingFields}`,
    );
    lines.push(
      `${timestamp},postgres,pg-primary,${209715200 + tick * 4096},80,40,0,${40 + Math.floor(tick / 30)},${bindingFields}`,
    );
  }
  return `${lines.join("\n")}\n`;
}

const timelineEventIds = [
  "load_started",
  "primary_failure_injected",
  "new_primary_writable",
  "old_primary_isolated",
  "new_primary_promoted",
  "old_primary_write_rejected",
  "marker_a_written",
  "restore_point_created",
  "marker_b_written",
  "restore_verified",
  "owner_a_paused",
  "owner_b_acquired",
  "owner_a_resumed",
  "stale_transport_rejected",
  "stale_agent_rejected",
  "scheduler_a_paused",
  "scheduler_b_acquired",
  "scheduler_a_resumed",
  "stale_scheduler_commit_rejected",
  "api_instance_failed",
  "gateway_traffic_transferred",
  "api_slo_measured",
  "worker_instance_failed",
  "worker_replacement_active",
  "dispatch_recovered",
  "relay_a_failed",
  "relay_b_active",
  "direct_path_active",
  "direct_path_failed",
  "relay_path_active",
  "direct_path_recovered",
  "bulk_disconnect_injected",
  "reconnect_started",
  "reconnect_completed",
  "outbox_claim_committed",
  "worker_crashed_before_send",
  "command_recovered",
  "transport_send_accepted",
  "worker_crashed_before_mark_sent",
  "command_reconciled",
  "result_received",
  "ingress_crashed_before_commit",
  "result_reconciled",
  "api_recovered",
  "worker_recovered",
  "load_stopped",
];

function buildTimeline() {
  return jsonl(
    timelineEventIds.map((event_id, index) => ({
      event_id,
      sequence: index + 1,
      ...bindingRecord(at(index)),
    })),
  );
}

function buildEpochEvents() {
  const records = [
    { sequence: 1, ...bindingRecord(at(0)), subject: "connection_owner", event_type: "owner_registered", node: "node-1", instance: "worker-a", epoch: 1 },
    { sequence: 2, ...bindingRecord(at(5)), subject: "connection_owner", event_type: "owner_accept", node: "node-1", instance: "worker-a", epoch: 1, accepted: true },
    { sequence: 3, ...bindingRecord(at(10)), subject: "connection_owner", event_type: "owner_lease_expired", node: "node-1", epoch: 1 },
    { sequence: 4, ...bindingRecord(at(20)), subject: "connection_owner", event_type: "owner_registered", node: "node-1", instance: "worker-b", epoch: 2 },
    { sequence: 5, ...bindingRecord(at(25)), subject: "connection_owner", event_type: "owner_accept", node: "node-1", instance: "worker-b", epoch: 2, accepted: true },
    { sequence: 6, ...bindingRecord(at(30)), subject: "connection_owner", event_type: "owner_accept", node: "node-1", instance: "worker-a", epoch: 1, accepted: false },
    { sequence: 7, ...bindingRecord(at(60)), subject: "scheduler", event_type: "leader_acquired", instance: "sched-a", epoch: 1 },
    { sequence: 8, ...bindingRecord(at(65)), subject: "scheduler", event_type: "leader_commit", instance: "sched-a", epoch: 1, accepted: true },
    { sequence: 9, ...bindingRecord(at(75)), subject: "scheduler", event_type: "leader_lease_expired", epoch: 1 },
    { sequence: 10, ...bindingRecord(at(76)), subject: "scheduler", event_type: "leader_acquired", instance: "sched-b", epoch: 2 },
    { sequence: 11, ...bindingRecord(at(85)), subject: "scheduler", event_type: "leader_commit", instance: "sched-b", epoch: 2, accepted: true },
    { sequence: 12, ...bindingRecord(at(86)), subject: "scheduler", event_type: "leader_commit", instance: "sched-a", epoch: 1, accepted: false },
  ];
  return jsonl(records);
}

function buildCommandTrace() {
  const commands = Array.from({ length: 50 }, (_, index) =>
    String(index + 1).padStart(3, "0"),
  );
  const records = [
    {
      sequence: 1,
      ...bindingRecord(at(0)),
      record_type: "profile",
      dispatch_bound_seconds: 10,
    },
  ];
  let sequence = 1;
  for (const suffix of commands) {
    sequence += 1;
    records.push({ sequence, ...bindingRecord(at(0)), record_type: "enqueued", command_id: `cmd-${suffix}` });
  }
  for (const suffix of commands) {
    sequence += 1;
    records.push({ sequence, ...bindingRecord(at(1)), record_type: "dispatched", command_id: `cmd-${suffix}` });
  }
  for (const suffix of commands) {
    sequence += 1;
    records.push({
      sequence,
      ...bindingRecord(at(2)),
      record_type: "effect",
      idempotency_key: `idem-${suffix}`,
      effect_id: `fx-${suffix}`,
    });
  }
  for (const [index, suffix] of commands.entries()) {
    sequence += 1;
    records.push({
      sequence,
      ...bindingRecord(at(4 + Math.floor(index / 10))),
      record_type: "result",
      command_id: `cmd-${suffix}`,
      outcome: "success",
    });
  }
  return jsonl(records);
}

function buildOutboxSnapshot() {
  const rows = Array.from({ length: 50 }, (_, index) => {
    const suffix = String(index + 1).padStart(3, "0");
    const state =
      index >= 49 ? "unknown_reconciling" : index >= 47 ? "reconciliation_active" : "terminal";
    return {
      command_id: `cmd-${suffix}`,
      created_at: at(0),
      due_at: at(10),
      state,
    };
  });
  return canonicalJson({
    environment_id: environmentId,
    candidate_sha: candidateSha,
    snapshot_taken_at: at(300),
    rows,
  });
}

function buildHttpSamples() {
  const header = "timestamp,kind,status,latency_seconds,environment_id,candidate_sha";
  const lines = [header];
  const latency = (millis) => String(millis / 1000);
  let enqueueCounter = 0;
  for (let second = 0; second < 300; second += 1) {
    const timestamp = at(second);
    for (let sub = 0; sub < 2; sub += 1) {
      lines.push(`${timestamp},read,ok,${latency(10 + ((second * 2 + sub) % 7))},${bindingFields}`);
      lines.push(
        `${timestamp},enqueue,ok,${latency(50 + (enqueueCounter % 10) * 10)},${bindingFields}`,
      );
      enqueueCounter += 1;
    }
  }
  return `${lines.join("\n")}\n`;
}

function buildTelemetrySnapshot() {
  return canonicalJson({
    environment_id: environmentId,
    candidate_sha: candidateSha,
    snapshot_taken_at: at(270),
    freshness_bound_seconds: 60,
    agents: Array.from({ length: 50 }, (_, index) => ({
      agent_id: `agent-${String(index + 1).padStart(2, "0")}`,
      last_telemetry_at: at(270 - 10 - index),
    })),
  });
}

function buildAuditCorrelation() {
  return canonicalJson({
    environment_id: environmentId,
    candidate_sha: candidateSha,
    writes: Array.from({ length: 200 }, (_, index) => ({
      write_id: `wr-${String(index + 1).padStart(3, "0")}`,
      intent_recorded: true,
      result_recorded: true,
    })),
  });
}

function buildPostgresRecovery() {
  return canonicalJson({
    environment_id: environmentId,
    candidate_sha: candidateSha,
    outage_declared_at: at(60),
    service_restored_at: at(120),
    acknowledged: [
      { txid: "tx-m1", acknowledged_at: at(40) },
      { txid: "tx-m2", acknowledged_at: at(50) },
      { txid: "tx-m3", acknowledged_at: at(55) },
    ],
    failover: {
      old_primary: "pg-primary",
      new_primary: "pg-standby-1",
      isolated_at: at(65),
      promoted_at: at(90),
      isolated_primary_writes: [
        { at: at(105), accepted: false },
        { at: at(110), accepted: false },
      ],
    },
    recovery: {
      restored_at: at(150),
      present_txids: ["tx-m1", "tx-m2", "tx-m3"],
    },
  });
}

function buildPitrReport() {
  return canonicalJson({
    environment_id: environmentId,
    candidate_sha: candidateSha,
    marker_a: { txid: "pitr-marker-a", written_at: at(10) },
    restore_point_created_at: at(20),
    marker_b: { txid: "pitr-marker-b", written_at: at(30) },
    restore: {
      restored_at: at(180),
      marker_a_present: true,
      marker_b_present: false,
    },
  });
}

function buildAgentSessions() {
  return canonicalJson({
    environment_id: environmentId,
    candidate_sha: candidateSha,
    snapshot_taken_at: at(270),
    sessions: Array.from({ length: 50 }, (_, index) => ({
      agent_id: `agent-${String(index + 1).padStart(2, "0")}`,
      node: `node-${(index % 5) + 1}`,
      authorized: true,
      connected: true,
      session_started_at: at(0),
    })),
    reconnect_storm: {
      bulk_disconnect_at: at(180),
      reconnect_completed_at: at(210),
    },
  });
}

function buildRelayTransitions() {
  const records = [
    { sequence: 1, ...bindingRecord(at(150)), event_type: "path_active", session_id: "s-001", path: "direct" },
    { sequence: 2, ...bindingRecord(at(155)), event_type: "path_failed", session_id: "s-001", path: "direct" },
    { sequence: 3, ...bindingRecord(at(160)), event_type: "relay_failed", relay: "relay-a" },
    { sequence: 4, ...bindingRecord(at(165)), event_type: "path_active", session_id: "s-001", path: "relay" },
    { sequence: 5, ...bindingRecord(at(170)), event_type: "relay_active", relay: "relay-b" },
    { sequence: 6, ...bindingRecord(at(190)), event_type: "path_failed", session_id: "s-001", path: "relay" },
    { sequence: 7, ...bindingRecord(at(195)), event_type: "path_active", session_id: "s-001", path: "direct" },
  ];
  return jsonl(records);
}

const artifactFiles = [
  ["resource-samples.csv", "text/csv", "resource_samples", buildResourceSamples()],
  ["timeline.jsonl", "application/x-ndjson", "timeline", buildTimeline()],
  ["epoch-events.jsonl", "application/x-ndjson", "epoch_events", buildEpochEvents()],
  ["command-trace.jsonl", "application/x-ndjson", "command_trace", buildCommandTrace()],
  ["outbox-snapshot.json", "application/json", "outbox_snapshot", buildOutboxSnapshot()],
  ["http-samples.csv", "text/csv", "http_samples", buildHttpSamples()],
  ["telemetry-snapshot.json", "application/json", "telemetry_snapshot", buildTelemetrySnapshot()],
  ["audit-correlation.json", "application/json", "audit_correlation", buildAuditCorrelation()],
  ["postgres-recovery.json", "application/json", "postgres_recovery", buildPostgresRecovery()],
  ["pitr-report.json", "application/json", "pitr_report", buildPitrReport()],
  ["agent-sessions.json", "application/json", "agent_sessions", buildAgentSessions()],
  ["relay-transitions.jsonl", "application/x-ndjson", "relay_transitions", buildRelayTransitions()],
];

function buildEvidence() {
  const sloText = read("docs/acceptance/g6-slo.yaml");
  const manifestText = read("testdata/g6/release-manifest.json");
  const topologyText = `${JSON.stringify(JSON.parse(read("testdata/g6/topology.json")), null, 2)}\n`;

  const artifacts = artifactFiles.map(([name, mediaType, kind, content]) => ({
    name,
    digest: sha256Digest(content),
    media_type: mediaType,
    kind,
  }));
  const digestByKind = new Map(artifacts.map((a) => [a.kind, a.digest]));

  const derivations = computeG6Derivations({
    sloText,
    artifactEntries: artifactFiles.map(([name, , kind, content]) => ({
      name,
      kind,
      bytes: Buffer.from(content, "utf8"),
    })),
    environmentId,
    candidateSha,
    startedAt,
    finishedAt,
  });

  const slo = parseSlo(sloText);
  const measurements = {};
  for (const [name, metric] of Object.entries(slo.metrics)) {
    const derived = derivations.get(metric.derivation);
    measurements[name] = {
      actual: derived.value,
      sample_count: derived.sampleCount,
      source_artifact_digest: digestByKind.get(
        derivationKind(metric.derivation),
      ),
    };
  }

  const observations = {};
  for (const [name, observation] of Object.entries(slo.observations)) {
    observations[name] = {
      observed: true,
      timeline_event_ids: observation.required_timeline_events,
      source_artifact_digest: digestByKind.get("timeline"),
    };
  }

  return canonicalJson({
    schema_version: "ocservia.g6-evidence.v2",
    candidate_sha: candidateSha,
    release_manifest_digest: sha256Digest(manifestText),
    slo_contract_digest: sha256Digest(sloText),
    topology_digest: sha256Digest(topologyText),
    started_at: startedAt,
    finished_at: finishedAt,
    environment: {
      environment_id: environmentId,
      failure_domain_class: "multi_host",
      authority: "production_readiness",
    },
    measurements,
    observations,
    artifacts,
  });
}

function derivationKind(derivation) {
  return derivation.split(".")[0];
}

const check = process.argv.includes("--check");
let drift = 0;
for (const [name, , , content] of artifactFiles) {
  const path = join(artifactDirectory, name);
  if (check) {
    const current = readFileSync(path, "utf8");
    if (current !== content) {
      console.error(`drifted: testdata/g6/artifacts/${name}`);
      drift += 1;
    }
    continue;
  }
  mkdirSync(dirname(path), { recursive: true });
  writeFileSync(path, content);
}

const evidence = buildEvidence();
const evidencePath = join(root, "testdata/g6/evidence-pass.json");
if (check) {
  if (readFileSync(evidencePath, "utf8") !== evidence) {
    console.error("drifted: testdata/g6/evidence-pass.json");
    drift += 1;
  }
  if (drift > 0) {
    console.error(
      `${drift} fixture file(s) drifted; run scripts/generate-g6-test-fixtures.mjs`,
    );
    process.exitCode = 1;
  } else {
    console.log("G6 fixtures match the generator output");
  }
} else {
  writeFileSync(evidencePath, evidence);
  console.log(
    `regenerated ${artifactFiles.length} artifacts and evidence-pass.json`,
  );
}
