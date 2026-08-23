#!/usr/bin/env node

// Integration test for the G6 readiness evidence builder: a complete
// synthetic harness state (both failure domains, every trusted-producer
// format the fd-a/fd-b phases freeze) is assembled, run through
// build-g6-evidence.mjs, and the resulting bundle must be awarded a final
// G6 pass by the shared verifier. The same bundle assembled under the
// engineering authority must stay non-final for the authority reason alone.

import { spawnSync } from "node:child_process";
import {
  cpSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import {
  sha256Digest,
  structuredArtifactKinds,
  verifyG6,
} from "./g6-contract-lib.mjs";

const root = fileURLToPath(new URL("../", import.meta.url));
const environmentId = "g6-abcd1234";
const candidateSha = "2234567890123456789012345678901234567890";
const base = Date.parse("2026-08-14T10:00:00Z");
const at = (seconds) =>
  `${new Date(base + seconds * 1000).toISOString().slice(0, 19)}Z`;
const atMicros = (seconds, micros) =>
  at(seconds).replace("Z", `.${String(micros).padStart(6, "0")}Z`);
const atNanos = (seconds, nanos) =>
  at(seconds).replace("Z", `.${String(nanos).padStart(9, "0")}Z`);
const epochSeconds = (seconds) => Math.floor((base + seconds * 1000) / 1000);

function fakeUuid(seed, version = 7) {
  // the seed appears in both kept groups so every distinct seed yields a
  // distinct uuid (and a distinct dash-stripped 32-hex identity)
  const head = seed.toString(16).padStart(8, "0").slice(-8);
  const tail = seed.toString(16).padStart(12, "0").slice(-12);
  return `${head}-0000-${version}000-8000-${tail}`;
}

const digestOf = (byte) => `sha256:${byte.repeat(64)}`;
const workerAIncarnation = "1700000000000000001";
const workerBIncarnation = "1700000000000000002";
const schedulerAIncarnation = "1800000000000000001";
const schedulerBIncarnation = "1800000000000000002";

function jsonl(records) {
  return records.length === 0
    ? ""
    : `${records.map((record) => JSON.stringify(record)).join("\n")}\n`;
}

const work = mkdtempSync(join(tmpdir(), "g6-builder-"));
const runDir = join(work, "run");
const peerDir = join(work, "peer");
for (const directory of [
  join(runDir, "state", "evidence", "effects"),
  join(runDir, "outbox"),
  join(runDir, "outbox", "relay-pre-fault"),
  join(peerDir, "isolation"),
  join(peerDir, "pitr"),
  join(peerDir, "evidence"),
]) {
  mkdirSync(directory, { recursive: true });
}
const write = (path, content) => writeFileSync(path, content);

// ---------------------------------------------------------------------------
// The formal managed fleet: exactly 25 agents in each failure domain.
// ---------------------------------------------------------------------------

const nodes = [];
for (let index = 1; index <= 25; index += 1) {
  nodes.push({
    name: `g6-fd-a-${String(index).padStart(2, "0")}`,
    nodeId: fakeUuid(index),
    endpointId: index.toString(16).padStart(64, "0"),
  });
}
for (let index = 1; index <= 25; index += 1) {
  nodes.push({
    name: `g6-fd-b-${String(index).padStart(2, "0")}`,
    nodeId: fakeUuid(index + 100),
    endpointId: (index + 100).toString(16).padStart(64, "0"),
  });
}
write(
  join(runDir, "state", "all-nodes.tsv"),
  `${nodes.map((node) => `${node.name}\t${node.nodeId}\t${node.endpointId}`).join("\n")}\n`,
);

// ---------------------------------------------------------------------------
// The bounded window: sixty-one sampler ticks (six rows each) and a read
// and enqueue stream that opens with a fleet-wide parallel burst.
// ---------------------------------------------------------------------------

const samplerLines = [
  "timestamp,component,instance,rss_bytes,fd_count,tasks,queue_depth,db_connections,environment_id,candidate_sha",
];
const readLog = [];
const enqueueLog = [];
const commands = [];
const attempts = [];
const outboxRows = [];
const auditRows = [];
const effectsByAgent = new Map();
let commandSeed = 1;
let rejectedCommandId = "";

function enqueueCommand(
  nodeIndex,
  stampSeconds,
  commandState = "succeeded",
  keyOverride,
) {
  const commandId = fakeUuid(commandSeed + 500, 7);
  const key = keyOverride ?? `g6-window-run-${commandSeed}`;
  const staleFirstAttempt = commandSeed === 1;
  const acceptedRequestId = `${key}.attempt-${staleFirstAttempt ? 2 : 1}`;
  commandSeed += 1;
  if (commandState === "rejected") rejectedCommandId = commandId;
  if (staleFirstAttempt) {
    enqueueLog.push({
      at: at(stampSeconds),
      node: nodes[nodeIndex].nodeId,
      idempotency_key: key,
      attempt_request_id: `${key}.attempt-1`,
      attempt_ordinal: 1,
      attempt_limit: 3,
      requested_revision: 10,
      status: 409,
      latency_seconds: 0.02,
      problem_type: "https://ocservia.dev/problems/stale-revision",
      problem_detail: "the node changed after this operation was prepared",
      command_id: "",
    });
  }
  enqueueLog.push({
    at: staleFirstAttempt ? atMicros(stampSeconds, 30000) : at(stampSeconds),
    node: nodes[nodeIndex].nodeId,
    idempotency_key: key,
    attempt_request_id: `${key}.attempt-${staleFirstAttempt ? 2 : 1}`,
    attempt_ordinal: staleFirstAttempt ? 2 : 1,
    attempt_limit: 3,
    requested_revision: staleFirstAttempt ? 11 : 10 + commandSeed,
    status: 202,
    latency_seconds: 0.05,
    problem_type: "",
    problem_detail: "",
    command_id: commandId,
  });
  const completion = stampSeconds + (stampSeconds < 10 ? 10 : 3);
  commands.push({
    id: commandId,
    idempotency_key: key,
    node_id: nodes[nodeIndex].nodeId,
    state: commandState,
    created_at: at(stampSeconds),
    updated_at: at(completion),
  });
  attempts.push({
    command_id: commandId,
    attempt_number: 1,
    state: "sent",
    started_at: at(stampSeconds + 1),
    finished_at: at(completion),
  });
  outboxRows.push({
    command_id: commandId,
    created_at: at(stampSeconds),
    available_at: at(stampSeconds + 10),
    published_at: at(completion),
    locked: false,
  });
  auditRows.push(
    {
      command_id: commandId,
      request_id: acceptedRequestId,
      result: "intent",
      occurred_at: at(stampSeconds),
    },
    {
      command_id: commandId,
      request_id: acceptedRequestId,
      result: commandState === "succeeded" ? "succeeded" : "failed",
      occurred_at: at(completion),
    },
  );
  if (commandState === "succeeded") {
    const agentFile = `${nodes[nodeIndex].name.replace("g6-", "agent-")}.tsv`;
    const lines = effectsByAgent.get(agentFile) ?? [];
    lines.push(
      `${fakeUuid(commandSeed, 3).replaceAll("-", "")} ${commandId.replaceAll("-", "")} ${epochSeconds(stampSeconds + 2)}`,
    );
    effectsByAgent.set(agentFile, lines);
  }
}

for (let tick = 0; tick <= 100; tick += 1) {
  const stamp = at(5 + tick * 3);
  for (const [component, instance, rss, fdCount, tasks, queue, db] of [
    ["controller", "api-fd-b", 104857600, 48, 120, 0, ""],
    ["controller", "worker-fd-b", 104857600, 48, 120, 0, ""],
    ["controller", "scheduler-fd-b", 52428800, 24, 60, 0, ""],
    ["transportd", "transportd-fd-b", 104857600, 40, 200, 0, ""],
    ["agent", "agent-fd-b-01", 52428800, 24, 60, 0, ""],
    ["postgres", "postgres-fd-b", 209715200, 80, 40, 0, 12],
  ]) {
    samplerLines.push(
      [
        stamp,
        component,
        instance,
        rss,
        fdCount,
        tasks,
        queue,
        db,
        environmentId,
        candidateSha,
      ].join(","),
    );
  }
  readLog.push(
    { at: stamp, status: 200, latency_seconds: 0.012 },
    { at: stamp, status: 200, latency_seconds: 0.013 },
  );
  if (tick === 0) {
    for (let nodeIndex = 0; nodeIndex < nodes.length; nodeIndex += 1) {
      enqueueCommand(
        nodeIndex,
        5,
        "succeeded",
        `g6-window-test-run-opening-${nodeIndex}`,
      );
    }
  }
  enqueueCommand(
    tick % nodes.length,
    5 + tick * 3,
    tick === 0 ? "rejected" : "succeeded",
  );
  enqueueCommand((tick + 1) % nodes.length, 5 + tick * 3);
}

const relayCommandId = fakeUuid(8800, 7);
const relayCommandKey = "g6-relay-failover-test-run";
const relayEffectKey = fakeUuid(8801, 3).replaceAll("-", "");
const relayEffectId = `fx-agent-fd-a-01-${relayEffectKey}`;
commands.push({
  id: relayCommandId,
  idempotency_key: relayCommandKey,
  node_id: nodes[0].nodeId,
  state: "succeeded",
  created_at: at(181),
  updated_at: at(184),
});
attempts.push({
  command_id: relayCommandId,
  attempt_number: 1,
  state: "sent",
  started_at: at(182),
  finished_at: at(184),
});
outboxRows.push({
  command_id: relayCommandId,
  created_at: at(181),
  available_at: at(181),
  published_at: at(184),
  locked: false,
});
auditRows.push(
  {
    command_id: relayCommandId,
    request_id: `${relayCommandKey}.attempt-1`,
    result: "intent",
    occurred_at: at(181),
  },
  {
    command_id: relayCommandId,
    request_id: `${relayCommandKey}.attempt-1`,
    result: "succeeded",
    occurred_at: at(184),
  },
);
const relayEffectFile = "agent-fd-a-01.tsv";
effectsByAgent.set(relayEffectFile, [
  ...(effectsByAgent.get(relayEffectFile) ?? []),
  `${relayEffectKey} ${relayCommandId.replaceAll("-", "")} ${epochSeconds(183)}`,
]);
const relayPreFaultCommandId = fakeUuid(8790, 7);
const relayPreFaultRunId = "test-run";
const relayPreFaultCommandKey = `g6-relay-pre-fault-${relayPreFaultRunId}`;
const relayPreFaultEffectKey = fakeUuid(8791, 3).replaceAll("-", "");
const relayPreFaultEffectId =
  `fx-agent-fd-a-01-${relayPreFaultEffectKey}`;
commands.push({
  id: relayPreFaultCommandId,
  idempotency_key: relayPreFaultCommandKey,
  node_id: nodes[0].nodeId,
  state: "succeeded",
  created_at: at(175),
  updated_at: at(178),
});
attempts.push({
  command_id: relayPreFaultCommandId,
  attempt_number: 1,
  state: "sent",
  started_at: at(176),
  finished_at: at(178),
});
outboxRows.push({
  command_id: relayPreFaultCommandId,
  created_at: at(175),
  available_at: at(175),
  published_at: at(178),
  locked: false,
});
auditRows.push(
  {
    command_id: relayPreFaultCommandId,
    request_id: `${relayPreFaultCommandKey}.attempt-1`,
    result: "intent",
    occurred_at: at(175),
  },
  {
    command_id: relayPreFaultCommandId,
    request_id: `${relayPreFaultCommandKey}.attempt-1`,
    result: "succeeded",
    occurred_at: at(178),
  },
);
effectsByAgent.set(relayEffectFile, [
  ...(effectsByAgent.get(relayEffectFile) ?? []),
  `${relayPreFaultEffectKey} ${relayPreFaultCommandId.replaceAll("-", "")} ${epochSeconds(177)}`,
]);

// A successful command from before the bounded HTTP/SLO window proves that
// durable-effect completeness is checked against the run-wide synthetic
// population without widening the window-only HTTP/dispatch denominator.
const historicalCommandId = fakeUuid(9000, 7);
const historicalCommandKey = "g6-load-test-run-history";
commands.push({
  id: historicalCommandId,
  idempotency_key: historicalCommandKey,
  node_id: nodes[0].nodeId,
  state: "succeeded",
  created_at: at(1),
  updated_at: at(4),
});
attempts.push({
  command_id: historicalCommandId,
  attempt_number: 1,
  state: "sent",
  started_at: at(2),
  finished_at: at(4),
});
outboxRows.push({
  command_id: historicalCommandId,
  created_at: at(1),
  available_at: at(2),
  published_at: at(4),
  locked: false,
});
auditRows.push(
  {
    command_id: historicalCommandId,
    request_id: `${historicalCommandKey}.attempt-1`,
    result: "intent",
    occurred_at: at(1),
  },
  {
    command_id: historicalCommandId,
    request_id: `${historicalCommandKey}.attempt-1`,
    result: "succeeded",
    occurred_at: at(4),
  },
);
const historicalEffectFile = "agent-fd-a-01.tsv";
effectsByAgent.set(historicalEffectFile, [
  `${fakeUuid(9001, 3).replaceAll("-", "")} ${historicalCommandId.replaceAll("-", "")} ${epochSeconds(3)}`,
  ...(effectsByAgent.get(historicalEffectFile) ?? []),
]);
write(
  join(runDir, "state", "resource-samples.csv"),
  `${samplerLines.join("\n")}\n`,
);
write(join(runDir, "state", "read-log.jsonl"), jsonl(readLog));
write(join(runDir, "state", "enqueue-log.jsonl"), jsonl(enqueueLog));
write(
  join(runDir, "state", "window-opening-active.json"),
  `${JSON.stringify(
    {
      captured_at: at(10),
      expected_count: nodes.length,
      commands: commands.slice(0, nodes.length).map((command) => ({
        command_id: command.id,
        node_id: command.node_id,
        state: "running",
        sent_attempt_count: 1,
      })),
      result_count: 0,
    },
    null,
    2,
  )}\n`,
);
write(join(runDir, "state", "evidence", "commands.jsonl"), jsonl(commands));
write(join(runDir, "state", "evidence", "attempts.jsonl"), jsonl(attempts));
write(join(runDir, "state", "evidence", "outbox.jsonl"), jsonl(outboxRows));
write(join(runDir, "state", "evidence", "audit.jsonl"), jsonl(auditRows));
for (const [name, lines] of effectsByAgent) {
  write(
    join(runDir, "state", "evidence", "effects", name),
    `${lines.join("\n")}\n`,
  );
}

// ---------------------------------------------------------------------------
// Sessions, telemetry, and the peer's failure-domain evidence.
// ---------------------------------------------------------------------------

write(
  join(runDir, "state", "era2-sessions.tsv"),
  `${nodes.map((node, index) => `${node.nodeId}\t${at(130 + (index % 5))}`).join("\n")}\n`,
);
const finalSessionsPath = join(
  runDir,
  "state",
  "evidence",
  "final-sessions.json",
);
const beforeFinalSessionsPath = join(
  runDir,
  "state",
  "evidence",
  "final-sessions-before.json",
);
const afterFinalSessionsPath = join(
  runDir,
  "state",
  "evidence",
  "final-sessions-after.json",
);
const reconnectSessionsPath = join(
  runDir,
  "state",
  "reconnect-sessions.json",
);
const finalSessionInventory = {
  mode: "node_connection",
  all_matched: true,
  observations: nodes.map((node, index) => ({
    node_id: node.nodeId,
    endpoint_id: node.endpointId,
    agent_instance_id: fakeUuid(index + 3000),
    found: true,
    path: "direct",
    owner_instance_id: index < 2 ? "worker-b" : "worker-a",
    owner_incarnation:
      index < 2 ? workerBIncarnation : workerAIncarnation,
    connection_id: fakeUuid(
      index === 0 ? 2001 : index === 1 ? 2002 : index + 1000,
    ).replaceAll("-", ""),
    owner_epoch: index === 0 ? 4 : index === 1 ? 3 : 1,
    owner_lease_until: at(350),
    owner_fence_id: fakeUuid(index + 4000).replaceAll("-", ""),
    authorization_revision: 11,
    negotiated_capabilities: ["ocserv.fencing.v2", "ocserv.status.read"],
    connected_at: at(160 + (index % 10)),
    session_expires_at: at(350),
  })),
};
write(beforeFinalSessionsPath, JSON.stringify(finalSessionInventory));
write(afterFinalSessionsPath, JSON.stringify(finalSessionInventory));
write(finalSessionsPath, JSON.stringify(finalSessionInventory));
write(
  join(runDir, "state", "evidence", "final-sessions-before-complete-at"),
  `${atMicros(320, 100000)}\n`,
);
write(
  join(runDir, "state", "evidence", "final-sessions-after-start-at"),
  `${atMicros(320, 300000)}\n`,
);
write(
  join(runDir, "state", "evidence", "telemetry.jsonl"),
  jsonl(
    nodes.map((node) => ({
      agent_id: node.name,
      last_telemetry_at: at(315),
    })),
  ),
);
write(
  join(runDir, "state", "evidence", "markers.jsonl"),
  jsonl([
    {
      id: "pitr-marker-a",
      txid: fakeUuid(41).replaceAll("-", ""),
      written_at: at(10),
    },
    {
      id: "pitr-marker-b",
      txid: fakeUuid(42).replaceAll("-", ""),
      written_at: at(20),
    },
  ]),
);
write(
  join(runDir, "state", "evidence", "snapshot-taken-at"),
  `${atMicros(320, 200000)}\n`,
);
write(join(runDir, "state", "promoted-at"), `${at(120)}\n`);
write(join(runDir, "state", "window-ended-at"), `${at(310)}\n`);
write(
  join(runDir, "state", "stale-transport-probe.json"),
  JSON.stringify({ status: "rejected" }),
);
write(
  join(runDir, "state", "stale-agent-probe.json"),
  JSON.stringify({ status: "rejected" }),
);
write(
  join(runDir, "state", "owner-a-terms.tsv"),
  `${fakeUuid(1).replaceAll("-", "")}:worker-a:${workerAIncarnation}:${fakeUuid(9).replaceAll("-", "")}:1\n`,
);
write(
  join(runDir, "state", "stale-scheduler-term"),
  `sched-a:${schedulerAIncarnation}:1\n`,
);
write(
  join(runDir, "state", "evidence", "failure-domain.txt"),
  "failure_domain=fd-b\nalias=fd-beta\n",
);

write(
  join(peerDir, "isolation", "isolation.json"),
  JSON.stringify({
    outage_declared_at: atMicros(50, 300000),
    isolated_at: at(51),
    failure_domain: "fd-a",
  }),
);
write(
  join(peerDir, "isolation", "outage-declared-at"),
  `${atMicros(50, 300000)}\n`,
);
write(join(peerDir, "isolation", "isolated-at"), `${at(51)}\n`);
write(join(peerDir, "isolation", "rto-started-at"), `${at(50)}\n`);
write(
  join(peerDir, "isolation", "isolated-primary-writes.jsonl"),
  jsonl(
    [126, 127, 128, 129, 130, 131].map((second) => ({
      at: at(second),
      accepted: false,
    })),
  ),
);
write(
  join(peerDir, "isolation", "active-load.json"),
  `${JSON.stringify({
    captured_at: atMicros(50, 234000),
    queued_outbox_count: nodes.length,
    commands: commands.slice(0, nodes.length).map((command) => ({
      command_id: command.id,
      command_state: "dispatched",
      attempt_state: "sent",
      attempt_finished: true,
      last_telemetry_at: at(40),
    })),
  })}\n`,
);
write(
  join(peerDir, "pitr", "pitr-report.json"),
  `${JSON.stringify(
    {
      environment_id: environmentId,
      candidate_sha: candidateSha,
      marker_a: {
        txid: fakeUuid(41).replaceAll("-", ""),
        written_at: at(10),
      },
      restore_point_created_at: at(15),
      marker_b: {
        txid: fakeUuid(42).replaceAll("-", ""),
        written_at: at(20),
      },
      restore: {
        restored_at: at(200),
        marker_a_present: true,
        marker_b_present: false,
      },
    },
    null,
    2,
  )}\n`,
);
write(join(peerDir, "relay-a-failed-at"), `${at(180)}\n`);
write(join(peerDir, "rejoin-at"), `${at(205)}\n`);
write(join(peerDir, "final-freeze-at"), `${at(321)}\n`);
write(join(peerDir, "freeze-received-at"), `${at(322)}\n`);
write(join(peerDir, "evidence", "snapshot-taken-at"), `${at(323)}\n`);
write(
  join(peerDir, "post-rejoin-probes.jsonl"),
  jsonl([206, 207, 208].map((second) => ({ at: at(second), accepted: false }))),
);
write(
  join(peerDir, "evidence", "failure-domain.txt"),
  "failure_domain=fd-a\nalias=fd-alpha\n",
);

function instanceLine(service, startedSeconds, finishedSeconds, digestByte, tagged = false) {
  const finished =
    finishedSeconds === undefined
      ? "0001-01-01T00:00:00Z"
      : at(finishedSeconds);
  const digest = tagged
    ? `public-image-digest-sha256-${digestByte.repeat(64)}`
    : digestOf(digestByte);
  return `/${service}\t${digest}\t${at(startedSeconds)}\t${finished}\t${service}`;
}
const peerInstances = [
  instanceLine("postgres", 200, undefined, "d"),
  instanceLine("api", 30, 51, "a"),
  instanceLine("worker", 30, 51, "a"),
  instanceLine("scheduler", 30, 51, "a"),
  instanceLine("transportd", 30, 51, "b"),
  instanceLine("relay", 30, 182, "c"),
];
for (let index = 1; index <= 25; index += 1) {
  peerInstances.push(
    instanceLine(
      `agent-fd-a-${String(index).padStart(2, "0")}`,
      30,
      undefined,
      "e",
    ),
  );
}
write(
  join(peerDir, "evidence", "instances.tsv"),
  `${peerInstances.join("\n")}\n`,
);
const localInstances = [
  instanceLine("postgres", 60, undefined, "d"),
  instanceLine("api", 121, undefined, "a", true),
  instanceLine("worker", 121, undefined, "a"),
  instanceLine("scheduler", 121, undefined, "a"),
  instanceLine("transportd", 121, undefined, "b"),
  instanceLine("relay", 55, undefined, "c"),
];
for (let index = 1; index <= 25; index += 1) {
  localInstances.push(
    instanceLine(
      `agent-fd-b-${String(index).padStart(2, "0")}`,
      125,
      undefined,
      "e",
    ),
  );
}
write(
  join(runDir, "state", "evidence", "instances.tsv"),
  `${localInstances.join("\n")}\n`,
);

// ---------------------------------------------------------------------------
// Timeline, epoch history, and the run.
// ---------------------------------------------------------------------------

const timeline = [
  ["load_started", 0],
  ["marker_a_written", 10],
  ["restore_point_created", 15],
  ["marker_b_written", 20],
  ["primary_failure_injected", 50],
  ["old_primary_isolated", 51],
  ["api_instance_failed", 51],
  ["worker_instance_failed", 51],
  ["new_primary_writable", 120],
  ["new_primary_promoted", 120],
  ["gateway_traffic_transferred", 121],
  ["api_recovered", 122],
  ["worker_replacement_active", 122],
  ["worker_recovered", 123],
  ["dispatch_recovered", 124],
  ["load_stopped", 125],
  ["old_primary_write_rejected", 132],
  ["scheduler_a_paused", 140],
  ["scheduler_b_acquired", 142],
  ["scheduler_a_resumed", 143],
  ["stale_scheduler_commit_rejected", 144],
  ["owner_a_paused", 150],
  ["owner_b_acquired", 156],
  ["owner_a_resumed", 157],
  ["stale_transport_rejected", 158],
  ["bulk_disconnect_injected", 159],
  ["reconnect_started", 160],
  ["stale_agent_rejected", 161],
  ["reconnect_completed", 165],
  ["relay_a_failed", 180],
  ["relay_b_active", 185],
  ["direct_path_active", 190],
  ["direct_path_failed", 191],
  ["relay_path_active", 192],
  ["direct_path_recovered", 196],
  ["restore_verified", 205],
  ["outbox_claim_committed", 210],
  ["worker_crashed_before_send", 211],
  ["command_recovered", 215],
  ["transport_send_accepted", 220],
  ["worker_crashed_before_mark_sent", 221],
  ["command_reconciled", 225],
  ["result_received", 230],
  ["ingress_crashed_before_commit", 231],
  ["result_reconciled", 235],
  ["resource_sampling_stopped", 305],
  ["api_slo_measured", 310],
];
const timelinePath = join(runDir, "outbox", "timeline.jsonl");
const nanosecondTimelineEvents = new Set([
  "stale_scheduler_commit_rejected",
  "stale_transport_rejected",
  "stale_agent_rejected",
]);
write(
  timelinePath,
  jsonl(
    timeline.map(([eventId, seconds], index) => ({
      sequence: index + 1,
      timestamp: nanosecondTimelineEvents.has(eventId)
        ? atNanos(seconds, 123456789)
        : at(seconds),
      environment_id: environmentId,
      candidate_sha: candidateSha,
      event_id: eventId,
    })),
  ),
);

const node1Hex = nodes[0].nodeId.replaceAll("-", "");
const node2Hex = nodes[1].nodeId.replaceAll("-", "");
const initialOwnerConnections = nodes.map((_, index) =>
  fakeUuid(index + 1000).replaceAll("-", ""),
);
const initialOwnerHistory = nodes.map(
  (node, index) =>
    `${index + 1}:${node.nodeId.replaceAll("-", "")}:worker-a:${workerAIncarnation}:${initialOwnerConnections[index]}:1:${atMicros(150, 500000)}:${atMicros(1, index * 1000)}`,
);
const firstTakeoverHistoryId = nodes.length + 1;
const scenarioOwnerTerms = new Map();
const takeoverOwnerHistory = nodes.map((node, index) => {
  const nodeHex = node.nodeId.replaceAll("-", "");
  const term = {
    instance: "worker-b",
    incarnation: workerBIncarnation,
    connectionId: fakeUuid(index + 2000).replaceAll("-", ""),
    epoch: 2,
    registeredAt: atMicros(151, index * 1000),
  };
  term.history = `${firstTakeoverHistoryId + index}:${nodeHex}:${term.instance}:${term.incarnation}:${term.connectionId}:${term.epoch}:${atMicros(350, 0)}:${term.registeredAt}`;
  scenarioOwnerTerms.set(nodeHex, term);
  return term.history;
});
const node1Epoch2History = scenarioOwnerTerms.get(node1Hex).history;
const node2Epoch2History = scenarioOwnerTerms.get(node2Hex).history;
const preStormOwnerHistory = [...initialOwnerHistory, ...takeoverOwnerHistory];
const preStormFinalOwnerHistory = new Map(
  nodes.map((node) => [
    node.nodeId.replaceAll("-", ""),
    scenarioOwnerTerms.get(node.nodeId.replaceAll("-", "")).history,
  ]),
);
const reconnectOwnerTerms = new Map();
const stormReleaseHistory = [];
const stormRegistrationHistory = [];
let nextOwnerHistoryId = firstTakeoverHistoryId + takeoverOwnerHistory.length;
for (const [index, node] of nodes.entries()) {
  const nodeHex = node.nodeId.replaceAll("-", "");
  const previous = preStormFinalOwnerHistory.get(nodeHex).split(":");
  const previousEpoch = previous[5];
  const releaseAt = atMicros(159, 100000 + index * 1000);
  stormReleaseHistory.push(
    `${nextOwnerHistoryId + index}:${nodeHex}:${previous[2]}:${previous[3]}:${previous[4]}:${previousEpoch}:${releaseAt}:${releaseAt}`,
  );
  const term = {
    instance: "worker-b",
    incarnation: workerBIncarnation,
    connectionId: fakeUuid(index + 6000).replaceAll("-", ""),
    epoch: Number(previousEpoch) + 1,
    registeredAt: atMicros(160, 200000 + index * 1000),
    connectedAt: atMicros(160, 200000 + index * 1000),
  };
  term.history = `${nextOwnerHistoryId + nodes.length + index}:${nodeHex}:${term.instance}:${term.incarnation}:${term.connectionId}:${term.epoch}:${atMicros(350, 0)}:${term.registeredAt}`;
  stormRegistrationHistory.push(term.history);
  reconnectOwnerTerms.set(nodeHex, term);
}
nextOwnerHistoryId += nodes.length * 2;
const stormOwnerHistory = [
  ...stormReleaseHistory,
  ...stormRegistrationHistory,
];

// One node reconnects again after the storm snapshot. Its earlier storm term
// remains the recovery proof, while the final inventory and authority cut
// bind a later current term.
const node1ReconnectTerm = reconnectOwnerTerms.get(node1Hex);
node1ReconnectTerm.history = node1ReconnectTerm.history.replace(
  atMicros(350, 0),
  atMicros(350, 123456),
);
stormOwnerHistory[nodes.length] = node1ReconnectTerm.history;
const node1LaterRelease = `${nextOwnerHistoryId}:${node1Hex}:${node1ReconnectTerm.instance}:${node1ReconnectTerm.incarnation}:${node1ReconnectTerm.connectionId}:${node1ReconnectTerm.epoch}:${atMicros(200, 100000)}:${atMicros(200, 100000)}`;
nextOwnerHistoryId += 1;
const node1LaterTerm = {
  instance: "worker-b",
  incarnation: workerBIncarnation,
  connectionId: fakeUuid(7001).replaceAll("-", ""),
  epoch: node1ReconnectTerm.epoch + 1,
  registeredAt: atMicros(200, 200000),
  connectedAt: at(200),
};
node1LaterTerm.history = `${nextOwnerHistoryId}:${node1Hex}:${node1LaterTerm.instance}:${node1LaterTerm.incarnation}:${node1LaterTerm.connectionId}:${node1LaterTerm.epoch}:${atMicros(350, 0)}:${node1LaterTerm.registeredAt}`;
const node1ShortAcquireHistory = node1ReconnectTerm.history;
const node1ShortReleaseHistory = node1LaterRelease;
const node1FinalOwnerHistory = node1LaterTerm.history;
const completeOwnerHistory = [
  ...preStormOwnerHistory,
  ...stormOwnerHistory,
  node1LaterRelease,
  node1LaterTerm.history,
];
const fencingHistoryPath = join(runDir, "outbox", "fencing-history.jsonl");
write(
  fencingHistoryPath,
  completeOwnerHistory.join("\n") + "\n",
);
const finalOwnerHistory = new Map(
  nodes.map((node) => {
    const nodeHex = node.nodeId.replaceAll("-", "");
    return [
      nodeHex,
      nodeHex === node1Hex
        ? node1LaterTerm.history
        : reconnectOwnerTerms.get(nodeHex).history,
    ];
  }),
);
const finalOwnerTerms = new Map(
  nodes.map((node) => {
    const nodeHex = node.nodeId.replaceAll("-", "");
    const term =
      nodeHex === node1Hex ? node1LaterTerm : reconnectOwnerTerms.get(nodeHex);
    return [nodeHex, term];
  }),
);
for (const [index, observation] of finalSessionInventory.observations.entries()) {
  const nodeHex = nodes[index].nodeId.replaceAll("-", "");
  const term = finalOwnerTerms.get(nodeHex);
  observation.owner_instance_id = term.instance;
  observation.owner_incarnation = term.incarnation;
  observation.connection_id = term.connectionId;
  observation.owner_epoch = term.epoch;
  observation.owner_lease_until = at(350);
  observation.connected_at = term.connectedAt;
}
const reconnectSessionInventory = structuredClone(finalSessionInventory);
for (const [index, observation] of reconnectSessionInventory.observations.entries()) {
  const nodeHex = nodes[index].nodeId.replaceAll("-", "");
  const term = reconnectOwnerTerms.get(nodeHex);
  observation.owner_instance_id = term.instance;
  observation.owner_incarnation = term.incarnation;
  observation.connection_id = term.connectionId;
  observation.owner_epoch = term.epoch;
  observation.owner_lease_until = at(350);
  observation.connected_at = term.connectedAt;
}
write(beforeFinalSessionsPath, JSON.stringify(finalSessionInventory));
write(afterFinalSessionsPath, JSON.stringify(finalSessionInventory));
write(finalSessionsPath, JSON.stringify(finalSessionInventory));
write(reconnectSessionsPath, JSON.stringify(reconnectSessionInventory));
const relayBNodeIdPath = join(runDir, "state", "relay-b-node-id");
const relayBObservationPath = join(
  runDir,
  "state",
  "relay-b-observation.json",
);
const relayBObservation = structuredClone(
  reconnectSessionInventory.observations[0],
);
Object.assign(relayBObservation, {
  matched: true,
  path: "relay",
  path_detail: "iroh/relay-b",
});
write(relayBNodeIdPath, `${nodes[0].nodeId}\n`);
write(
  relayBObservationPath,
  JSON.stringify({
    mode: "node_connection",
    expected_path: "relay",
    all_matched: true,
    observations: [relayBObservation],
  }),
);
write(
  join(runDir, "state", "relay-b-before-command.json"),
  JSON.stringify({
    mode: "node_connection",
    expected_path: "relay",
    all_matched: true,
    observations: [relayBObservation],
  }),
);
const relayAObservation = structuredClone(relayBObservation);
relayAObservation.path_detail = "iroh/relay-a";
write(
  join(runDir, "outbox", "relay-pre-fault", "relay-a-before-command.json"),
  JSON.stringify({
    mode: "node_connection",
    expected_path: "relay",
    all_matched: true,
    observations: [relayAObservation],
  }),
);
write(
  join(runDir, "outbox", "relay-pre-fault", "relay-a-observation.json"),
  JSON.stringify({
    mode: "node_connection",
    expected_path: "relay",
    all_matched: true,
    observations: [relayAObservation],
  }),
);
write(
  join(runDir, "outbox", "relay-pre-fault", "observed-at"),
  `${at(179)}\n`,
);
write(
  join(runDir, "outbox", "relay-pre-fault", "node-id"),
  `${nodes[0].nodeId}\n`,
);
write(
  join(
    runDir,
    "outbox",
    "relay-pre-fault",
    "relay-a-only-readiness.json",
  ),
  JSON.stringify({
    schema_version: "ocservia.g6-relay-topology.v1",
    environment_id: environmentId,
    candidate_sha: candidateSha,
    node_id: nodes[0].nodeId,
    agent_service: "agent-fd-a-01",
    network_name: "g6-rd-test_relay-a-only",
    network_internal: true,
    agent_default_network_connected: false,
    relay_alias: "relay-a",
    topology_ready_at: at(173),
  }),
);
write(
  join(runDir, "outbox", "relay-pre-fault", "relay-b-disabled.json"),
  JSON.stringify({
    schema_version: "ocservia.g6-relay-state.v1",
    environment_id: environmentId,
    candidate_sha: candidateSha,
    node_id: nodes[0].nodeId,
    relay: "relay-b",
    state: "stopped",
    disabled_at: at(174),
  }),
);
write(
  join(runDir, "outbox", "relay-pre-fault", "relay-a-command-proof.json"),
  JSON.stringify({
    observed_at: atMicros(178, 500000),
    command_id: relayPreFaultCommandId,
    idempotency_key: relayPreFaultCommandKey,
    node_id: nodes[0].nodeId,
    command_state: "succeeded",
    result_count: 1,
    result_state: "succeeded",
    agent_result_completed_at: at(178),
    result_observed_at: at(178),
  }),
);
write(
  join(runDir, "outbox", "relay-pre-fault", "relay-a-dispatch-proof.json"),
  JSON.stringify({
    event_type: "command_frame_written",
    message: "command frame written to authenticated Agent session",
    command_id: relayPreFaultCommandId.replaceAll("-", ""),
    node_id: nodes[0].nodeId.replaceAll("-", ""),
    owner_fence_id: relayAObservation.owner_fence_id.replaceAll("-", ""),
    connection_id: relayAObservation.connection_id.replaceAll("-", ""),
    owner_epoch: relayAObservation.owner_epoch,
    path: "relay",
    path_detail: "iroh/relay-a",
  }),
);
write(
  join(peerDir, "relay-fault-cut.json"),
  JSON.stringify({
    cut_at: at(180),
    node_id: nodes[0].nodeId.replaceAll("-", ""),
    owner_instance: relayAObservation.owner_instance_id,
    owner_incarnation: relayAObservation.owner_incarnation,
    connection_id: relayAObservation.connection_id.replaceAll("-", ""),
    owner_epoch: relayAObservation.owner_epoch,
    authority_lease_until: at(350),
  }),
);
write(join(runDir, "state", "relay-b-active-at"), `${at(185)}\n`);
write(
  join(runDir, "state", "relay-b-started.json"),
  JSON.stringify({
    schema_version: "ocservia.g6-relay-state.v1",
    environment_id: environmentId,
    candidate_sha: candidateSha,
    node_id: nodes[0].nodeId,
    relay: "relay-b",
    state: "healthy",
    started_at: atMicros(180, 500000),
  }),
);
write(
  join(runDir, "state", "relay-command-proof.json"),
  `${JSON.stringify({
    observed_at: atMicros(184, 500000),
    command_id: relayCommandId,
    idempotency_key: relayCommandKey,
    node_id: nodes[0].nodeId,
    command_state: "succeeded",
    result_count: 1,
    result_state: "succeeded",
    agent_result_completed_at: at(184),
    result_observed_at: at(184),
  })}\n`,
);
write(
  join(runDir, "state", "relay-dispatch-proof.json"),
  `${JSON.stringify({
    event_type: "command_frame_written",
    message: "command frame written to authenticated Agent session",
    command_id: relayCommandId.replaceAll("-", ""),
    node_id: nodes[0].nodeId.replaceAll("-", ""),
    owner_fence_id: relayBObservation.owner_fence_id.replaceAll("-", ""),
    connection_id: relayBObservation.connection_id.replaceAll("-", ""),
    owner_epoch: relayBObservation.owner_epoch,
    path: "relay",
    path_detail: "iroh/relay-b",
  })}\n`,
);
const ownerBTermsPath = join(runDir, "state", "owner-b-terms.tsv");
const ownerReplacementSessionsPath = join(
  runDir,
  "state",
  "owner-replacement-sessions.json",
);
const ownerReplacementObservations = nodes.map((node, index) => {
  const nodeHex = node.nodeId.replaceAll("-", "");
  const term = scenarioOwnerTerms.get(nodeHex);
  return {
    ...finalSessionInventory.observations[index],
    matched: true,
    path: "direct",
    path_detail: "iroh/direct",
    owner_instance_id: term.instance,
    owner_incarnation: term.incarnation,
    connection_id: term.connectionId,
    owner_epoch: term.epoch,
    connected_at: atMicros(151, 100000 + index * 1000),
    last_seen: atMicros(155, index * 1000),
  };
});
write(
  ownerBTermsPath,
  `${nodes
    .map((node) => {
      const nodeHex = node.nodeId.replaceAll("-", "");
      const term = scenarioOwnerTerms.get(nodeHex);
      return [
        nodeHex,
        term.instance,
        term.incarnation,
        term.connectionId,
        term.epoch,
        term.registeredAt,
      ].join("\t");
    })
    .join("\n")}\n`,
);
write(
  ownerReplacementSessionsPath,
  JSON.stringify({
    mode: "node_connection",
    expected_path: "any",
    all_matched: true,
    observations: ownerReplacementObservations,
  }),
);

const initialLeaderHistory = `1001:sched-a:${schedulerAIncarnation}:1:${atMicros(21, 0)}:${atMicros(1, 0)}`;
const renewedLeaderHistory = `1002:sched-a:${schedulerAIncarnation}:1:${atMicros(41, 0)}:${atMicros(21, 0)}`;
const successorLeaderHistory = `1003:sched-b:${schedulerBIncarnation}:2:${atMicros(62, 0)}:${atMicros(42, 0)}`;
const firstSuccessorRenewal = `1004:sched-b:${schedulerBIncarnation}:2:${atMicros(82, 0)}:${atMicros(43, 0)}`;
const laterSuccessorRenewal = `1005:sched-b:${schedulerBIncarnation}:2:${atMicros(200, 0)}:${atMicros(100, 0)}`;
const finalLeaderHistory = `1006:sched-b:${schedulerBIncarnation}:2:${atMicros(350, 0)}:${atMicros(300, 0)}`;
const leadershipHistoryPath = join(
  runDir,
  "outbox",
  "leadership-history.jsonl",
);
const completeSchedulerHistory = [
  initialLeaderHistory,
  renewedLeaderHistory,
  successorLeaderHistory,
  firstSuccessorRenewal,
  laterSuccessorRenewal,
  finalLeaderHistory,
];
write(
  leadershipHistoryPath,
  completeSchedulerHistory.join("\n") + "\n",
);
const schedulerAMaintenance = `2001:sched-a:${schedulerAIncarnation}:1:${atMicros(2, 500000)}`;
const schedulerBMaintenance = `2002:sched-b:${schedulerBIncarnation}:2:${atMicros(43, 500000)}`;
const schedulerMaintenanceObservedAt = atMicros(43, 500001);
const completeSchedulerMaintenanceHistory = [
  schedulerAMaintenance,
  schedulerBMaintenance,
];
const schedulerMaintenanceHistoryPath = join(
  runDir,
  "outbox",
  "scheduler-maintenance-history.jsonl",
);
write(
  schedulerMaintenanceHistoryPath,
  completeSchedulerMaintenanceHistory.join("\n") + "\n",
);
const schedulerReplacementTermPath = join(
  runDir,
  "state",
  "scheduler-replacement-term",
);
write(
  schedulerReplacementTermPath,
  `sched-b:${schedulerBIncarnation}:2\n`,
);
const schedulerMaintenanceObservationPath = join(
  runDir,
  "state",
  "scheduler-maintenance-observation.json",
);
const schedulerMaintenanceObservation = {
  maintenance_id: "2002",
  instance_id: "sched-b",
  incarnation: schedulerBIncarnation,
  epoch: 2,
  marker_completed_at: atMicros(43, 500000),
  committed_observed_at: schedulerMaintenanceObservedAt,
};
write(
  schedulerMaintenanceObservationPath,
  JSON.stringify(schedulerMaintenanceObservation),
);
const finalAuthorityCutPath = join(
  runDir,
  "state",
  "final-authority-cut.json",
);
write(
  finalAuthorityCutPath,
  JSON.stringify({
    cut_at: atMicros(320, 200000),
    owner_history: completeOwnerHistory,
    scheduler_history: completeSchedulerHistory,
    scheduler_maintenance_history: completeSchedulerMaintenanceHistory,
    owners: nodes.map((node) => {
      const nodeHex = node.nodeId.replaceAll("-", "");
      const term = finalOwnerTerms.get(nodeHex);
      return {
        node_hex: nodeHex,
        owner_instance_id: term.instance,
        owner_incarnation: term.incarnation,
        connection_id: term.connectionId,
        owner_epoch: term.epoch,
        lease_until: at(350),
        history: finalOwnerHistory.get(nodeHex),
      };
    }),
    leader: {
      instance_id: "sched-b",
      incarnation: schedulerBIncarnation,
      epoch: 2,
      lease_until: at(350),
      history: finalLeaderHistory.slice(finalLeaderHistory.indexOf(":") + 1),
    },
  }),
);

// ---------------------------------------------------------------------------
// Run the builder and verify both authorities.
// ---------------------------------------------------------------------------

function runBuilder(outDir, authority) {
  mkdirSync(outDir, { recursive: true });
  writeFileSync(
    join(outDir, "raw-source-inventory.json"),
    `${JSON.stringify({
      schema_version: "ocservia.g6-raw-source-inventory.v1",
      sources: [{ failure_domain: "fd-a", artifact_id: "1001" }],
    })}\n`,
  );
  const result = spawnSync(
    process.execPath,
    [
      join(root, "scripts", "build-g6-evidence.mjs"),
      "--run-dir",
      runDir,
      "--peer-dir",
      peerDir,
      "--out-dir",
      outDir,
      "--slo",
      join(root, "docs", "acceptance", "g6-slo.yaml"),
      "--environment-id",
      environmentId,
      "--candidate-sha",
      candidateSha,
      "--authority",
      authority,
      "--failure-domain-class",
      "multi_host",
      "--run-id",
      "test-run",
    ],
    { encoding: "utf8" },
  );
  if (result.status !== 0) {
    throw new Error(
      `builder failed (${authority}): ${result.stderr}${result.stdout}`,
    );
  }
}

function expectBuilderFailure(outDir, expectedMessage, expectedDetails = {}) {
  const result = spawnSync(
    process.execPath,
    [
      join(root, "scripts", "build-g6-evidence.mjs"),
      "--run-dir",
      runDir,
      "--peer-dir",
      peerDir,
      "--out-dir",
      outDir,
      "--slo",
      join(root, "docs", "acceptance", "g6-slo.yaml"),
      "--environment-id",
      environmentId,
      "--candidate-sha",
      candidateSha,
      "--authority",
      "production_readiness",
      "--failure-domain-class",
      "multi_host",
      "--run-id",
      "test-run",
    ],
    { encoding: "utf8" },
  );
  const output = `${result.stderr}${result.stdout}`;
  if (result.status === 0 || !output.includes(expectedMessage)) {
    throw new Error(
      `builder did not reject the invalid producer state: ${output}`,
    );
  }
  const errorResult = JSON.parse(
    readFileSync(join(outDir, "builder-error.json"), "utf8"),
  );
  if (
    !errorResult.reason.includes(expectedMessage) ||
    Object.entries(expectedDetails).some(
      ([key, value]) => errorResult[key] !== value,
    )
  ) {
    throw new Error(
      `builder did not preserve structured failure details: ${JSON.stringify(errorResult)}`,
    );
  }
}

function verifyBundle(outDir, authority) {
  return verifyG6({
    sloText: readFileSync(
      join(root, "docs", "acceptance", "g6-slo.yaml"),
      "utf8",
    ),
    evidenceText: readFileSync(join(outDir, "evidence.json"), "utf8"),
    topologyText: readFileSync(join(outDir, "topology.json"), "utf8"),
    manifestText: readFileSync(join(outDir, "release-manifest.json"), "utf8"),
    artifactRoot: outDir,
    expectedAuthority: authority,
    expectedEnvironmentId: environmentId,
    expectedFailureDomainClass: "multi_host",
  });
}

function expectTamperedBundleFailure(
  sourceDir,
  name,
  artifactName,
  mutate,
  expectedMessage,
) {
  const outDir = join(work, name);
  cpSync(sourceDir, outDir, { recursive: true });
  const artifactPath = join(outDir, artifactName);
  const mutated = mutate(readFileSync(artifactPath, "utf8"));
  write(artifactPath, mutated);

  const evidencePath = join(outDir, "evidence.json");
  const evidence = JSON.parse(readFileSync(evidencePath, "utf8"));
  const artifact = evidence.artifacts.find(
    (entry) => entry.name === artifactName,
  );
  if (!artifact) throw new Error(`missing artifact ${artifactName}`);
  const previousDigest = artifact.digest;
  artifact.digest = sha256Digest(mutated);
  for (const result of [
    ...Object.values(evidence.measurements),
    ...Object.values(evidence.observations),
  ]) {
    if (result.source_artifact_digest === previousDigest) {
      result.source_artifact_digest = artifact.digest;
    }
  }
  write(evidencePath, `${JSON.stringify(evidence, null, 2)}\n`);

  let rejected;
  try {
    verifyBundle(outDir, "production_readiness");
  } catch (error) {
    rejected = error;
  }
  if (!rejected?.message.includes(expectedMessage)) {
    throw new Error(
      `${name} was not rejected for ${expectedMessage}: ${rejected?.message ?? "PASS"}`,
    );
  }
}

try {
  const productionDir = join(work, "production-bundle");
  runBuilder(productionDir, "production_readiness");
  const builtEpochEvents = readFileSync(
    join(productionDir, "epoch-events.jsonl"),
    "utf8",
  )
    .trimEnd()
    .split("\n")
    .map((line) => JSON.parse(line));
  if (
    builtEpochEvents.some(
      (event, index) =>
        !/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{6}(?:\d{3})?Z$/.test(
          event.timestamp,
        ) ||
        (index > 0 &&
          event.timestamp < builtEpochEvents[index - 1].timestamp),
    )
  ) {
    throw new Error(
      "epoch event timestamps must retain source precision and be nondecreasing",
    );
  }
  for (const event of builtEpochEvents.filter(
    (record) =>
      (record.subject === "connection_owner" &&
        record.event_type === "owner_accept") ||
      (record.subject === "scheduler" &&
        record.event_type === "leader_commit" &&
        record.accepted === false),
  )) {
    if (!event.timestamp.endsWith(".123456789Z")) {
      throw new Error(
        "timeline-derived epoch events must preserve nanosecond ordering",
      );
    }
  }
  const shortOwnerLifecycle = builtEpochEvents.filter(
    (event) =>
      event.node === node1Hex &&
      ((event.epoch === 3 &&
          ["owner_registered", "owner_retired"].includes(event.event_type)) ||
        (event.epoch === 4 && event.event_type === "owner_registered")),
  );
  const shortOwnerTransitions = shortOwnerLifecycle.map(
    (event) => `${event.event_type}:${event.epoch}`,
  );
  if (
    shortOwnerTransitions.join(",") !==
    "owner_registered:3,owner_retired:3,owner_registered:4"
  ) {
    throw new Error(
      `short owner lifecycle lost journal causality: ${shortOwnerTransitions.join(",")}`,
    );
  }
  if (
    shortOwnerLifecycle.some(
      (event, index) =>
        index > 0 &&
        (event.sequence <= shortOwnerLifecycle[index - 1].sequence ||
          Date.parse(event.timestamp) <
            Date.parse(shortOwnerLifecycle[index - 1].timestamp)),
    )
  ) {
    throw new Error("short owner lifecycle is not causally ordered");
  }
  const shortRegistration = shortOwnerLifecycle[0];
  const shortRetirement = shortOwnerLifecycle[1];
  if (
    shortRegistration.timestamp !== atMicros(160, 200000) ||
    shortRetirement.timestamp !== atMicros(200, 100000)
  ) {
    throw new Error(
      "the short-lived owner lifecycle lost its acquisition or release timestamp",
    );
  }
  if (
    shortRegistration.lease_until !== atMicros(350, 123456) ||
    Date.parse(shortRegistration.lease_until) <=
      Date.parse(shortRegistration.timestamp)
  ) {
    throw new Error(
      "the immediate Release overwrote the short-lived owner's acquisition lease",
    );
  }
  if (
    builtEpochEvents.some(
      (event) =>
        event.node === node1Hex &&
        event.epoch === node1LaterTerm.epoch &&
        ["owner_lease_expired", "owner_retired"].includes(event.event_type),
    )
  ) {
    throw new Error("the final owner epoch must remain active");
  }
  const scenarioExpiredNodes = new Set(
    builtEpochEvents
      .filter(
        (event) =>
          event.subject === "connection_owner" &&
          event.event_type === "owner_lease_expired" &&
          event.epoch === 1 &&
          event.timestamp === atMicros(150, 500000),
      )
      .map((event) => event.node),
  );
  if (
    scenarioExpiredNodes.size !== nodes.length ||
    nodes.some((node) => !scenarioExpiredNodes.has(node.nodeId.replaceAll("-", "")))
  ) {
    throw new Error(
      "the formal owner scenario must expire every managed-node lease",
    );
  }
  const scenarioConnectedNodes = new Set(
    builtEpochEvents
      .filter(
        (event) =>
          event.subject === "connection_owner" &&
          event.event_type === "owner_registered" &&
          event.epoch === 2 &&
          typeof event.session_connected_at === "string",
      )
      .map((event) => event.node),
  );
  if (
    scenarioConnectedNodes.size !== nodes.length ||
    nodes.some(
      (node) => !scenarioConnectedNodes.has(node.nodeId.replaceAll("-", "")),
    )
  ) {
    throw new Error(
      "the formal owner scenario must bind every successor to a live session",
    );
  }
  const successorMaintenanceCommits = builtEpochEvents.filter(
    (event) =>
      event.subject === "scheduler" &&
      event.event_type === "leader_commit" &&
      event.instance === "sched-b" &&
      event.epoch === 2 &&
      event.accepted === true,
  );
  if (
    successorMaintenanceCommits.length !== 1 ||
    successorMaintenanceCommits[0].timestamp !==
      schedulerMaintenanceObservedAt ||
    successorMaintenanceCommits[0].marker_completed_at !==
      atMicros(43, 500000) ||
    successorMaintenanceCommits[0].incarnation !== schedulerBIncarnation ||
    successorMaintenanceCommits[0].maintenance_id !== "2002"
  ) {
    throw new Error(
      "scheduler takeover must use the first exact-term fenced maintenance completion",
    );
  }
  const verdict = verifyBundle(productionDir, "production_readiness");
  const failedMetrics = Object.entries(verdict.measurement_results).filter(
    ([, result]) => !result.passed,
  );
  if (failedMetrics.length > 0) {
    throw new Error(
      `production bundle metrics failed: ${failedMetrics.map(([name, result]) => `${name}=${result.actual}`).join(", ")}`,
    );
  }
  const failedObservations = Object.entries(verdict.observation_results).filter(
    ([, result]) => !result.passed,
  );
  if (failedObservations.length > 0) {
    throw new Error(
      `production bundle observations failed: ${failedObservations.map(([name]) => name).join(", ")}`,
    );
  }
  if (!verdict.passed) {
    throw new Error(
      `production bundle must pass: ${verdict.failure_reasons.join("; ")}`,
    );
  }
  const builtHttpLines = readFileSync(
    join(productionDir, "http-samples.csv"),
    "utf8",
  )
    .trimEnd()
    .split("\n");
  const builtHttpHeader = builtHttpLines[0].split(",");
  const builtHttpRows = builtHttpLines.slice(1).map((line) => line.split(","));
  const builtHttpColumn = (name) => builtHttpHeader.indexOf(name);
  const staleRetryRows = builtHttpRows.filter(
    (row) =>
      row[builtHttpColumn("idempotency_key")] ===
      "g6-window-test-run-opening-0",
  );
  if (
    staleRetryRows.length !== 2 ||
    staleRetryRows[0][builtHttpColumn("http_status")] !== "409" ||
    staleRetryRows[1][builtHttpColumn("http_status")] !== "202" ||
    staleRetryRows[0][builtHttpColumn("attempt_ordinal")] !== "1" ||
    staleRetryRows[1][builtHttpColumn("attempt_ordinal")] !== "2" ||
    staleRetryRows[0][builtHttpColumn("requested_revision")] !== "10" ||
    staleRetryRows[1][builtHttpColumn("requested_revision")] !== "11" ||
    staleRetryRows[0][builtHttpColumn("request_id")] ===
      staleRetryRows[1][builtHttpColumn("request_id")] ||
    verdict.measurement_results.enqueue_success_ratio.actual !== 1 ||
    verdict.measurement_results.enqueue_success_ratio.sample_count !==
      enqueueLog.filter((sample) => sample.status >= 200 && sample.status < 300)
        .length
  ) {
    throw new Error(
      "builder did not preserve one bounded stale-revision logical request and both HTTP attempts",
    );
  }
  const builtAudit = JSON.parse(
    readFileSync(join(productionDir, "audit-correlation.json"), "utf8"),
  );
  const acceptedRequestIdByCommand = new Map(
    enqueueLog
      .filter((sample) => sample.status >= 200 && sample.status < 300)
      .map((sample) => [sample.command_id, sample.attempt_request_id]),
  );
  if (
    builtAudit.writes.length !== acceptedRequestIdByCommand.size ||
    builtAudit.writes.some((writeRecord) => {
      const requestId = acceptedRequestIdByCommand.get(writeRecord.write_id);
      return (
        requestId === undefined ||
        writeRecord.intent_request_id !== requestId ||
        writeRecord.result_request_id !== requestId
      );
    })
  ) {
    throw new Error(
      "builder did not bind audit intent and result records to the terminal accepted HTTP request",
    );
  }
  const rawAuditPath = join(runDir, "state", "evidence", "audit.jsonl");
  const originalRawAudit = readFileSync(rawAuditPath, "utf8");
  const reboundRawAudit = originalRawAudit
    .trimEnd()
    .split("\n")
    .map(JSON.parse);
  const firstAcceptedAudit = reboundRawAudit.find(
    (row) => row.command_id === enqueueLog.find((sample) => sample.status === 202).command_id,
  );
  firstAcceptedAudit.request_id = enqueueLog.find(
    (sample) => sample.status === 202 && sample.command_id !== firstAcceptedAudit.command_id,
  ).attempt_request_id;
  write(rawAuditPath, jsonl(reboundRawAudit));
  expectBuilderFailure(
    join(work, "rebound-audit-request-id-bundle"),
    "does not retain terminal HTTP request_id",
  );
  write(rawAuditPath, originalRawAudit);
  if (
    verdict.measurement_results.connection_owner_takeover_seconds.sample_count <
    nodes.length
  ) {
    throw new Error(
      "connection-owner takeover metric did not cover the full managed population",
    );
  }
  const builtCommandTrace = readFileSync(
    join(productionDir, "command-trace.jsonl"),
    "utf8",
  )
    .trimEnd()
    .split("\n")
    .map((line) => JSON.parse(line));
  const earliestEnqueueTimestamp = builtCommandTrace
    .filter((record) => record.record_type === "enqueued")
    .map((record) => record.timestamp)
    .sort()[0];
  if (
    builtCommandTrace[0]?.record_type !== "profile" ||
    builtCommandTrace[0].sequence !== 1 ||
    builtCommandTrace[0].timestamp !== earliestEnqueueTimestamp
  ) {
    throw new Error(
      "builder did not anchor the leading command-trace profile to the complete traced population",
    );
  }
  const inflightSnapshot = builtCommandTrace.find(
    (record) => record.record_type === "inflight_snapshot",
  );
  if (
    inflightSnapshot?.expected_count !== nodes.length ||
    inflightSnapshot.result_count !== 0 ||
    inflightSnapshot.commands?.length !== nodes.length ||
    new Set(inflightSnapshot.commands.map((command) => command.node_id)).size !==
      nodes.length
  ) {
    throw new Error(
      "builder did not bind the exact all-fleet inflight snapshot into the command trace",
    );
  }
  const rejectedTraceResult = builtCommandTrace.find(
      (record) =>
        record.record_type === "result" &&
        record.command_id === rejectedCommandId,
    );
  if (rejectedTraceResult?.outcome !== "failed") {
    throw new Error(
      "builder must map an accepted command's rejected terminal result to failed",
    );
  }
  const openingSnapshotPath = join(
    runDir,
    "state",
    "window-opening-active.json",
  );
  const originalOpeningSnapshot = readFileSync(openingSnapshotPath, "utf8");
  rmSync(openingSnapshotPath);
  expectBuilderFailure(
    join(work, "missing-opening-snapshot-bundle"),
    "required input is missing or empty",
  );
  write(openingSnapshotPath, originalOpeningSnapshot);
  const incompleteOpeningSnapshot = JSON.parse(originalOpeningSnapshot);
  incompleteOpeningSnapshot.commands.pop();
  write(openingSnapshotPath, `${JSON.stringify(incompleteOpeningSnapshot)}\n`);
  expectBuilderFailure(
    join(work, "incomplete-opening-snapshot-bundle"),
    "window opening inflight snapshot is not the exact managed population",
  );
  const unknownOpeningSnapshot = JSON.parse(originalOpeningSnapshot);
  unknownOpeningSnapshot.commands[0].state = "unknown";
  write(openingSnapshotPath, `${JSON.stringify(unknownOpeningSnapshot)}\n`);
  runBuilder(join(work, "unknown-opening-snapshot-bundle"), "production_readiness");
  const unsentOpeningSnapshot = JSON.parse(originalOpeningSnapshot);
  unsentOpeningSnapshot.commands[0].sent_attempt_count = 0;
  write(openingSnapshotPath, `${JSON.stringify(unsentOpeningSnapshot)}\n`);
  expectBuilderFailure(
    join(work, "unsent-opening-snapshot-bundle"),
    "window opening inflight snapshot command 1 is malformed",
  );
  const tiedOpeningSnapshot = JSON.parse(originalOpeningSnapshot);
  tiedOpeningSnapshot.captured_at = commands[0].updated_at;
  write(openingSnapshotPath, `${JSON.stringify(tiedOpeningSnapshot)}\n`);
  expectBuilderFailure(
    join(work, "terminal-tie-opening-snapshot-bundle"),
    "was not transport-accepted and result-free at the snapshot boundary",
  );
  write(openingSnapshotPath, originalOpeningSnapshot);
  const builtSessionInventory = JSON.parse(
    readFileSync(join(productionDir, "agent-sessions.json"), "utf8"),
  );
  const laterFinalSession = builtSessionInventory.sessions.find(
    (session) => session.node.replaceAll("-", "") === node1Hex,
  );
  if (
    laterFinalSession?.reconnected_at !== node1ReconnectTerm.registeredAt ||
    laterFinalSession?.connected_at !== node1LaterTerm.connectedAt ||
    laterFinalSession?.reconnect_owner_epoch !== node1ReconnectTerm.epoch ||
    laterFinalSession?.owner_epoch !== node1LaterTerm.epoch
  ) {
    throw new Error(
      "builder did not preserve the earlier storm term separately from the later final connection",
    );
  }
  const expectedSessionByNode = new Map(
    finalSessionInventory.observations.map((observation) => [
      observation.node_id,
      observation,
    ]),
  );
  const expectedReconnectByNode = new Map(
    reconnectSessionInventory.observations.map((observation) => [
      observation.node_id,
      observation,
    ]),
  );
  const sourceAuthorityCut = JSON.parse(
    readFileSync(finalAuthorityCutPath, "utf8"),
  );
  const builtAuthorityCut = JSON.parse(
    readFileSync(join(productionDir, "authority-cut.json"), "utf8"),
  );
  const builtEvidence = JSON.parse(
    readFileSync(join(productionDir, "evidence.json"), "utf8"),
  );
  const builtRelayRecords = readFileSync(
    join(productionDir, "relay-transitions.jsonl"),
    "utf8",
  )
    .trimEnd()
    .split("\n")
    .map((line) => JSON.parse(line));
  const builtRelayTraffic = builtRelayRecords.find(
    (record) =>
      record.event_type === "path_active" && record.relay === "relay-b",
  );
  const builtPreFaultRelayTraffic = builtRelayRecords.find(
    (record) =>
      record.event_type === "path_active" && record.relay === "relay-a",
  );
  if (
    builtRelayTraffic?.session_id !== nodes[0].nodeId.replaceAll("-", "") ||
    builtRelayTraffic?.path !== "relay" ||
    builtRelayTraffic?.relay !== "relay-b" ||
    builtRelayTraffic?.authenticated !== true ||
    builtRelayTraffic?.owner_fence_id !==
      relayBObservation.owner_fence_id.replaceAll("-", "") ||
    builtRelayTraffic?.connection_id !==
      relayBObservation.connection_id.replaceAll("-", "") ||
    builtRelayTraffic?.command_id !== relayCommandId ||
    builtRelayTraffic?.command_idempotency_key !== relayCommandKey ||
    builtRelayTraffic?.effect_id !== relayEffectId ||
    builtRelayTraffic?.effect_idempotency_key !==
      `g6-journal-key-${relayEffectKey}` ||
    builtRelayTraffic?.result_observed_at !== at(184) ||
    builtRelayTraffic?.relay_b_started_at !== atMicros(180, 500000) ||
    !builtRelayTraffic?.negotiated_capabilities?.includes("ocserv.fencing.v2")
  ) {
    throw new Error(
      "builder did not derive authenticated relay-b traffic from the chosen raw probe",
    );
  }
  if (
    builtPreFaultRelayTraffic?.session_id !==
      nodes[0].nodeId.replaceAll("-", "") ||
    builtPreFaultRelayTraffic?.path !== "relay" ||
    builtPreFaultRelayTraffic?.relay !== "relay-a" ||
    builtPreFaultRelayTraffic?.authenticated !== true ||
    builtPreFaultRelayTraffic?.command_id !== relayPreFaultCommandId ||
    builtPreFaultRelayTraffic?.command_idempotency_key !==
      relayPreFaultCommandKey ||
    builtPreFaultRelayTraffic?.effect_id !== relayPreFaultEffectId ||
    builtPreFaultRelayTraffic?.effect_idempotency_key !==
      `g6-journal-key-${relayPreFaultEffectKey}` ||
    builtPreFaultRelayTraffic?.result_observed_at !== at(178) ||
    builtPreFaultRelayTraffic?.topology_mode !== "relay-a-only" ||
    builtPreFaultRelayTraffic?.topology_network_name !==
      "g6-rd-test_relay-a-only" ||
    builtPreFaultRelayTraffic?.topology_agent_service !== "agent-fd-a-01" ||
    builtPreFaultRelayTraffic?.topology_network_internal !== true ||
    builtPreFaultRelayTraffic?.topology_agent_default_network_connected !==
      false ||
    builtPreFaultRelayTraffic?.topology_ready_at !== at(173) ||
    builtPreFaultRelayTraffic?.relay_b_disabled_at !== at(174)
  ) {
    throw new Error(
      "builder did not derive authenticated relay-a traffic from the pre-fault command",
    );
  }
  const sourceInventory = JSON.parse(
    readFileSync(join(productionDir, "builder-source-inventory.json"), "utf8"),
  );
  const rawSourceInventory = JSON.parse(
    readFileSync(join(productionDir, "raw-source-inventory.json"), "utf8"),
  );
  if (rawSourceInventory.sources[0]?.artifact_id !== "1001") {
    throw new Error("builder overwrote the raw GitHub artifact provenance inventory");
  }
  if (
    sourceInventory.schema_version !== "ocservia.g6-builder-source-inventory.v1" ||
    !sourceInventory.sources.some(
      (source) => source.path === "peer/isolation/active-load.json",
    ) ||
    !sourceInventory.sources.some(
      (source) => source.path === "contract/g6-slo.yaml",
    ) ||
    !sourceInventory.sources.some(
      (source) => source.path === "run/state/relay-b-observation.json",
    ) ||
    !sourceInventory.sources.some(
      (source) => source.path === "run/state/relay-b-node-id",
    ) ||
    !sourceInventory.sources.some(
      (source) => source.path === "run/state/window-opening-active.json",
    ) ||
    !sourceInventory.sources.some(
      (source) => source.path === "peer/isolation/rto-started-at",
    ) ||
    !sourceInventory.sources.some(
      (source) => source.path === "run/state/relay-b-before-command.json",
    ) ||
    !sourceInventory.sources.some(
      (source) => source.path === "run/state/relay-command-proof.json",
    ) ||
    !sourceInventory.sources.some(
      (source) => source.path === "run/state/relay-dispatch-proof.json",
    ) ||
    !sourceInventory.sources.some(
      (source) => source.path === "run/state/relay-b-active-at",
    ) ||
    !sourceInventory.sources.some(
      (source) => source.path === "run/state/relay-b-started.json",
    ) ||
    !sourceInventory.sources.some(
      (source) =>
        source.path === "run/outbox/relay-pre-fault/relay-a-observation.json",
    ) ||
    !sourceInventory.sources.some(
      (source) =>
        source.path ===
        "run/outbox/relay-pre-fault/relay-a-before-command.json",
    ) ||
    !sourceInventory.sources.some(
      (source) =>
        source.path ===
        "run/outbox/relay-pre-fault/relay-a-command-proof.json",
    ) ||
    !sourceInventory.sources.some(
      (source) =>
        source.path ===
        "run/outbox/relay-pre-fault/relay-a-dispatch-proof.json",
    ) ||
    !sourceInventory.sources.some(
      (source) => source.path === "run/outbox/relay-pre-fault/observed-at",
    ) ||
    !sourceInventory.sources.some(
      (source) =>
        source.path ===
        "run/outbox/relay-pre-fault/relay-a-only-readiness.json",
    ) ||
    !sourceInventory.sources.some(
      (source) =>
        source.path === "run/outbox/relay-pre-fault/relay-b-disabled.json",
    ) ||
    !sourceInventory.sources.some(
      (source) => source.path === "run/state/owner-b-terms.tsv",
    ) ||
    !sourceInventory.sources.some(
      (source) => source.path === "peer/relay-fault-cut.json",
    ) ||
    !sourceInventory.sources.some(
      (source) => source.path === "run/state/owner-replacement-sessions.json",
    ) ||
    !sourceInventory.sources.some(
      (source) => source.path === "run/state/scheduler-replacement-term",
    ) ||
    !sourceInventory.sources.some(
      (source) =>
        source.path === "run/state/scheduler-maintenance-observation.json",
    ) ||
    !sourceInventory.sources.some(
      (source) =>
        source.path === "run/outbox/scheduler-maintenance-history.jsonl",
    ) ||
    sourceInventory.sources.some((source) => source.path.startsWith("/"))
  ) {
    throw new Error(
      "builder source inventory must preserve safe relative input identities",
    );
  }

  const originalRelayBNodeId = readFileSync(relayBNodeIdPath, "utf8");
  const originalRelayBProbe = readFileSync(relayBObservationPath, "utf8");
  const relayPreFaultPath = join(
    runDir,
    "outbox",
    "relay-pre-fault",
    "relay-a-observation.json",
  );
  const relayPreFaultBeforePath = join(
    runDir,
    "outbox",
    "relay-pre-fault",
    "relay-a-before-command.json",
  );
  const relayPreFaultCommandProofPath = join(
    runDir,
    "outbox",
    "relay-pre-fault",
    "relay-a-command-proof.json",
  );
  const relayPreFaultDispatchProofPath = join(
    runDir,
    "outbox",
    "relay-pre-fault",
    "relay-a-dispatch-proof.json",
  );
  const relayTopologyPath = join(
    runDir,
    "outbox",
    "relay-pre-fault",
    "relay-a-only-readiness.json",
  );
  const relayDisabledPath = join(
    runDir,
    "outbox",
    "relay-pre-fault",
    "relay-b-disabled.json",
  );
  const relayFaultCutPath = join(peerDir, "relay-fault-cut.json");
  const relayBeforeCommandPath = join(
    runDir,
    "state",
    "relay-b-before-command.json",
  );
  const relayCommandProofPath = join(
    runDir,
    "state",
    "relay-command-proof.json",
  );
  const relayDispatchProofPath = join(
    runDir,
    "state",
    "relay-dispatch-proof.json",
  );
  const relayActiveAtPath = join(runDir, "state", "relay-b-active-at");
  const relayStartedPath = join(runDir, "state", "relay-b-started.json");
  const relayFailureAtPath = join(peerDir, "relay-a-failed-at");
  const originalRelayPreFault = readFileSync(relayPreFaultPath, "utf8");
  const originalRelayPreFaultBefore = readFileSync(
    relayPreFaultBeforePath,
    "utf8",
  );
  const originalRelayPreFaultCommandProof = readFileSync(
    relayPreFaultCommandProofPath,
    "utf8",
  );
  const originalRelayPreFaultDispatchProof = readFileSync(
    relayPreFaultDispatchProofPath,
    "utf8",
  );
  const originalRelayTopology = readFileSync(relayTopologyPath, "utf8");
  const originalRelayDisabled = readFileSync(relayDisabledPath, "utf8");
  const originalRelayFaultCut = readFileSync(relayFaultCutPath, "utf8");
  const originalRelayBeforeCommand = readFileSync(
    relayBeforeCommandPath,
    "utf8",
  );
  const originalRelayCommandProof = readFileSync(relayCommandProofPath, "utf8");
  const originalRelayDispatchProof = readFileSync(
    relayDispatchProofPath,
    "utf8",
  );
  const originalRelayActiveAt = readFileSync(relayActiveAtPath, "utf8");
  const originalRelayStarted = readFileSync(relayStartedPath, "utf8");
  const originalRelayFailureAt = readFileSync(relayFailureAtPath, "utf8");
  const rtoStartedPath = join(peerDir, "isolation", "rto-started-at");
  const originalRtoStartedAt = readFileSync(rtoStartedPath, "utf8");
  rmSync(rtoStartedPath);
  expectBuilderFailure(
    join(work, "missing-rto-start-bundle"),
    "required input is missing or empty",
  );
  write(rtoStartedPath, `${at(120)}\n`);
  expectBuilderFailure(
    join(work, "reversed-rto-clock-bundle"),
    "database RTO boundaries are not ordered on the promoted database clock",
  );
  write(rtoStartedPath, originalRtoStartedAt);
  rmSync(relayBObservationPath);
  expectBuilderFailure(
    join(work, "missing-relay-probe-bundle"),
    "required input is missing or empty",
  );
  write(relayBObservationPath, "");
  expectBuilderFailure(
    join(work, "empty-relay-probe-bundle"),
    "required input is missing or empty",
  );
  write(relayBObservationPath, originalRelayBProbe);
  rmSync(relayPreFaultPath);
  expectBuilderFailure(
    join(work, "missing-relay-pre-fault-probe-bundle"),
    "required input is missing or empty",
  );
  write(relayPreFaultPath, originalRelayPreFault);
  rmSync(relayTopologyPath);
  expectBuilderFailure(
    join(work, "missing-relay-controlled-topology-bundle"),
    "required input is missing or empty",
  );
  write(relayTopologyPath, originalRelayTopology);
  const substitutedRelayTopology = JSON.parse(originalRelayTopology);
  substitutedRelayTopology.node_id = nodes[1].nodeId;
  write(relayTopologyPath, JSON.stringify(substitutedRelayTopology));
  expectBuilderFailure(
    join(work, "substituted-relay-controlled-topology-bundle"),
    "relay-a proof is not bound to the controlled internal topology",
  );
  write(relayTopologyPath, originalRelayTopology);
  rmSync(relayDisabledPath);
  expectBuilderFailure(
    join(work, "missing-relay-b-disabled-marker-bundle"),
    "required input is missing or empty",
  );
  write(relayDisabledPath, originalRelayDisabled);
  const lateRelayDisabled = JSON.parse(originalRelayDisabled);
  lateRelayDisabled.disabled_at = at(175);
  write(relayDisabledPath, JSON.stringify(lateRelayDisabled));
  expectBuilderFailure(
    join(work, "late-relay-b-disabled-marker-bundle"),
    "relay failover evidence has invalid authoritative database boundaries",
  );
  write(relayDisabledPath, originalRelayDisabled);
  rmSync(relayStartedPath);
  expectBuilderFailure(
    join(work, "missing-relay-b-started-marker-bundle"),
    "required input is missing or empty",
  );
  write(relayStartedPath, originalRelayStarted);
  const substitutedRelayStarted = JSON.parse(originalRelayStarted);
  substitutedRelayStarted.candidate_sha = "f".repeat(40);
  write(relayStartedPath, JSON.stringify(substitutedRelayStarted));
  expectBuilderFailure(
    join(work, "substituted-relay-b-started-marker-bundle"),
    "relay-b started marker is invalid or substituted",
  );
  const earlyRelayStarted = JSON.parse(originalRelayStarted);
  earlyRelayStarted.started_at = at(180);
  write(relayStartedPath, JSON.stringify(earlyRelayStarted));
  expectBuilderFailure(
    join(work, "early-relay-b-started-marker-bundle"),
    "relay failover evidence has invalid authoritative database boundaries",
  );
  write(relayStartedPath, originalRelayStarted);
  rmSync(relayPreFaultCommandProofPath);
  expectBuilderFailure(
    join(work, "missing-relay-pre-fault-command-proof-bundle"),
    "required input is missing or empty",
  );
  write(relayPreFaultCommandProofPath, originalRelayPreFaultCommandProof);
  const wrongPreFaultDispatchPath = JSON.parse(
    originalRelayPreFaultDispatchProof,
  );
  wrongPreFaultDispatchPath.path_detail = "iroh/relay-b";
  write(
    relayPreFaultDispatchProofPath,
    JSON.stringify(wrongPreFaultDispatchPath),
  );
  expectBuilderFailure(
    join(work, "wrong-relay-pre-fault-dispatch-path-bundle"),
    "pre-fault relay dispatch proof is not the exact observed relay-a fenced session",
  );
  write(
    relayPreFaultDispatchProofPath,
    originalRelayPreFaultDispatchProof,
  );
  const expiredRelayFaultCut = JSON.parse(originalRelayFaultCut);
  expiredRelayFaultCut.authority_lease_until = expiredRelayFaultCut.cut_at;
  write(relayFaultCutPath, JSON.stringify(expiredRelayFaultCut));
  expectBuilderFailure(
    join(work, "expired-relay-fault-authority-bundle"),
    "relay fault cut is not bound to one live pre-fault owner session",
  );
  write(relayFaultCutPath, originalRelayFaultCut);
  const expiredPreFaultSession = JSON.parse(originalRelayPreFault);
  expiredPreFaultSession.observations[0].owner_lease_until = at(179);
  write(relayPreFaultPath, JSON.stringify(expiredPreFaultSession));
  expectBuilderFailure(
    join(work, "expired-relay-pre-fault-session-bundle"),
    "relay fault cut is not bound to one live pre-fault owner session",
  );
  write(relayPreFaultPath, originalRelayPreFault);
  write(relayPreFaultBeforePath, originalRelayPreFaultBefore);
  rmSync(relayDispatchProofPath);
  expectBuilderFailure(
    join(work, "missing-relay-dispatch-proof-bundle"),
    "required input is missing or empty",
  );
  write(relayDispatchProofPath, originalRelayDispatchProof);
  const wrongRelayDispatchCommand = JSON.parse(originalRelayDispatchProof);
  wrongRelayDispatchCommand.command_id = fakeUuid(17, 7).replaceAll("-", "");
  write(relayDispatchProofPath, JSON.stringify(wrongRelayDispatchCommand));
  expectBuilderFailure(
    join(work, "wrong-relay-dispatch-command-bundle"),
    "relay dispatch proof is not the exact observed relay-b fenced session",
  );
  const wrongRelayDispatchPath = JSON.parse(originalRelayDispatchProof);
  wrongRelayDispatchPath.path = "direct";
  wrongRelayDispatchPath.path_detail = "iroh/direct";
  write(relayDispatchProofPath, JSON.stringify(wrongRelayDispatchPath));
  expectBuilderFailure(
    join(work, "wrong-relay-dispatch-path-bundle"),
    "relay dispatch proof is not the exact observed relay-b fenced session",
  );
  const wrongRelayDispatchSession = JSON.parse(originalRelayDispatchProof);
  wrongRelayDispatchSession.connection_id = "f".repeat(32);
  write(relayDispatchProofPath, JSON.stringify(wrongRelayDispatchSession));
  expectBuilderFailure(
    join(work, "wrong-relay-dispatch-session-bundle"),
    "relay dispatch proof is not the exact observed relay-b fenced session",
  );
  write(relayDispatchProofPath, originalRelayDispatchProof);
  const wrongRelayCommandProof = JSON.parse(originalRelayCommandProof);
  wrongRelayCommandProof.command_id = fakeUuid(17, 7);
  write(relayCommandProofPath, JSON.stringify(wrongRelayCommandProof));
  expectBuilderFailure(
    join(work, "wrong-relay-command-proof-bundle"),
    "relay failover command proof does not bind one successful durable command",
  );
  write(relayCommandProofPath, originalRelayCommandProof);
  write(relayActiveAtPath, originalRelayFailureAt);
  expectBuilderFailure(
    join(work, "nonpositive-relay-database-clock-bundle"),
    "relay failover evidence has invalid authoritative database boundaries",
  );
  write(relayBeforeCommandPath, originalRelayBeforeCommand);
  write(relayActiveAtPath, originalRelayActiveAt);
  const relayCommandsPath = join(
    runDir,
    "state",
    "evidence",
    "commands.jsonl",
  );
  const originalRelayCommands = readFileSync(relayCommandsPath, "utf8");
  const preCutRelayCommands = originalRelayCommands
    .trimEnd()
    .split("\n")
    .map(JSON.parse);
  preCutRelayCommands.find(
    (command) => command.id === relayCommandId,
  ).created_at = at(179);
  write(relayCommandsPath, jsonl(preCutRelayCommands));
  expectBuilderFailure(
    join(work, "relay-b-command-before-fault-cut-bundle"),
    "relay failover evidence has invalid authoritative database boundaries",
  );
  write(relayCommandsPath, originalRelayCommands);

  const originalOwnerBTerms = readFileSync(ownerBTermsPath, "utf8");
  const originalOwnerReplacementSessions = readFileSync(
    ownerReplacementSessionsPath,
    "utf8",
  );
  rmSync(ownerReplacementSessionsPath);
  expectBuilderFailure(
    join(work, "missing-owner-replacement-sessions-bundle"),
    "required input is missing or empty",
  );
  write(ownerReplacementSessionsPath, originalOwnerReplacementSessions);
  rmSync(ownerBTermsPath);
  expectBuilderFailure(
    join(work, "missing-owner-replacement-terms-bundle"),
    "required input is missing or empty",
  );
  write(ownerBTermsPath, originalOwnerBTerms);
  const incompleteOwnerReplacement = JSON.parse(
    originalOwnerReplacementSessions,
  );
  incompleteOwnerReplacement.observations.pop();
  write(
    ownerReplacementSessionsPath,
    JSON.stringify(incompleteOwnerReplacement),
  );
  expectBuilderFailure(
    join(work, "incomplete-owner-replacement-sessions-bundle"),
    "must cover exactly all managed nodes",
  );
  const wrongOwnerReplacementTerm = JSON.parse(
    originalOwnerReplacementSessions,
  );
  wrongOwnerReplacementTerm.observations[0].connection_id = "f".repeat(32);
  write(
    ownerReplacementSessionsPath,
    JSON.stringify(wrongOwnerReplacementTerm),
  );
  expectBuilderFailure(
    join(work, "wrong-owner-replacement-term-bundle"),
    "not bound to durable authority",
  );
  const earlyOwnerReplacementSession = JSON.parse(
    originalOwnerReplacementSessions,
  );
  earlyOwnerReplacementSession.observations[0].connected_at =
    atMicros(150, 999999);
  write(
    ownerReplacementSessionsPath,
    JSON.stringify(earlyOwnerReplacementSession),
  );
  expectBuilderFailure(
    join(work, "early-owner-replacement-session-bundle"),
    "not bound to durable authority",
  );
  const lateOwnerReplacementSession = JSON.parse(
    originalOwnerReplacementSessions,
  );
  lateOwnerReplacementSession.observations[0].connected_at = at(157);
  write(
    ownerReplacementSessionsPath,
    JSON.stringify(lateOwnerReplacementSession),
  );
  expectBuilderFailure(
    join(work, "late-owner-replacement-session-bundle"),
    "not bound to durable authority",
  );
  write(ownerReplacementSessionsPath, originalOwnerReplacementSessions);
  write(relayBNodeIdPath, `${nodes[1].nodeId}\n`);
  expectBuilderFailure(
    join(work, "substituted-relay-node-bundle"),
    "relay-a proof is not bound to the controlled internal topology",
  );
  write(relayBNodeIdPath, originalRelayBNodeId);
  const wrongRelayNodeProbe = JSON.parse(originalRelayBProbe);
  wrongRelayNodeProbe.observations[0] = {
    ...reconnectSessionInventory.observations[1],
    matched: true,
    path: "relay",
    path_detail: "iroh/relay-b",
  };
  write(relayBObservationPath, JSON.stringify(wrongRelayNodeProbe));
  expectBuilderFailure(
    join(work, "wrong-relay-node-bundle"),
    "relay-b observations do not retain one exact session around the command",
  );
  const wrongRelayPathProbe = JSON.parse(originalRelayBProbe);
  wrongRelayPathProbe.observations[0].path = "direct";
  wrongRelayPathProbe.observations[0].path_detail = "iroh/direct";
  write(relayBObservationPath, JSON.stringify(wrongRelayPathProbe));
  expectBuilderFailure(
    join(work, "wrong-relay-path-bundle"),
    "post-command relay observation is not one complete matched relay-b probe",
  );
  write(relayBObservationPath, originalRelayBProbe);
  if (
    structuredArtifactKinds.length !== 13 ||
    builtEvidence.artifacts.filter((artifact) =>
      structuredArtifactKinds.includes(artifact.kind),
    ).length !== 13
  ) {
    throw new Error("builder must emit exactly thirteen structured artifacts");
  }
  const builtAuthorityOwnerByNode = new Map(
    builtAuthorityCut.owners.map((owner) => [owner.node, owner]),
  );
  for (const owner of sourceAuthorityCut.owners) {
    const built = builtAuthorityOwnerByNode.get(owner.node_hex);
    if (
      built?.instance !== owner.owner_instance_id ||
      built?.incarnation !== owner.owner_incarnation ||
      built?.connection_id !== owner.connection_id ||
      built?.epoch !== owner.owner_epoch ||
      built?.lease_until !== atMicros(350, 0)
    ) {
      throw new Error(`builder did not preserve owner tuple ${owner.node_hex}`);
    }
  }
  if (
    builtAuthorityCut.cut_at !== sourceAuthorityCut.cut_at ||
    builtAuthorityCut.scheduler.instance !==
      sourceAuthorityCut.leader.instance_id ||
    builtAuthorityCut.scheduler.incarnation !==
      sourceAuthorityCut.leader.incarnation ||
    builtAuthorityCut.scheduler.epoch !== sourceAuthorityCut.leader.epoch ||
    builtAuthorityCut.scheduler.lease_until !== atMicros(350, 0)
  ) {
    throw new Error("builder did not preserve the scheduler authority tuple");
  }
  const expectedLeaseByNode = new Map(
    sourceAuthorityCut.owners.map((owner) => [
      owner.node_hex,
      atMicros(350, 0),
    ]),
  );
  for (const session of builtSessionInventory.sessions) {
    const expectedSession = expectedSessionByNode.get(session.node);
    const expectedReconnect = expectedReconnectByNode.get(session.node);
    const reconnectTerm = reconnectOwnerTerms.get(
      session.node.replaceAll("-", ""),
    );
    if (
      session.owner_instance !== expectedSession?.owner_instance_id ||
      session.owner_incarnation !== expectedSession?.owner_incarnation ||
      session.endpoint_id !== expectedSession?.endpoint_id ||
      session.agent_instance_id !==
        expectedSession?.agent_instance_id.replaceAll("-", "") ||
      session.connection_id !== expectedSession?.connection_id ||
      session.owner_epoch !== expectedSession?.owner_epoch ||
      session.connected_at !== expectedSession?.connected_at ||
      session.session_expires_at !== expectedSession?.session_expires_at ||
      session.reconnected_at !== reconnectTerm?.registeredAt ||
      session.reconnect_owner_instance !==
        expectedReconnect?.owner_instance_id ||
      session.reconnect_owner_incarnation !==
        expectedReconnect?.owner_incarnation ||
      session.reconnect_connection_id !== expectedReconnect?.connection_id ||
      session.reconnect_owner_epoch !== expectedReconnect?.owner_epoch ||
      session.owner_lease_until !==
        expectedLeaseByNode.get(session.node.replaceAll("-", ""))
    ) {
      throw new Error(
        `builder did not preserve final owner authority for ${session.node}`,
      );
    }
  }
  if (
    builtSessionInventory.snapshot_taken_at !== sourceAuthorityCut.cut_at ||
    builtSessionInventory.scheduler_authority.instance !==
      sourceAuthorityCut.leader.instance_id ||
    builtSessionInventory.scheduler_authority.incarnation !==
      sourceAuthorityCut.leader.incarnation ||
    builtSessionInventory.scheduler_authority.epoch !==
      sourceAuthorityCut.leader.epoch ||
    builtSessionInventory.scheduler_authority.lease_until !== atMicros(350, 0)
  ) {
    throw new Error("builder did not preserve the final scheduler authority");
  }
  const appendEpochExpiry = (content, record) => {
    const records = content.trimEnd().split("\n").map(JSON.parse);
    records.push({
      sequence: records.at(-1).sequence + 1,
      timestamp: builtSessionInventory.snapshot_taken_at,
      environment_id: environmentId,
      candidate_sha: candidateSha,
      ...record,
    });
    return jsonl(records);
  };
  expectTamperedBundleFailure(
    productionDir,
    "stale-session-owner-epoch-artifact",
    "agent-sessions.json",
    (content) => {
      const inventory = JSON.parse(content);
      inventory.sessions[0].owner_epoch = 2;
      return `${JSON.stringify(inventory, null, 2)}\n`;
    },
    `owner_epoch 2 does not match latest connection-owner epoch ${node1LaterTerm.epoch}`,
  );
  expectTamperedBundleFailure(
    productionDir,
    "invalid-session-owner-epoch-artifact",
    "agent-sessions.json",
    (content) => {
      const inventory = JSON.parse(content);
      inventory.sessions[0].owner_epoch = 0;
      return `${JSON.stringify(inventory, null, 2)}\n`;
    },
    "owner_epoch must be positive",
  );
  expectTamperedBundleFailure(
    productionDir,
    "mismatched-authority-owner-tuple",
    "authority-cut.json",
    (content) => {
      const cut = JSON.parse(content);
      cut.owners[0].instance = "worker-tampered";
      return `${JSON.stringify(cut, null, 2)}\n`;
    },
    "transport owner term does not match the database cut",
  );
  expectTamperedBundleFailure(
    productionDir,
    "mismatched-authority-owner-lease",
    "authority-cut.json",
    (content) => {
      const cut = JSON.parse(content);
      cut.owners[0].lease_until = at(349);
      return `${JSON.stringify(cut, null, 2)}\n`;
    },
    "authority cut owner tuple does not match latest epoch",
  );
  expectTamperedBundleFailure(
    productionDir,
    "mismatched-authority-owner-incarnation",
    "authority-cut.json",
    (content) => {
      const cut = JSON.parse(content);
      cut.owners[0].incarnation = (
        BigInt(cut.owners[0].incarnation) + 1n
      ).toString();
      return `${JSON.stringify(cut, null, 2)}\n`;
    },
    "transport owner term does not match the database cut",
  );
  expectTamperedBundleFailure(
    productionDir,
    "mismatched-authority-owner-connection",
    "authority-cut.json",
    (content) => {
      const cut = JSON.parse(content);
      cut.owners[0].connection_id = "f".repeat(32);
      return `${JSON.stringify(cut, null, 2)}\n`;
    },
    "transport owner term does not match the database cut",
  );
  expectTamperedBundleFailure(
    productionDir,
    "mismatched-authority-scheduler-tuple",
    "authority-cut.json",
    (content) => {
      const cut = JSON.parse(content);
      cut.scheduler.instance = "scheduler-tampered";
      return `${JSON.stringify(cut, null, 2)}\n`;
    },
    "authority cut scheduler tuple does not match latest epoch",
  );
  expectTamperedBundleFailure(
    productionDir,
    "mismatched-authority-scheduler-lease",
    "authority-cut.json",
    (content) => {
      const cut = JSON.parse(content);
      cut.scheduler.lease_until = at(349);
      return `${JSON.stringify(cut, null, 2)}\n`;
    },
    "authority cut scheduler tuple does not match latest epoch",
  );
  expectTamperedBundleFailure(
    productionDir,
    "mismatched-authority-scheduler-incarnation",
    "authority-cut.json",
    (content) => {
      const cut = JSON.parse(content);
      cut.scheduler.incarnation = (
        BigInt(cut.scheduler.incarnation) + 1n
      ).toString();
      return `${JSON.stringify(cut, null, 2)}\n`;
    },
    "authority cut scheduler tuple does not match latest epoch",
  );
  expectTamperedBundleFailure(
    productionDir,
    "collapsed-before-transport-boundary",
    "authority-cut.json",
    (content) => {
      const cut = JSON.parse(content);
      cut.transport_bracket.before_complete_at = cut.cut_at;
      return `${JSON.stringify(cut, null, 2)}\n`;
    },
    "transport inventories must strictly bracket cut_at",
  );
  expectTamperedBundleFailure(
    productionDir,
    "collapsed-after-transport-boundary",
    "authority-cut.json",
    (content) => {
      const cut = JSON.parse(content);
      cut.transport_bracket.after_start_at = cut.cut_at;
      return `${JSON.stringify(cut, null, 2)}\n`;
    },
    "transport inventories must strictly bracket cut_at",
  );
  expectTamperedBundleFailure(
    productionDir,
    "transport-tuple-changes-across-cut",
    "authority-cut.json",
    (content) => {
      const cut = JSON.parse(content);
      cut.transport_bracket.before[0].endpoint_id = "f".repeat(64);
      return `${JSON.stringify(cut, null, 2)}\n`;
    },
    "immutable transport tuple changes across cut_at",
  );
  expectTamperedBundleFailure(
    productionDir,
    "transport-owner-lease-does-not-cross-cut",
    "authority-cut.json",
    (content) => {
      const cut = JSON.parse(content);
      cut.transport_bracket.before[0].owner_lease_until = cut.cut_at;
      return `${JSON.stringify(cut, null, 2)}\n`;
    },
    "owner lease does not remain live across the cut",
  );
  expectTamperedBundleFailure(
    productionDir,
    "transport-session-does-not-cross-cut",
    "authority-cut.json",
    (content) => {
      const cut = JSON.parse(content);
      cut.transport_bracket.after[0].session_expires_at = cut.cut_at;
      return `${JSON.stringify(cut, null, 2)}\n`;
    },
    "session does not remain live across the cut",
  );
  expectTamperedBundleFailure(
    productionDir,
    "transport-term-mismatches-database-cut",
    "authority-cut.json",
    (content) => {
      const cut = JSON.parse(content);
      for (const side of ["before", "after"]) {
        cut.transport_bracket[side][0].connection_id = "f".repeat(32);
      }
      return `${JSON.stringify(cut, null, 2)}\n`;
    },
    "transport owner term does not match the database cut",
  );
  expectTamperedBundleFailure(
    productionDir,
    "transport-physical-session-mismatches-inventory",
    "authority-cut.json",
    (content) => {
      const cut = JSON.parse(content);
      for (const side of ["before", "after"]) {
        cut.transport_bracket[side][0].endpoint_id = "f".repeat(64);
      }
      return `${JSON.stringify(cut, null, 2)}\n`;
    },
    "transport tuple does not match agent session node",
  );
  expectTamperedBundleFailure(
    productionDir,
    "expired-authority-owner-lease",
    "authority-cut.json",
    (content) => {
      const cut = JSON.parse(content);
      cut.owners[0].lease_until = cut.cut_at;
      return `${JSON.stringify(cut, null, 2)}\n`;
    },
    "owner 1 lease must remain live after the cut",
  );
  expectTamperedBundleFailure(
    productionDir,
    "expired-authority-scheduler-lease",
    "authority-cut.json",
    (content) => {
      const cut = JSON.parse(content);
      cut.scheduler.lease_until = cut.cut_at;
      return `${JSON.stringify(cut, null, 2)}\n`;
    },
    "scheduler lease must remain live after the cut",
  );
  expectTamperedBundleFailure(
    productionDir,
    "inactive-session-owner-epoch-bundle",
    "epoch-events.jsonl",
    (content) =>
      appendEpochExpiry(content, {
        subject: "connection_owner",
        event_type: "owner_lease_expired",
        node: builtSessionInventory.sessions[0].node,
        epoch: builtSessionInventory.sessions[0].owner_epoch,
      }),
    `owner_epoch ${node1LaterTerm.epoch} is not active for node ${node1Hex}`,
  );
  expectTamperedBundleFailure(
    productionDir,
    "inactive-scheduler-epoch-bundle",
    "epoch-events.jsonl",
    (content) =>
      appendEpochExpiry(content, {
        subject: "scheduler",
        event_type: "leader_lease_expired",
        epoch: builtSessionInventory.scheduler_authority.epoch,
      }),
    "scheduler epoch 2 is not active",
  );
  expectTamperedBundleFailure(
    productionDir,
    "expired-session-owner-lease-bundle",
    "agent-sessions.json",
    (content) => {
      const inventory = JSON.parse(content);
      inventory.sessions[0].owner_lease_until = inventory.snapshot_taken_at;
      return `${JSON.stringify(inventory, null, 2)}\n`;
    },
    "owner lease must remain live after the snapshot",
  );
  expectTamperedBundleFailure(
    productionDir,
    "expired-scheduler-lease-bundle",
    "agent-sessions.json",
    (content) => {
      const inventory = JSON.parse(content);
      inventory.scheduler_authority.lease_until = inventory.snapshot_taken_at;
      return `${JSON.stringify(inventory, null, 2)}\n`;
    },
    "scheduler lease must remain live after the snapshot",
  );
  expectTamperedBundleFailure(
    productionDir,
    "duplicate-physical-session-node-bundle",
    "agent-sessions.json",
    (content) => {
      const inventory = JSON.parse(content);
      inventory.sessions[1].node = inventory.sessions[0].node;
      return `${JSON.stringify(inventory, null, 2)}\n`;
    },
    "repeats node",
  );
  expectTamperedBundleFailure(
    productionDir,
    "invalid-physical-session-endpoint-bundle",
    "agent-sessions.json",
    (content) => {
      const inventory = JSON.parse(content);
      inventory.sessions[0].endpoint_id = "f".repeat(32);
      return `${JSON.stringify(inventory, null, 2)}\n`;
    },
    "must be a 64-character lowercase hexadecimal endpoint id",
  );
  expectTamperedBundleFailure(
    productionDir,
    "expired-physical-session-bundle",
    "agent-sessions.json",
    (content) => {
      const inventory = JSON.parse(content);
      inventory.sessions[0].session_expires_at = inventory.snapshot_taken_at;
      return `${JSON.stringify(inventory, null, 2)}\n`;
    },
    "session must remain live after the snapshot",
  );
  expectTamperedBundleFailure(
    productionDir,
    "post-snapshot-physical-session-bundle",
    "agent-sessions.json",
    (content) => {
      const inventory = JSON.parse(content);
      inventory.sessions[0].connected_at = at(321);
      inventory.sessions[0].reconnected_at = at(321);
      return `${JSON.stringify(inventory, null, 2)}\n`;
    },
    "must connect before the snapshot",
  );
  expectTamperedBundleFailure(
    productionDir,
    "reconnect-transport-predates-registration",
    "agent-sessions.json",
    (content) => {
      const inventory = JSON.parse(content);
      inventory.sessions[0].reconnected_at = atMicros(160, 100000);
      return `${JSON.stringify(inventory, null, 2)}\n`;
    },
    "transport reconnect predates the durable owner registration",
  );
  expectTamperedBundleFailure(
    productionDir,
    "reconnect-registration-tuple-mismatch",
    "agent-sessions.json",
    (content) => {
      const inventory = JSON.parse(content);
      inventory.sessions[1].reconnect_connection_id = "f".repeat(32);
      return `${JSON.stringify(inventory, null, 2)}\n`;
    },
    "reconnect tuple has no durable owner registration",
  );

  const engineeringDir = join(work, "engineering-bundle");
  runBuilder(engineeringDir, "engineering");
  const rehearsal = verifyBundle(engineeringDir, "engineering");
  if (rehearsal.passed) {
    throw new Error("engineering bundle must stay non-final");
  }
  if (
    !rehearsal.failure_reasons.includes(
      "final pass requires production_readiness authority",
    )
  ) {
    throw new Error(
      `engineering bundle must fail only on the authority fence: ${rehearsal.failure_reasons.join("; ")}`,
    );
  }
  const failedRehearsal = Object.entries(rehearsal.measurement_results).filter(
    ([, result]) => !result.passed,
  );
  if (failedRehearsal.length > 0) {
    throw new Error(
      `engineering bundle metrics failed beyond the fence: ${failedRehearsal.map(([name]) => name).join(", ")}`,
    );
  }

  const originalReconnectSessions = readFileSync(reconnectSessionsPath, "utf8");
  rmSync(reconnectSessionsPath);
  expectBuilderFailure(
    join(work, "missing-reconnect-snapshot-bundle"),
    "required input is missing or empty",
  );
  write(reconnectSessionsPath, originalReconnectSessions);

  const incompleteReconnectSessions = JSON.parse(originalReconnectSessions);
  incompleteReconnectSessions.observations.pop();
  write(reconnectSessionsPath, JSON.stringify(incompleteReconnectSessions));
  expectBuilderFailure(
    join(work, "incomplete-reconnect-snapshot-bundle"),
    "must cover exactly all managed nodes",
  );
  write(reconnectSessionsPath, originalReconnectSessions);

  const mismatchedReconnectSessions = JSON.parse(originalReconnectSessions);
  mismatchedReconnectSessions.observations[1].connection_id = "f".repeat(32);
  write(reconnectSessionsPath, JSON.stringify(mismatchedReconnectSessions));
  expectBuilderFailure(
    join(work, "mismatched-reconnect-tuple-bundle"),
    "has no durable owner registration",
  );
  write(reconnectSessionsPath, originalReconnectSessions);

  const preStormReconnectSessions = JSON.parse(originalReconnectSessions);
  preStormReconnectSessions.observations[0].connected_at = at(159);
  write(reconnectSessionsPath, JSON.stringify(preStormReconnectSessions));
  expectBuilderFailure(
    join(work, "pre-storm-reconnect-snapshot-bundle"),
    "connects before the storm",
  );
  write(reconnectSessionsPath, originalReconnectSessions);

  const fractionalReconnectSessions = JSON.parse(originalReconnectSessions);
  fractionalReconnectSessions.observations[0].connected_at = atMicros(160, 200001);
  write(reconnectSessionsPath, JSON.stringify(fractionalReconnectSessions));
  const fractionalReconnectDir = join(work, "fractional-reconnect-snapshot-bundle");
  runBuilder(fractionalReconnectDir, "production_readiness");
  if (!verifyBundle(fractionalReconnectDir, "production_readiness").passed) {
    throw new Error("a transport reconnect strictly after registration was rejected");
  }
  write(reconnectSessionsPath, originalReconnectSessions);

  const originalTimeline = readFileSync(timelinePath, "utf8");
  const timelineWithReconnectCompletion = (timestamp, compact = false) =>
    `${originalTimeline
      .trimEnd()
      .split("\n")
      .map((line) => {
        const event = JSON.parse(line);
        if (compact && event.event_id === "stale_agent_rejected") {
          event.timestamp = atMicros(160, 150000);
        }
        if (event.event_id === "reconnect_completed") event.timestamp = timestamp;
        return JSON.stringify(event);
      })
      .join("\n")}\n`;
  write(timelinePath, timelineWithReconnectCompletion(atMicros(164, 300000)));
  const preciseCompletionDir = join(work, "precise-reconnect-completion-bundle");
  runBuilder(preciseCompletionDir, "production_readiness");
  if (!verifyBundle(preciseCompletionDir, "production_readiness").passed) {
    throw new Error("a precise completion after a same-second registration was rejected");
  }
  write(
    timelinePath,
    timelineWithReconnectCompletion(atMicros(160, 240000), true),
  );
  expectBuilderFailure(
    join(work, "early-reconnect-completion-bundle"),
    "connects after reconnect completion",
  );
  write(timelinePath, originalTimeline);

  const lateReconnectSessions = JSON.parse(originalReconnectSessions);
  lateReconnectSessions.observations[0].connected_at = at(166);
  write(reconnectSessionsPath, JSON.stringify(lateReconnectSessions));
  expectBuilderFailure(
    join(work, "late-reconnect-snapshot-bundle"),
    "connects after reconnect completion",
  );
  write(reconnectSessionsPath, originalReconnectSessions);

  const staleFinalSessions = structuredClone(finalSessionInventory);
  staleFinalSessions.observations[0].owner_epoch = 2;
  write(afterFinalSessionsPath, JSON.stringify(staleFinalSessions));
  expectBuilderFailure(
    join(work, "stale-session-owner-epoch-bundle"),
    "final session owner epoch disagrees with authority",
  );
  write(afterFinalSessionsPath, JSON.stringify(finalSessionInventory));

  const effectDir = join(runDir, "state", "evidence", "effects");
  const effectPath = join(effectDir, historicalEffectFile);
  const originalEffects = readFileSync(effectPath, "utf8");
  const historicalHex = historicalCommandId.replaceAll("-", "");
  const withoutHistorical = originalEffects
    .trimEnd()
    .split("\n")
    .filter((line) => !line.includes(historicalHex));
  if (
    withoutHistorical.length === originalEffects.trimEnd().split("\n").length
  ) {
    throw new Error("historical effect fixture is missing");
  }
  write(effectPath, `${withoutHistorical.join("\n")}\n`);
  expectBuilderFailure(
    join(work, "missing-historical-effect-bundle"),
    "has no durable effect",
  );
  write(effectPath, originalEffects);

  const windowHex = commands[0].id.replaceAll("-", "");
  let windowEffectPath;
  let windowEffectText;
  for (const entry of readdirSync(effectDir).sort()) {
    const path = join(effectDir, entry);
    const content = readFileSync(path, "utf8");
    if (content.includes(windowHex)) {
      windowEffectPath = path;
      windowEffectText = content;
      break;
    }
  }
  if (!windowEffectPath || !windowEffectText) {
    throw new Error("window effect fixture is missing");
  }
  write(
    windowEffectPath,
    `${windowEffectText
      .trimEnd()
      .split("\n")
      .filter((line) => !line.includes(windowHex))
      .join("\n")}\n`,
  );
  expectBuilderFailure(
    join(work, "missing-window-effect-bundle"),
    "has no durable effect",
  );
  write(windowEffectPath, windowEffectText);

  const duplicateTargetPath = join(effectDir, "agent-fd-b-25.tsv");
  const duplicateTargetEffects = readFileSync(duplicateTargetPath, "utf8");
  const duplicateLine = windowEffectText
    .trimEnd()
    .split("\n")
    .find((line) => line.includes(windowHex));
  if (!duplicateLine) {
    throw new Error("duplicate-effect source fixture is missing");
  }
  write(duplicateTargetPath, `${duplicateTargetEffects}${duplicateLine}\n`);
  expectBuilderFailure(
    join(work, "duplicate-run-effect-bundle"),
    "two agents hold an effect",
  );
  write(duplicateTargetPath, duplicateTargetEffects);

  const missingAgentPath = join(effectDir, "agent-fd-a-25.tsv");
  const missingAgentEffects = readFileSync(missingAgentPath, "utf8");
  rmSync(missingAgentPath);
  expectBuilderFailure(
    join(work, "missing-agent-freeze-bundle"),
    "does not cover exactly all managed Agents",
  );
  write(missingAgentPath, missingAgentEffects);

  const commandsPath = join(runDir, "state", "evidence", "commands.jsonl");
  const originalCommands = readFileSync(commandsPath, "utf8");
  const sameSecondCommands = originalCommands
    .trimEnd()
    .split("\n")
    .map((line) => JSON.parse(line));
  const sameSecondCommand = sameSecondCommands[0];
  sameSecondCommand.created_at = atMicros(5, 500000);
  const sameSecondHex = sameSecondCommand.id.replaceAll("-", "");
  let sameSecondEffectPath;
  let sameSecondOriginalEffects;
  let sameSecondEffectKey;
  for (const entry of readdirSync(effectDir).sort()) {
    const path = join(effectDir, entry);
    const content = readFileSync(path, "utf8");
    if (!content.includes(sameSecondHex)) continue;
    sameSecondEffectPath = path;
    sameSecondOriginalEffects = content;
    const lines = content.trimEnd().split("\n");
    const lineIndex = lines.findIndex((line) => line.includes(sameSecondHex));
    const fields = lines[lineIndex].split(/\s+/);
    sameSecondEffectKey = fields[0].toLowerCase();
    fields[2] = String(epochSeconds(5));
    lines[lineIndex] = fields.join(" ");
    write(path, `${lines.join("\n")}\n`);
    break;
  }
  if (!sameSecondEffectPath || !sameSecondOriginalEffects || !sameSecondEffectKey) {
    throw new Error("same-second effect fixture is missing");
  }
  write(commandsPath, jsonl(sameSecondCommands));
  const sameSecondEffectDir = join(work, "same-second-effect-bundle");
  runBuilder(sameSecondEffectDir, "production_readiness");
  if (!verifyBundle(sameSecondEffectDir, "production_readiness").passed) {
    throw new Error("a coarse same-second Agent effect was rejected");
  }
  const sameSecondTrace = readFileSync(
    join(sameSecondEffectDir, "command-trace.jsonl"),
    "utf8",
  )
    .trimEnd()
    .split("\n")
    .map(JSON.parse);
  const observedEffect = sameSecondTrace.find(
    (record) =>
      record.record_type === "effect" &&
      record.idempotency_key === `g6-journal-key-${sameSecondEffectKey}`,
  );
  if (observedEffect?.timestamp !== atMicros(15, 0)) {
    throw new Error(
      "a coarse Agent effect was not conservatively stamped at terminal observation",
    );
  }
  write(commandsPath, originalCommands);
  write(sameSecondEffectPath, sameSecondOriginalEffects);

  const preciseSlowCommands = originalCommands
    .trimEnd()
    .split("\n")
    .map((line) => JSON.parse(line));
  for (const command of preciseSlowCommands.slice(0, 5)) {
    command.updated_at = atMicros(35, 100000);
  }
  write(commandsPath, jsonl(preciseSlowCommands));
  const preciseSlowDir = join(work, "precise-command-completion-bundle");
  runBuilder(preciseSlowDir, "production_readiness");
  const preciseSlowTrace = readFileSync(
    join(preciseSlowDir, "command-trace.jsonl"),
    "utf8",
  );
  if (!preciseSlowTrace.includes(`${atMicros(35, 100000)}`)) {
    throw new Error("command trace discarded the captured sub-second completion");
  }
  const preciseSlowVerdict = verifyBundle(
    preciseSlowDir,
    "production_readiness",
  );
  const preciseP99 =
    preciseSlowVerdict.measurement_results.synthetic_command_completion_seconds_p99;
  if (preciseSlowVerdict.passed || preciseP99.passed || preciseP99.actual <= 30) {
    throw new Error(
      `a true just-over-30s command P99 was not rejected: ${preciseP99.actual}`,
    );
  }
  write(commandsPath, originalCommands);

  const peerSnapshotPath = join(peerDir, "evidence", "snapshot-taken-at");
  const originalPeerSnapshot = readFileSync(peerSnapshotPath, "utf8");
  write(peerSnapshotPath, `${atMicros(323, 1)}\n`);
  const preciseWindowDir = join(work, "precise-evidence-window-bundle");
  runBuilder(preciseWindowDir, "production_readiness");
  const preciseWindowEvidence = JSON.parse(
    readFileSync(join(preciseWindowDir, "evidence.json"), "utf8"),
  );
  if (preciseWindowEvidence.finished_at !== atNanos(323, 1000)) {
    throw new Error(
      `evidence window discarded its precise maximum: ${preciseWindowEvidence.finished_at}`,
    );
  }
  if (!verifyBundle(preciseWindowDir, "production_readiness").passed) {
    throw new Error("a precise evidence-window boundary was rejected");
  }
  write(peerSnapshotPath, originalPeerSnapshot);

  const nonterminalCommands = originalCommands
    .trimEnd()
    .split("\n")
    .map((line) => JSON.parse(line));
  const nonterminalCommand = nonterminalCommands.find(
    (command) => command.id === rejectedCommandId,
  );
  if (!nonterminalCommand) {
    throw new Error("rejected command source fixture is missing");
  }
  nonterminalCommand.state = "running";
  write(commandsPath, jsonl(nonterminalCommands));
  expectBuilderFailure(
    join(work, "nonterminal-command-bundle"),
    "command state running has no trace outcome mapping",
    {
      path: `run/state/evidence/commands.jsonl#command_id=${rejectedCommandId}/state`,
      expected: "succeeded, failed, rejected, or unknown",
      actual: "running",
    },
  );
  write(commandsPath, originalCommands);

  const localInstancesPath = join(
    runDir,
    "state",
    "evidence",
    "instances.tsv",
  );
  const originalLocalInstances = readFileSync(localInstancesPath, "utf8");
  const mismatchedLocalInstances = originalLocalInstances
    .trimEnd()
    .split("\n")
    .map((line) => {
      const fields = line.split("\t");
      if (fields[4] === "agent-fd-b-01") fields[1] = digestOf("f");
      return fields.join("\t");
    })
    .join("\n");
  write(localInstancesPath, `${mismatchedLocalInstances}\n`);
  expectBuilderFailure(
    join(work, "cross-domain-agent-digest-bundle"),
    "component agent has different digests across failure domains",
    {
      path: "topology.instances#instance_id=agent-fd-b-01/component_digest",
      expected: digestOf("e"),
      actual: digestOf("f"),
      expected_instance_id: "agent-fd-a-01",
    },
  );
  write(localInstancesPath, originalLocalInstances);

  const freezePath = join(peerDir, "final-freeze-at");
  const originalFreeze = readFileSync(freezePath, "utf8");
  write(freezePath, `${at(309)}\n`);
  expectBuilderFailure(
    join(work, "premature-final-freeze-bundle"),
    "was not frozen after the bounded window",
  );
  write(freezePath, originalFreeze);

  const activeLoadPath = join(peerDir, "isolation", "active-load.json");
  const originalActiveLoad = readFileSync(activeLoadPath, "utf8");
  const lateActiveLoad = JSON.parse(originalActiveLoad);
  lateActiveLoad.captured_at = atMicros(50, 301000);
  write(activeLoadPath, JSON.stringify(lateActiveLoad));
  expectBuilderFailure(
    join(work, "late-active-load-bundle"),
    "active-load snapshot was captured after the database outage",
    {
      path: "peer/isolation/active-load.json#/captured_at",
      expected: `<= ${atMicros(50, 300000)}`,
      actual: atMicros(50, 301000),
    },
  );
  write(activeLoadPath, originalActiveLoad);

  const originalSchedulerMaintenanceObservation = readFileSync(
    schedulerMaintenanceObservationPath,
    "utf8",
  );
  const markerAfterObservation = {
    ...schedulerMaintenanceObservation,
    committed_observed_at: atMicros(43, 499999),
  };
  write(
    schedulerMaintenanceObservationPath,
    JSON.stringify(markerAfterObservation),
  );
  expectBuilderFailure(
    join(work, "scheduler-marker-after-observation-bundle"),
    "scheduler maintenance marker completed after it was observed committed",
  );
  write(
    schedulerMaintenanceObservationPath,
    originalSchedulerMaintenanceObservation,
  );

  write(
    schedulerMaintenanceObservationPath,
    JSON.stringify({
      ...schedulerMaintenanceObservation,
      maintenance_id: "2999",
    }),
  );
  expectBuilderFailure(
    join(work, "scheduler-observation-id-mismatch-bundle"),
    "scheduler maintenance observation does not match a durable replacement marker",
  );
  write(
    schedulerMaintenanceObservationPath,
    originalSchedulerMaintenanceObservation,
  );

  write(
    schedulerMaintenanceObservationPath,
    JSON.stringify({
      ...schedulerMaintenanceObservation,
      marker_completed_at: atMicros(43, 400000),
    }),
  );
  expectBuilderFailure(
    join(work, "scheduler-observation-marker-mismatch-bundle"),
    "scheduler maintenance observation does not match a durable replacement marker",
  );
  write(
    schedulerMaintenanceObservationPath,
    originalSchedulerMaintenanceObservation,
  );

  write(
    schedulerMaintenanceObservationPath,
    JSON.stringify({
      ...schedulerMaintenanceObservation,
      committed_observed_at: atMicros(320, 200001),
    }),
  );
  expectBuilderFailure(
    join(work, "post-cut-scheduler-observation-bundle"),
    "scheduler maintenance completion was first observed after the final authority cut",
  );
  write(
    schedulerMaintenanceObservationPath,
    originalSchedulerMaintenanceObservation,
  );

  const originalSchedulerReplacementTerm = readFileSync(
    schedulerReplacementTermPath,
    "utf8",
  );
  write(
    schedulerReplacementTermPath,
    `sched-b:${schedulerBIncarnation}:3\n`,
  );
  expectBuilderFailure(
    join(work, "scheduler-observation-term-mismatch-bundle"),
    "scheduler maintenance observation does not match the replacement exact term",
  );
  write(schedulerReplacementTermPath, originalSchedulerReplacementTerm);

  const originalAuthorityCut = readFileSync(finalAuthorityCutPath, "utf8");
  const originalFencingHistory = readFileSync(fencingHistoryPath, "utf8");
  const originalLeadershipHistory = readFileSync(leadershipHistoryPath, "utf8");
  const originalSchedulerMaintenanceHistory = readFileSync(
    schedulerMaintenanceHistoryPath,
    "utf8",
  );
  const writePublishedAndFrozenHistory = (path, field, lines) => {
    write(path, `${lines.join("\n")}\n`);
    const cut = JSON.parse(originalAuthorityCut);
    cut[field] = lines;
    write(finalAuthorityCutPath, JSON.stringify(cut));
  };
  const restoreFrozenHistories = () => {
    write(fencingHistoryPath, originalFencingHistory);
    write(leadershipHistoryPath, originalLeadershipHistory);
    write(
      schedulerMaintenanceHistoryPath,
      originalSchedulerMaintenanceHistory,
    );
    write(finalAuthorityCutPath, originalAuthorityCut);
  };

  const mismatchedPublishedOwnerHistory = completeOwnerHistory.filter(
    (line) => line !== node1ShortAcquireHistory,
  );
  write(fencingHistoryPath, `${mismatchedPublishedOwnerHistory.join("\n")}\n`);
  expectBuilderFailure(
    join(work, "mismatched-published-owner-history-bundle"),
    "published fencing history does not match the frozen final authority cut",
  );
  restoreFrozenHistories();

  const missingShortAcquire = completeOwnerHistory.filter(
    (line) => line !== node1ShortAcquireHistory,
  );
  if (missingShortAcquire.length === completeOwnerHistory.length) {
    throw new Error("short-lived owner acquisition fixture is missing");
  }
  writePublishedAndFrozenHistory(
    fencingHistoryPath,
    "owner_history",
    missingShortAcquire,
  );
  expectBuilderFailure(
    join(work, "missing-short-owner-acquisition-bundle"),
    "expired epoch 3 without a recorded acquisition",
  );
  restoreFrozenHistories();

  const crossInstanceFinalOwnerHistory = node1FinalOwnerHistory.replace(
    `:worker-b:${workerBIncarnation}:`,
    `:worker-a:${workerAIncarnation}:`,
  );
  const crossInstanceEarlyTakeover = completeOwnerHistory
    .filter((line) => line !== node1ShortReleaseHistory)
    .map((line) =>
      line === node1FinalOwnerHistory ? crossInstanceFinalOwnerHistory : line,
    );
  if (
    crossInstanceFinalOwnerHistory === node1FinalOwnerHistory ||
    crossInstanceEarlyTakeover.includes(node1ShortReleaseHistory)
  ) {
    throw new Error("cross-instance early takeover fixture is invalid");
  }
  writePublishedAndFrozenHistory(
    fencingHistoryPath,
    "owner_history",
    crossInstanceEarlyTakeover,
  );
  expectBuilderFailure(
    join(work, "cross-instance-early-owner-takeover-bundle"),
    "replaces live owner epoch 3 across instances",
  );
  restoreFrozenHistories();

  const bulkBoundaryOwnerHistory = completeOwnerHistory.map((line) => {
    if (line === stormReleaseHistory[0]) {
      return line.replaceAll(
        atMicros(159, 100000),
        atMicros(159, 0),
      );
    }
    if (line === node1ReconnectTerm.history) {
      return line.replace(node1ReconnectTerm.registeredAt, atMicros(159, 0));
    }
    return line;
  });
  writePublishedAndFrozenHistory(
    fencingHistoryPath,
    "owner_history",
    bulkBoundaryOwnerHistory,
  );
  expectBuilderFailure(
    join(work, "bulk-boundary-owner-registration-bundle"),
    "owner registration falls outside the storm",
  );
  restoreFrozenHistories();

  const missingSuccessorMaintenance = [schedulerAMaintenance];
  writePublishedAndFrozenHistory(
    schedulerMaintenanceHistoryPath,
    "scheduler_maintenance_history",
    missingSuccessorMaintenance,
  );
  expectBuilderFailure(
    join(work, "missing-scheduler-maintenance-completion-bundle"),
    "leadership epoch 2 has no exact-term fenced maintenance completion",
  );
  restoreFrozenHistories();

  const earlierReplacementMaintenance = `2002:sched-b:${schedulerBIncarnation}:2:${atMicros(43, 400000)}`;
  const laterObservedReplacementMaintenance = schedulerBMaintenance.replace(
    "2002:",
    "2003:",
  );
  writePublishedAndFrozenHistory(
    schedulerMaintenanceHistoryPath,
    "scheduler_maintenance_history",
    [
      schedulerAMaintenance,
      earlierReplacementMaintenance,
      laterObservedReplacementMaintenance,
    ],
  );
  write(
    schedulerMaintenanceObservationPath,
    JSON.stringify({
      ...schedulerMaintenanceObservation,
      maintenance_id: "2003",
    }),
  );
  expectBuilderFailure(
    join(work, "scheduler-observation-not-first-marker-bundle"),
    "scheduler maintenance observation does not bind the first durable replacement marker",
  );
  write(
    schedulerMaintenanceObservationPath,
    originalSchedulerMaintenanceObservation,
  );
  restoreFrozenHistories();

  const mismatchedSchedulerMaintenance = [
    schedulerAMaintenance,
    schedulerBMaintenance.replace(
      `:${schedulerBIncarnation}:2:`,
      `:${schedulerAIncarnation}:2:`,
    ),
  ];
  writePublishedAndFrozenHistory(
    schedulerMaintenanceHistoryPath,
    "scheduler_maintenance_history",
    mismatchedSchedulerMaintenance,
  );
  expectBuilderFailure(
    join(work, "mismatched-scheduler-maintenance-term-bundle"),
    "references an unacquired exact term",
  );
  restoreFrozenHistories();

  const staleSchedulerMaintenance = [
    schedulerAMaintenance.replace(atMicros(2, 500000), atMicros(41, 0)),
    schedulerBMaintenance,
  ];
  writePublishedAndFrozenHistory(
    schedulerMaintenanceHistoryPath,
    "scheduler_maintenance_history",
    staleSchedulerMaintenance,
  );
  expectBuilderFailure(
    join(work, "stale-scheduler-maintenance-term-bundle"),
    "was not committed under a live exact term",
  );
  restoreFrozenHistories();

  const postCutSchedulerMaintenance = [
    schedulerAMaintenance,
    schedulerBMaintenance.replace(atMicros(43, 500000), atMicros(321, 0)),
  ];
  writePublishedAndFrozenHistory(
    schedulerMaintenanceHistoryPath,
    "scheduler_maintenance_history",
    postCutSchedulerMaintenance,
  );
  expectBuilderFailure(
    join(work, "post-cut-scheduler-maintenance-bundle"),
    "was committed after the final authority cut",
  );
  restoreFrozenHistories();

  const mismatchedLiveSchedulerCut = JSON.parse(originalAuthorityCut);
  mismatchedLiveSchedulerCut.leader.incarnation = schedulerAIncarnation;
  write(finalAuthorityCutPath, JSON.stringify(mismatchedLiveSchedulerCut));
  expectBuilderFailure(
    join(work, "mismatched-live-scheduler-cut-bundle"),
    "frozen leadership journal does not match the live final authority term",
  );
  restoreFrozenHistories();

  const liveRenewedLeaderHistory = `1002:sched-a:${schedulerAIncarnation}:1:${atMicros(43, 0)}:${atMicros(21, 0)}`;
  const earlyLeaderTakeover = completeSchedulerHistory.map((line) =>
    line === renewedLeaderHistory ? liveRenewedLeaderHistory : line,
  );
  if (!earlyLeaderTakeover.includes(liveRenewedLeaderHistory)) {
    throw new Error("early scheduler takeover fixture is invalid");
  }
  writePublishedAndFrozenHistory(
    leadershipHistoryPath,
    "scheduler_history",
    earlyLeaderTakeover,
  );
  expectBuilderFailure(
    join(work, "early-scheduler-takeover-bundle"),
    "replaces live epoch 1 before lease expiry",
  );
  restoreFrozenHistories();

  console.log(
    "G6 readiness evidence builder produced a verifier-passing bundle from synthetic producer state",
  );
} finally {
  rmSync(work, { recursive: true, force: true });
}
