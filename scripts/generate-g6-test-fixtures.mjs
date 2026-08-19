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
const finishedAt = "2026-08-14T00:05:30Z";

function at(seconds) {
  const hh = String(Math.floor(seconds / 3600)).padStart(2, "0");
  const mm = String(Math.floor((seconds % 3600) / 60)).padStart(2, "0");
  const ss = String(seconds % 60).padStart(2, "0");
  return `2026-08-14T${hh}:${mm}:${ss}Z`;
}

function atMicros(seconds, micros) {
  return at(seconds).replace("Z", `.${String(micros).padStart(6, "0")}Z`);
}

const finalBeforeCompleteAt = atMicros(310, 100000);
const finalAuthorityCutAt = atMicros(310, 200000);
const finalAfterStartAt = atMicros(310, 300000);
const ownerLeaseExpiryAt = atMicros(10, 100000);
const ownerTakeoverAt = atMicros(10, 200000);
const bulkDisconnectAt = at(180);
const reconnectStartedAt = at(181);
const reconnectCompletedAt = at(250);
const resourceSamplingStoppedAt = atMicros(300, 100000);
const apiSloMeasuredAt = atMicros(300, 200000);
const finalLeaseUntil = at(329);

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

function reconnectAt(index) {
  return atMicros(200 + index, 100000);
}

function reconnectRetiredAt(index) {
  return atMicros(200 + index, 0);
}

function takeoverConnectionId(index) {
  return (0x8000n + BigInt(index + 1)).toString(16).padStart(32, "0");
}

function finalOwnerInstance() {
  return "worker-b";
}

function finalOwnerIncarnation() {
  return workerBIncarnation;
}

function finalConnectionId(index) {
  return (0x9000n + BigInt(index + 1)).toString(16).padStart(32, "0");
}

function finalOwnerEpoch() {
  return 3;
}

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
      `${timestamp},controller,api-fd-b,${104857600 + tick * 1024},${48 + Math.floor(tick / 15)},${120 + Math.floor(tick / 5)},0,,${bindingFields}`,
    );
    lines.push(
      `${timestamp},controller,worker-fd-b,${94371840 + tick * 768},${44 + Math.floor(tick / 20)},${110 + Math.floor(tick / 6)},0,,${bindingFields}`,
    );
    lines.push(
      `${timestamp},controller,scheduler-fd-b,${83886080 + tick * 512},${40 + Math.floor(tick / 30)},${100 + Math.floor(tick / 10)},0,,${bindingFields}`,
    );
    lines.push(
      `${timestamp},transportd,transportd-fd-b,${104857600 + tick * 2048},${40 + Math.floor(tick / 20)},${200 + Math.floor(tick / 8)},0,,${bindingFields}`,
    );
    lines.push(
      `${timestamp},agent,agent-fd-b-01,${52428800 + tick * 512},${24 + Math.floor(tick / 30)},${60 + Math.floor(tick / 15)},0,,${bindingFields}`,
    );
    lines.push(
      `${timestamp},postgres,postgres-fd-b,${209715200 + tick * 4096},80,40,0,${40 + Math.floor(tick / 30)},${bindingFields}`,
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
  "resource_sampling_stopped",
  "api_slo_measured",
];

function timelineAt(eventId, index) {
  if (eventId === "bulk_disconnect_injected") return bulkDisconnectAt;
  if (eventId === "reconnect_started") return reconnectStartedAt;
  if (eventId === "reconnect_completed") return reconnectCompletedAt;
  if (eventId === "resource_sampling_stopped") return resourceSamplingStoppedAt;
  if (eventId === "api_slo_measured") return apiSloMeasuredAt;
  const reconnectCompletedIndex = timelineEventIds.indexOf("reconnect_completed");
  if (index > reconnectCompletedIndex) {
    return at(251 + index - reconnectCompletedIndex - 1);
  }
  return at(index);
}

function buildTimeline() {
  return jsonl(
    timelineEventIds.map((event_id, index) => ({
      event_id,
      sequence: index + 1,
      ...bindingRecord(timelineAt(event_id, index)),
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
    lease_until: ownerLeaseExpiryAt,
  }));
  records.push({
    sequence: 51,
    ...bindingRecord(at(5)),
    subject: "connection_owner",
    event_type: "owner_accept",
    node: fixtureNodeId(1),
    instance: "worker-a",
    epoch: 1,
    accepted: true,
  });
  let sequence = records.at(-1).sequence;
  // Every managed epoch-one term expires and advances inside the formal owner
  // failover timeline. The later reconnect storm retires epoch two and creates
  // a distinct durable epoch-three registration; it cannot masquerade as the
  // lease-expiry scenario.
  for (let index = 0; index < 50; index += 1) {
    records.push({
      sequence: (sequence += 1),
      ...bindingRecord(ownerLeaseExpiryAt),
      subject: "connection_owner",
      event_type: "owner_lease_expired",
      node: fixtureNodeId(index + 1),
      epoch: 1,
    });
  }
  for (let index = 0; index < 50; index += 1) {
    records.push({
      sequence: (sequence += 1),
      ...bindingRecord(ownerTakeoverAt),
      subject: "connection_owner",
      event_type: "owner_registered",
      node: fixtureNodeId(index + 1),
      instance: "worker-b",
      incarnation: workerBIncarnation,
      connection_id: takeoverConnectionId(index),
      epoch: 2,
      lease_until: reconnectRetiredAt(index),
      session_connected_at: atMicros(10, 300000 + index * 1000),
    });
  }
  records.push(
    {
      sequence: (sequence += 1),
      ...bindingRecord(at(12)),
      subject: "connection_owner",
      event_type: "owner_accept",
      node: fixtureNodeId(1),
      instance: "worker-b",
      epoch: 2,
      accepted: true,
    },
    {
      sequence: (sequence += 1),
      ...bindingRecord(at(13)),
      subject: "connection_owner",
      event_type: "owner_accept",
      node: fixtureNodeId(1),
      instance: "worker-a",
      epoch: 1,
      accepted: false,
    },
    { sequence: (sequence += 1), ...bindingRecord(at(60)), subject: "scheduler", event_type: "leader_acquired", instance: "sched-a", incarnation: schedulerAIncarnation, epoch: 1, lease_until: at(75) },
    { sequence: (sequence += 1), ...bindingRecord(at(65)), subject: "scheduler", event_type: "leader_commit", instance: "sched-a", incarnation: schedulerAIncarnation, epoch: 1, maintenance_id: "1", marker_completed_at: at(65), accepted: true },
    { sequence: (sequence += 1), ...bindingRecord(at(75)), subject: "scheduler", event_type: "leader_lease_expired", epoch: 1 },
    { sequence: (sequence += 1), ...bindingRecord(at(76)), subject: "scheduler", event_type: "leader_acquired", instance: "sched-b", incarnation: schedulerBIncarnation, epoch: 2, lease_until: finalLeaseUntil },
    { sequence: (sequence += 1), ...bindingRecord(atMicros(85, 1)), subject: "scheduler", event_type: "leader_commit", instance: "sched-b", incarnation: schedulerBIncarnation, epoch: 2, maintenance_id: "2", marker_completed_at: at(85), accepted: true },
    { sequence: (sequence += 1), ...bindingRecord(at(86)), subject: "scheduler", event_type: "leader_commit", instance: "sched-a", incarnation: schedulerAIncarnation, epoch: 1, accepted: false },
  );
  for (let index = 0; index < 50; index += 1) {
    records.push(
      {
        sequence: (sequence += 1),
        ...bindingRecord(reconnectRetiredAt(index)),
        subject: "connection_owner",
        event_type: "owner_retired",
        node: fixtureNodeId(index + 1),
        epoch: 2,
      },
      {
        sequence: (sequence += 1),
        ...bindingRecord(reconnectAt(index)),
        subject: "connection_owner",
        event_type: "owner_registered",
        node: fixtureNodeId(index + 1),
        instance: finalOwnerInstance(index),
        incarnation: finalOwnerIncarnation(index),
        connection_id: finalConnectionId(index),
        epoch: finalOwnerEpoch(index),
        lease_until: finalLeaseUntil,
      },
    );
  }
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
  const record = (timestamp, extra) => {
    sequence += 1;
    records.push({ sequence, ...bindingRecord(timestamp), ...extra });
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
        record(at(second), {
          record_type: "enqueued",
          command_id: `cmd-${suffix}`,
          idempotency_key: `idem-http-${suffix}`,
        });
      }
      for (const suffix of wave) {
        record(at(second), { record_type: "dispatched", command_id: `cmd-${suffix}` });
      }
      for (const suffix of wave) {
        record(at(second), {
          record_type: "effect",
          command_id: `cmd-${suffix}`,
          idempotency_key: `idem-${suffix}`,
          effect_id: `fx-${suffix}`,
        });
      }
    }
    if (second === 4) {
      record(atMicros(4, 0), {
        record_type: "inflight_snapshot",
        expected_count: 50,
        result_count: 0,
        commands: commands.slice(0, 50).map((suffix, index) => ({
          command_id: `cmd-${suffix}`,
          node_id: fixtureNodeId(index + 1),
          state: "running",
        })),
      });
    }
    if (second >= 4) {
      const wave = commands.slice((second - 4) * 10, (second - 4) * 10 + 10);
      for (const suffix of wave) {
        record(atMicros(second, 1000), {
          record_type: "result",
          command_id: `cmd-${suffix}`,
          outcome: "success",
        });
      }
    }
  }
  record(at(155), {
    record_type: "enqueued",
    command_id: "cmd-relay-pre-proof",
    idempotency_key: "g6-relay-pre-fault-fixture",
  });
  record(at(156), {
    record_type: "dispatched",
    command_id: "cmd-relay-pre-proof",
  });
  record(at(157), {
    record_type: "effect",
    command_id: "cmd-relay-pre-proof",
    idempotency_key: "idem-relay-pre-proof",
    effect_id: "fx-relay-pre-proof",
  });
  record(at(158), {
    record_type: "result",
    command_id: "cmd-relay-pre-proof",
    outcome: "success",
  });
  record(at(161), {
    record_type: "enqueued",
    command_id: "cmd-relay-proof",
    idempotency_key: "g6-relay-failover-fixture",
  });
  record(at(162), {
    record_type: "dispatched",
    command_id: "cmd-relay-proof",
  });
  record(at(163), {
    record_type: "effect",
    command_id: "cmd-relay-proof",
    idempotency_key: "idem-relay-proof",
    effect_id: "fx-relay-proof",
  });
  record(at(164), {
    record_type: "result",
    command_id: "cmd-relay-proof",
    outcome: "success",
  });
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
    "timestamp,kind,status,latency_seconds,request_id,idempotency_key,attempt_ordinal,attempt_limit,requested_revision,http_status,problem_type,problem_detail,command_id,environment_id,candidate_sha";
  const lines = [header];
  const latency = (millis) => String(millis / 1000);
  const row = (values) => values.join(",");
  let enqueueCounter = 0;
  for (let second = 0; second < 300; second += 1) {
    const timestamp = at(second);
    for (let sub = 0; sub < 2; sub += 1) {
      lines.push(row([
        timestamp,
        "read",
        "ok",
        latency(10 + ((second * 2 + sub) % 7)),
        "", "", "", "", "", 200, "", "", "",
        environmentId,
        candidateSha,
      ]));
      const suffix = String(enqueueCounter + 1).padStart(3, "0");
      const key = `idem-http-${suffix}`;
      if (enqueueCounter === 0) {
        lines.push(row([
          timestamp,
          "enqueue",
          "error",
          latency(20),
          `${key}.attempt-1`,
          key,
          1,
          3,
          10,
          409,
          "https://ocservia.dev/problems/stale-revision",
          "the node changed after this operation was prepared",
          "",
          environmentId,
          candidateSha,
        ]));
      }
      lines.push(row([
        enqueueCounter === 0 ? atMicros(second, 30000) : timestamp,
        "enqueue",
        "ok",
        latency(50 + (enqueueCounter % 10) * 10),
        `${key}.attempt-${enqueueCounter === 0 ? 2 : 1}`,
        key,
        enqueueCounter === 0 ? 2 : 1,
        3,
        enqueueCounter === 0 ? 11 : 10 + enqueueCounter,
        202,
        "",
        "",
        `cmd-${suffix}`,
        environmentId,
        candidateSha,
      ]));
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
    writes: Array.from({ length: 600 }, (_, index) => {
      const suffix = String(index + 1).padStart(3, "0");
      const requestId = `idem-http-${suffix}.attempt-${index === 0 ? 2 : 1}`;
      return {
        write_id: `cmd-${suffix}`,
        intent_recorded: true,
        intent_request_id: requestId,
        result_recorded: true,
        result_request_id: requestId,
      };
    }),
  });
}

function buildPostgresRecovery() {
  return canonicalJson({
    environment_id: environmentId,
    candidate_sha: candidateSha,
    outage_declared_at: at(60),
    rto_started_at: at(58),
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
      promoted_at: at(120),
      isolated_primary_writes: [
        { at: at(125), accepted: false },
        { at: at(130), accepted: false },
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
      owner_instance: finalOwnerInstance(index),
      owner_incarnation: finalOwnerIncarnation(index),
      connection_id: finalConnectionId(index),
      owner_epoch: finalOwnerEpoch(index),
      owner_lease_until: finalLeaseUntil,
      session_started_at: at(0),
      connected_at: reconnectAt(index),
      session_expires_at: finalLeaseUntil,
      reconnected_at: reconnectAt(index),
      reconnect_owner_instance: finalOwnerInstance(index),
      reconnect_owner_incarnation: finalOwnerIncarnation(index),
      reconnect_connection_id: finalConnectionId(index),
      reconnect_owner_epoch: finalOwnerEpoch(index),
    })),
    scheduler_authority: {
      instance: "sched-b",
      incarnation: schedulerBIncarnation,
      epoch: 2,
      lease_until: finalLeaseUntil,
    },
    reconnect_storm: {
      bulk_disconnect_at: bulkDisconnectAt,
    },
  });
}

function buildTransportObservation(index) {
  return {
    node: fixtureNodeId(index + 1),
    endpoint_id: (index + 1).toString(16).padStart(64, "0"),
    agent_instance_id: (index + 1001).toString(16).padStart(32, "0"),
    connected_at: reconnectAt(index),
    session_expires_at: finalLeaseUntil,
    owner_fence_id: (index + 2001).toString(16).padStart(32, "0"),
    owner_instance: finalOwnerInstance(index),
    owner_incarnation: finalOwnerIncarnation(index),
    connection_id: finalConnectionId(index),
    owner_epoch: finalOwnerEpoch(index),
    owner_lease_until: finalLeaseUntil,
    authorization_revision: 11,
    negotiated_capabilities: ["ocserv.fencing.v2", "ocserv.status.read"],
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
      instance: finalOwnerInstance(index),
      incarnation: finalOwnerIncarnation(index),
      connection_id: finalConnectionId(index),
      epoch: finalOwnerEpoch(index),
      lease_until: finalLeaseUntil,
    })),
    scheduler: {
      instance: "sched-b",
      incarnation: schedulerBIncarnation,
      epoch: 2,
      lease_until: finalLeaseUntil,
      maintenance_id: "2",
      maintenance_completed_at: at(85),
    },
  });
}

function buildRelayTransitions() {
  const relayProbe = buildTransportObservation(0);
  relayProbe.connected_at = atMicros(10, 300000);
  relayProbe.session_expires_at = reconnectRetiredAt(0);
  relayProbe.owner_instance = "worker-b";
  relayProbe.owner_incarnation = workerBIncarnation;
  relayProbe.connection_id = takeoverConnectionId(0);
  relayProbe.owner_epoch = 2;
  relayProbe.owner_lease_until = reconnectRetiredAt(0);
  const records = [
    { sequence: 1, ...bindingRecord(at(150)), event_type: "path_active", session_id: "s-001", path: "direct", authenticated: true },
    { sequence: 2, ...bindingRecord(at(155)), event_type: "path_failed", session_id: "s-001", path: "direct" },
    {
      sequence: 3,
      ...bindingRecord(at(159)),
      event_type: "path_active",
      session_id: relayProbe.node,
      path: "relay",
      relay: "relay-a",
      authenticated: true,
      endpoint_id: relayProbe.endpoint_id,
      path_detail: "iroh/relay-a",
      owner_fence_id: relayProbe.owner_fence_id,
      owner_instance: relayProbe.owner_instance,
      owner_incarnation: relayProbe.owner_incarnation,
      connection_id: relayProbe.connection_id,
      owner_epoch: relayProbe.owner_epoch,
      authorization_revision: relayProbe.authorization_revision,
      negotiated_capabilities: relayProbe.negotiated_capabilities,
      session_connected_at: relayProbe.connected_at,
      owner_lease_until: relayProbe.owner_lease_until,
      session_expires_at: relayProbe.session_expires_at,
      topology_mode: "relay-a-only",
      topology_network_name: "g6-rd-fixture_relay-a-only",
      topology_agent_service: "agent-fd-a-01",
      topology_network_internal: true,
      topology_agent_default_network_connected: false,
      topology_ready_at: at(153),
      relay_b_disabled_at: at(154),
      command_id: "cmd-relay-pre-proof",
      command_idempotency_key: "g6-relay-pre-fault-fixture",
      effect_idempotency_key: "idem-relay-pre-proof",
      effect_id: "fx-relay-pre-proof",
      result_observed_at: at(158),
    },
    {
      sequence: 4,
      ...bindingRecord(at(160)),
      event_type: "relay_failed",
      relay: "relay-a",
      session_id: relayProbe.node,
      owner_instance: relayProbe.owner_instance,
      owner_incarnation: relayProbe.owner_incarnation,
      connection_id: relayProbe.connection_id,
      owner_epoch: relayProbe.owner_epoch,
      owner_lease_until: relayProbe.owner_lease_until,
      authority_lease_until: relayProbe.owner_lease_until,
      fault_cut_at: at(160),
    },
    {
      sequence: 5,
      ...bindingRecord(at(165)),
      event_type: "path_active",
      session_id: relayProbe.node,
      path: "relay",
      relay: "relay-b",
      authenticated: true,
      endpoint_id: relayProbe.endpoint_id,
      path_detail: "iroh/relay-b",
      owner_fence_id: relayProbe.owner_fence_id,
      owner_instance: relayProbe.owner_instance,
      owner_incarnation: relayProbe.owner_incarnation,
      connection_id: relayProbe.connection_id,
      owner_epoch: relayProbe.owner_epoch,
      authorization_revision: relayProbe.authorization_revision,
      negotiated_capabilities: relayProbe.negotiated_capabilities,
      session_connected_at: relayProbe.connected_at,
      owner_lease_until: relayProbe.owner_lease_until,
      session_expires_at: relayProbe.session_expires_at,
      relay_b_started_at: atMicros(160, 500000),
      command_id: "cmd-relay-proof",
      command_idempotency_key: "g6-relay-failover-fixture",
      effect_idempotency_key: "idem-relay-proof",
      effect_id: "fx-relay-proof",
      result_observed_at: at(164),
    },
    { sequence: 6, ...bindingRecord(at(170)), event_type: "relay_active", relay: "relay-b" },
    { sequence: 7, ...bindingRecord(at(190)), event_type: "path_failed", session_id: "s-001", path: "relay" },
    { sequence: 8, ...bindingRecord(at(195)), event_type: "path_active", session_id: "s-001", path: "direct", authenticated: true },
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
