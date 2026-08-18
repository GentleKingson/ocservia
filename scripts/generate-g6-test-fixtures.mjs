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

function atMicros(seconds, micros) {
  return at(seconds).replace("Z", `.${String(micros).padStart(6, "0")}Z`);
}

const finalBeforeCompleteAt = atMicros(270, 100000);
const finalAuthorityCutAt = atMicros(270, 200000);
const finalAfterStartAt = atMicros(270, 300000);

const bindingFields = `${environmentId},${candidateSha}`;
const bindingRecord = (timestamp) => ({
  timestamp,
  environment_id: environmentId,
  candidate_sha: candidateSha,
});
const fixtureNodeId = (index) => index.toString(16).padStart(32, "0");
const workerAIncarnation = "1700000000000000001";
const workerBIncarnation = "1700000000000000002";
const schedulerAIncarnation = "1800000000000000001";
const schedulerBIncarnation = "1800000000000000002";

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
  const records = Array.from({ length: 50 }, (_, index) => ({
    sequence: index + 1,
    ...bindingRecord(at(index === 0 ? 0 : 1)),
    subject: "connection_owner",
    event_type: "owner_registered",
    node: fixtureNodeId(index + 1),
    instance: "worker-a",
    incarnation: workerAIncarnation,
    connection_id: String(index + 1).padStart(32, "0"),
    epoch: 1,
    lease_until: at(index === 0 ? 10 : 300),
  }));
  records.push(
    { sequence: 51, ...bindingRecord(at(5)), subject: "connection_owner", event_type: "owner_accept", node: fixtureNodeId(1), instance: "worker-a", epoch: 1, accepted: true },
    { sequence: 52, ...bindingRecord(at(10)), subject: "connection_owner", event_type: "owner_lease_expired", node: fixtureNodeId(1), epoch: 1 },
    { sequence: 53, ...bindingRecord(at(20)), subject: "connection_owner", event_type: "owner_registered", node: fixtureNodeId(1), instance: "worker-b", incarnation: workerBIncarnation, connection_id: String(1).padStart(32, "0"), epoch: 2, lease_until: at(300) },
    { sequence: 54, ...bindingRecord(at(25)), subject: "connection_owner", event_type: "owner_accept", node: fixtureNodeId(1), instance: "worker-b", epoch: 2, accepted: true },
    { sequence: 55, ...bindingRecord(at(30)), subject: "connection_owner", event_type: "owner_accept", node: fixtureNodeId(1), instance: "worker-a", epoch: 1, accepted: false },
    { sequence: 56, ...bindingRecord(at(60)), subject: "scheduler", event_type: "leader_acquired", instance: "sched-a", incarnation: schedulerAIncarnation, epoch: 1, lease_until: at(75) },
    { sequence: 57, ...bindingRecord(at(65)), subject: "scheduler", event_type: "leader_commit", instance: "sched-a", epoch: 1, accepted: true },
    { sequence: 58, ...bindingRecord(at(75)), subject: "scheduler", event_type: "leader_lease_expired", epoch: 1 },
    { sequence: 59, ...bindingRecord(at(76)), subject: "scheduler", event_type: "leader_acquired", instance: "sched-b", incarnation: schedulerBIncarnation, epoch: 2, lease_until: at(300) },
    { sequence: 60, ...bindingRecord(at(85)), subject: "scheduler", event_type: "leader_commit", instance: "sched-b", epoch: 2, accepted: true },
    { sequence: 61, ...bindingRecord(at(86)), subject: "scheduler", event_type: "leader_commit", instance: "sched-a", epoch: 1, accepted: false },
  );
  return jsonl(records);
}

function buildCommandTrace() {
  // The trace's enqueued population must equal the accepted-write
  // population (cmd-001..cmd-600), so every accepted command appears here.
  // Commands run in 60 waves of 10: wave w enqueues, dispatches, and takes
  // effect at second w, with its results at w+4 (emitted inside second
  // w+4's block so stream timestamps never decrease). Just before each
  // second's results land, five waves are in flight, so the peak
  // concurrent in-flight count is exactly 50 — the SLO floor — and every
  // dispatch is inside the 10s bound.
  const commands = Array.from({ length: 600 }, (_, index) =>
    String(index + 1).padStart(3, "0"),
  );
  const record = (second, extra) => {
    sequence += 1;
    records.push({ sequence, ...bindingRecord(at(second)), ...extra });
  };
  const records = [
    {
      sequence: 1,
      ...bindingRecord(at(0)),
      record_type: "profile",
      dispatch_bound_seconds: 10,
    },
  ];
  let sequence = 1;
  for (let second = 0; second <= 63; second += 1) {
    if (second < 60) {
      const wave = commands.slice(second * 10, second * 10 + 10);
      for (const suffix of wave) {
        record(second, { record_type: "enqueued", command_id: `cmd-${suffix}` });
      }
      for (const suffix of wave) {
        record(second, { record_type: "dispatched", command_id: `cmd-${suffix}` });
      }
      for (const suffix of wave) {
        record(second, {
          record_type: "effect",
          idempotency_key: `idem-${suffix}`,
          effect_id: `fx-${suffix}`,
        });
      }
    }
    if (second >= 4) {
      const wave = commands.slice((second - 4) * 10, (second - 4) * 10 + 10);
      for (const suffix of wave) {
        record(second, {
          record_type: "result",
          command_id: `cmd-${suffix}`,
          outcome: "success",
        });
      }
    }
  }
  return jsonl(records);
}

function buildOutboxSnapshot() {
  // One outbox row per accepted enqueue request: the row set must equal
  // the ok-enqueue population in http-samples.csv and the audit write
  // population (same cmd-XXX identity chain).
  const rows = Array.from({ length: 600 }, (_, index) => {
    const suffix = String(index + 1).padStart(3, "0");
    const state =
      index >= 599
        ? "unknown_reconciling"
        : index >= 597
          ? "reconciliation_active"
          : "terminal";
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
  const header =
    "timestamp,kind,status,latency_seconds,request_id,environment_id,candidate_sha";
  const lines = [header];
  const latency = (millis) => String(millis / 1000);
  let enqueueCounter = 0;
  for (let second = 0; second < 300; second += 1) {
    const timestamp = at(second);
    for (let sub = 0; sub < 2; sub += 1) {
      lines.push(
        `${timestamp},read,ok,${latency(10 + ((second * 2 + sub) % 7))},,${bindingFields}`,
      );
      lines.push(
        `${timestamp},enqueue,ok,${latency(50 + (enqueueCounter % 10) * 10)},cmd-${String(enqueueCounter + 1).padStart(3, "0")},${bindingFields}`,
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
  // One audit write per accepted enqueue request, sharing the cmd-XXX
  // identity chain with http-samples.csv and the outbox snapshot.
  return canonicalJson({
    environment_id: environmentId,
    candidate_sha: candidateSha,
    writes: Array.from({ length: 600 }, (_, index) => ({
      write_id: `cmd-${String(index + 1).padStart(3, "0")}`,
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
    snapshot_taken_at: finalAuthorityCutAt,
    sessions: Array.from({ length: 50 }, (_, index) => ({
      agent_id: `agent-${String(index + 1).padStart(2, "0")}`,
      node: fixtureNodeId(index + 1),
      endpoint_id: (index + 1).toString(16).padStart(64, "0"),
      agent_instance_id: (index + 1001).toString(16).padStart(32, "0"),
      authorized: true,
      connected: true,
      owner_instance: index === 0 ? "worker-b" : "worker-a",
      owner_incarnation:
        index === 0 ? workerBIncarnation : workerAIncarnation,
      connection_id: String(index + 1).padStart(32, "0"),
      owner_epoch: index === 0 ? 2 : 1,
      owner_lease_until: at(300),
      session_started_at: at(0),
      connected_at: at(200 + index),
      session_expires_at: at(300),
      // Per-agent recovery timestamps: the verifier derives the storm
      // recovery as max(reconnected_at) - bulk_disconnect_at.
      reconnected_at: at(200 + index),
    })),
    scheduler_authority: {
      instance: "sched-b",
      incarnation: schedulerBIncarnation,
      epoch: 2,
      lease_until: at(300),
    },
    reconnect_storm: {
      bulk_disconnect_at: at(180),
    },
  });
}

function buildTransportObservation(index) {
  return {
    node: fixtureNodeId(index + 1),
    endpoint_id: (index + 1).toString(16).padStart(64, "0"),
    agent_instance_id: (index + 1001).toString(16).padStart(32, "0"),
    connected_at: at(200 + index),
    session_expires_at: at(300),
    owner_fence_id: (index + 2001).toString(16).padStart(32, "0"),
    owner_instance: index === 0 ? "worker-b" : "worker-a",
    owner_incarnation:
      index === 0 ? workerBIncarnation : workerAIncarnation,
    connection_id: String(index + 1).padStart(32, "0"),
    owner_epoch: index === 0 ? 2 : 1,
    owner_lease_until: at(300),
    authorization_revision: 11,
    negotiated_capabilities: ["ocserv.status.read"],
  };
}

function buildAuthorityCut() {
  return canonicalJson({
    environment_id: environmentId,
    candidate_sha: candidateSha,
    cut_at: finalAuthorityCutAt,
    transport_bracket: {
      before_complete_at: finalBeforeCompleteAt,
      after_start_at: finalAfterStartAt,
      before: Array.from({ length: 50 }, (_, index) =>
        buildTransportObservation(index),
      ),
      after: Array.from({ length: 50 }, (_, index) =>
        buildTransportObservation(index),
      ),
    },
    owners: Array.from({ length: 50 }, (_, index) => ({
      node: fixtureNodeId(index + 1),
      instance: index === 0 ? "worker-b" : "worker-a",
      incarnation: index === 0 ? workerBIncarnation : workerAIncarnation,
      connection_id: String(index + 1).padStart(32, "0"),
      epoch: index === 0 ? 2 : 1,
      lease_until: at(300),
    })),
    scheduler: {
      instance: "sched-b",
      incarnation: schedulerBIncarnation,
      epoch: 2,
      lease_until: at(300),
    },
  });
}

function buildRelayTransitions() {
  const records = [
    { sequence: 1, ...bindingRecord(at(150)), event_type: "path_active", session_id: "s-001", path: "direct", authenticated: true },
    { sequence: 2, ...bindingRecord(at(155)), event_type: "path_failed", session_id: "s-001", path: "direct" },
    { sequence: 3, ...bindingRecord(at(160)), event_type: "relay_failed", relay: "relay-a" },
    { sequence: 4, ...bindingRecord(at(165)), event_type: "path_active", session_id: "s-001", path: "relay", relay: "relay-b", authenticated: true },
    { sequence: 5, ...bindingRecord(at(170)), event_type: "relay_active", relay: "relay-b" },
    { sequence: 6, ...bindingRecord(at(190)), event_type: "path_failed", session_id: "s-001", path: "relay" },
    { sequence: 7, ...bindingRecord(at(195)), event_type: "path_active", session_id: "s-001", path: "direct", authenticated: true },
  ];
  return jsonl(records);
}

const artifactFiles = [
  ["resource-samples.csv", "text/csv", "resource_samples", buildResourceSamples()],
  ["timeline.jsonl", "application/x-ndjson", "timeline", buildTimeline()],
  ["epoch-events.jsonl", "application/x-ndjson", "epoch_events", buildEpochEvents()],
  ["authority-cut.json", "application/json", "authority_cut", buildAuthorityCut()],
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
