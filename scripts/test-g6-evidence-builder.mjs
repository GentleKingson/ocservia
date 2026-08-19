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
  join(peerDir, "isolation"),
  join(peerDir, "pitr"),
  join(peerDir, "evidence"),
]) {
  mkdirSync(directory, { recursive: true });
}
const write = (path, content) => writeFileSync(path, content);

// ---------------------------------------------------------------------------
// The managed fleet: 28 agents on fd-a, 27 on fd-b.
// ---------------------------------------------------------------------------

const nodes = [];
for (let index = 1; index <= 28; index += 1) {
  nodes.push({
    name: `g6-fd-a-${String(index).padStart(2, "0")}`,
    nodeId: fakeUuid(index),
    endpointId: index.toString(16).padStart(64, "0"),
  });
}
for (let index = 1; index <= 27; index += 1) {
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

function enqueueCommand(nodeIndex, stampSeconds, commandState = "succeeded") {
  const commandId = fakeUuid(commandSeed + 500, 7);
  const key = `g6-window-run-${commandSeed}`;
  commandSeed += 1;
  if (commandState === "rejected") rejectedCommandId = commandId;
  enqueueLog.push({
    at: at(stampSeconds),
    node: nodes[nodeIndex].nodeId,
    idempotency_key: key,
    status: 202,
    latency_seconds: 0.05,
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
      result: "intent",
      occurred_at: at(stampSeconds),
    },
    {
      command_id: commandId,
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
      enqueueCommand(nodeIndex, 5);
    }
  }
  enqueueCommand(
    tick % nodes.length,
    5 + tick * 3,
    tick === 0 ? "rejected" : "succeeded",
  );
  enqueueCommand((tick + 1) % nodes.length, 5 + tick * 3);
}

// A successful command from before the bounded HTTP/SLO window proves that
// durable-effect completeness is checked against the run-wide synthetic
// population without widening the window-only HTTP/dispatch denominator.
const historicalCommandId = fakeUuid(9000, 7);
commands.push({
  id: historicalCommandId,
  idempotency_key: "g6-load-test-run-history",
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
  { command_id: historicalCommandId, result: "intent", occurred_at: at(1) },
  {
    command_id: historicalCommandId,
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
    owner_epoch: index < 2 ? 3 : 1,
    owner_lease_until: at(350),
    owner_fence_id: fakeUuid(index + 4000).replaceAll("-", ""),
    authorization_revision: 11,
    negotiated_capabilities: ["ocserv.status.read"],
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
    queued_outbox_count: 55,
    commands: commands.slice(0, 55).map((command) => ({
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

function instanceLine(service, startedSeconds, finishedSeconds, digestByte) {
  const finished =
    finishedSeconds === undefined
      ? "0001-01-01T00:00:00Z"
      : at(finishedSeconds);
  return `/${service}\t${digestOf(digestByte)}\t${at(startedSeconds)}\t${finished}\t${service}`;
}
const peerInstances = [
  instanceLine("postgres", 200, undefined, "d"),
  instanceLine("api", 30, 51, "a"),
  instanceLine("worker", 30, 51, "a"),
  instanceLine("scheduler", 30, 51, "a"),
  instanceLine("transportd", 30, 51, "b"),
  instanceLine("relay", 30, 182, "c"),
];
for (let index = 1; index <= 28; index += 1) {
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
  instanceLine("api", 121, undefined, "a"),
  instanceLine("worker", 121, undefined, "a"),
  instanceLine("scheduler", 121, undefined, "a"),
  instanceLine("transportd", 121, undefined, "b"),
  instanceLine("relay", 55, undefined, "c"),
];
for (let index = 1; index <= 27; index += 1) {
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
  ["api_slo_measured", 310],
];
write(
  join(runDir, "outbox", "timeline.jsonl"),
  jsonl(
    timeline.map(([eventId, seconds], index) => ({
      sequence: index + 1,
      timestamp: at(seconds),
      environment_id: environmentId,
      candidate_sha: candidateSha,
      event_id: eventId,
    })),
  ),
);

const node1Hex = nodes[0].nodeId.replaceAll("-", "");
const node2Hex = nodes[1].nodeId.replaceAll("-", "");
const node1ConnectionHex = fakeUuid(2001).replaceAll("-", "");
const node2ConnectionHex = fakeUuid(2002).replaceAll("-", "");
const initialOwnerConnections = nodes.map((_, index) =>
  fakeUuid(index + 1000).replaceAll("-", ""),
);
const initialOwnerHistory = nodes.map(
  (node, index) =>
    `${node.nodeId.replaceAll("-", "")}:worker-a:${workerAIncarnation}:${initialOwnerConnections[index]}:1:${index < 2 ? at(31) : at(350)}:${at(1)}`,
);
const takeoverOwnerHistory = [
  `${node1Hex}:worker-b:${workerBIncarnation}:${node1ConnectionHex}:2:${at(66)}:${at(36)}`,
  `${node2Hex}:worker-b:${workerBIncarnation}:${node2ConnectionHex}:2:${at(66)}:${at(36)}`,
  `${node1Hex}:worker-b:${workerBIncarnation}:${node1ConnectionHex}:3:${at(350)}:${at(66)}`,
  `${node2Hex}:worker-b:${workerBIncarnation}:${node2ConnectionHex}:3:${at(350)}:${at(66)}`,
];
write(
  join(runDir, "outbox", "fencing-history.jsonl"),
  [...initialOwnerHistory, ...takeoverOwnerHistory].join("\n") + "\n",
);
const finalOwnerHistory = new Map(
  nodes.map((node, index) => [
    node.nodeId.replaceAll("-", ""),
    index < 2
      ? takeoverOwnerHistory[index + 2]
      : initialOwnerHistory[index],
  ]),
);
const finalLeaderHistory = `sched-b:${schedulerBIncarnation}:2:${at(350)}:${at(62)}`;
write(
  join(runDir, "outbox", "leadership-history.jsonl"),
  [
    `sched-a:${schedulerAIncarnation}:1:${at(21)}:${at(1)}`,
    `sched-a:${schedulerAIncarnation}:1:${at(41)}:${at(21)}`,
    `sched-b:${schedulerBIncarnation}:2:${at(62)}:${at(42)}`,
    finalLeaderHistory,
  ].join("\n") + "\n",
);
write(
  join(runDir, "state", "final-authority-cut.json"),
  JSON.stringify({
    cut_at: atMicros(320, 200000),
    owners: nodes.map((node, index) => {
      const nodeHex = node.nodeId.replaceAll("-", "");
      return {
        node_hex: nodeHex,
        owner_instance_id: index < 2 ? "worker-b" : "worker-a",
        owner_incarnation:
          index < 2 ? workerBIncarnation : workerAIncarnation,
        connection_id:
          index === 0
            ? node1ConnectionHex
            : index === 1
              ? node2ConnectionHex
              : initialOwnerConnections[index],
        owner_epoch: index < 2 ? 3 : 1,
        lease_until: at(350),
        history: finalOwnerHistory.get(nodeHex),
      };
    }),
    leader: {
      instance_id: "sched-b",
      incarnation: schedulerBIncarnation,
      epoch: 2,
      lease_until: at(350),
      history: finalLeaderHistory,
    },
  }),
);

// ---------------------------------------------------------------------------
// Run the builder and verify both authorities.
// ---------------------------------------------------------------------------

function runBuilder(outDir, authority) {
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
  const rejectedTraceResult = readFileSync(
    join(productionDir, "command-trace.jsonl"),
    "utf8",
  )
    .trimEnd()
    .split("\n")
    .map((line) => JSON.parse(line))
    .find(
      (record) =>
        record.record_type === "result" &&
        record.command_id === rejectedCommandId,
    );
  if (rejectedTraceResult?.outcome !== "failed") {
    throw new Error(
      "builder must map an accepted command's rejected terminal result to failed",
    );
  }
  const builtSessionInventory = JSON.parse(
    readFileSync(join(productionDir, "agent-sessions.json"), "utf8"),
  );
  const expectedSessionByNode = new Map(
    finalSessionInventory.observations.map((observation) => [
      observation.node_id,
      observation,
    ]),
  );
  const sourceAuthorityCut = JSON.parse(
    readFileSync(join(runDir, "state", "final-authority-cut.json"), "utf8"),
  );
  const builtAuthorityCut = JSON.parse(
    readFileSync(join(productionDir, "authority-cut.json"), "utf8"),
  );
  const builtEvidence = JSON.parse(
    readFileSync(join(productionDir, "evidence.json"), "utf8"),
  );
  const sourceInventory = JSON.parse(
    readFileSync(join(productionDir, "source-inventory.json"), "utf8"),
  );
  if (
    sourceInventory.schema_version !== "ocservia.g6-source-inventory.v1" ||
    !sourceInventory.sources.some(
      (source) => source.path === "peer/isolation/active-load.json",
    ) ||
    !sourceInventory.sources.some(
      (source) => source.path === "contract/g6-slo.yaml",
    ) ||
    sourceInventory.sources.some((source) => source.path.startsWith("/"))
  ) {
    throw new Error(
      "builder source inventory must preserve safe relative input identities",
    );
  }
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
      built?.lease_until !== owner.lease_until
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
    builtAuthorityCut.scheduler.lease_until !==
      sourceAuthorityCut.leader.lease_until
  ) {
    throw new Error("builder did not preserve the scheduler authority tuple");
  }
  const expectedLeaseByNode = new Map(
    sourceAuthorityCut.owners.map((owner) => [
      owner.node_hex,
      owner.lease_until,
    ]),
  );
  for (const session of builtSessionInventory.sessions) {
    const expectedSession = expectedSessionByNode.get(session.node);
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
    builtSessionInventory.scheduler_authority.lease_until !==
      sourceAuthorityCut.leader.lease_until
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
    "owner_epoch 2 does not match latest connection-owner epoch 3",
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
    `owner_epoch 3 is not active for node ${node1Hex}`,
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

  const duplicateTargetPath = join(effectDir, "agent-fd-b-27.tsv");
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

  const missingAgentPath = join(effectDir, "agent-fd-a-28.tsv");
  const missingAgentEffects = readFileSync(missingAgentPath, "utf8");
  rmSync(missingAgentPath);
  expectBuilderFailure(
    join(work, "missing-agent-freeze-bundle"),
    "does not cover exactly all managed Agents",
  );
  write(missingAgentPath, missingAgentEffects);

  const commandsPath = join(runDir, "state", "evidence", "commands.jsonl");
  const originalCommands = readFileSync(commandsPath, "utf8");
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

  console.log(
    "G6 readiness evidence builder produced a verifier-passing bundle from synthetic producer state",
  );
} finally {
  rmSync(work, { recursive: true, force: true });
}
