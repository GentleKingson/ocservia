#!/usr/bin/env node

// Integration test for the G6 readiness evidence builder: a complete
// synthetic harness state (both failure domains, every trusted-producer
// format the fd-a/fd-b phases freeze) is assembled, run through
// build-g6-evidence.mjs, and the resulting bundle must be awarded a final
// G6 pass by the shared verifier. The same bundle assembled under the
// engineering authority must stay non-final for the authority reason alone.

import { spawnSync } from "node:child_process";
import {
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { verifyG6 } from "./g6-contract-lib.mjs";

const root = fileURLToPath(new URL("../", import.meta.url));
const environmentId = "g6-abcd1234";
const candidateSha = "2234567890123456789012345678901234567890";
const base = Date.parse("2026-08-14T10:00:00Z");
const at = (seconds) =>
  `${new Date(base + seconds * 1000).toISOString().slice(0, 19)}Z`;
const epochSeconds = (seconds) => Math.floor((base + seconds * 1000) / 1000);

function fakeUuid(seed, version = 7) {
  // the seed appears in both kept groups so every distinct seed yields a
  // distinct uuid (and a distinct dash-stripped 32-hex identity)
  const group = seed.toString(16).padStart(4, "0");
  const hex = `${group}0000${group}00000000`;
  return `${hex.slice(0, 8)}-0000-${version}00-8000-${hex.slice(16, 28)}`;
}

const digestOf = (byte) =>
  `sha256:${byte.repeat(64)}`;

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
  });
}
for (let index = 1; index <= 27; index += 1) {
  nodes.push({
    name: `g6-fd-b-${String(index).padStart(2, "0")}`,
    nodeId: fakeUuid(index + 100),
  });
}
write(
  join(runDir, "state", "all-nodes.tsv"),
  `${nodes.map((node) => `${node.name}\t${node.nodeId}\t${fakeUuid(1, 5)}`).join("\n")}\n`,
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

function enqueueCommand(nodeIndex, stampSeconds) {
  const commandId = fakeUuid(commandSeed + 500, 7);
  const key = `g6-window-run-${commandSeed}`;
  commandSeed += 1;
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
    state: "succeeded",
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
      result: "succeeded",
      occurred_at: at(completion),
    },
  );
  const agentFile = `${nodes[nodeIndex].name.replace("g6-", "agent-")}.tsv`;
  const lines = effectsByAgent.get(agentFile) ?? [];
  lines.push(
    `${fakeUuid(commandSeed, 3).replaceAll("-", "")} ${commandId.replaceAll("-", "")} ${epochSeconds(stampSeconds + 2)}`,
  );
  effectsByAgent.set(agentFile, lines);
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
      [stamp, component, instance, rss, fdCount, tasks, queue, db, environmentId, candidateSha].join(
        ",",
      ),
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
  enqueueCommand(tick % nodes.length, 5 + tick * 3);
  enqueueCommand((tick + 1) % nodes.length, 5 + tick * 3);
}
write(join(runDir, "state", "resource-samples.csv"), `${samplerLines.join("\n")}\n`);
write(join(runDir, "state", "read-log.jsonl"), jsonl(readLog));
write(join(runDir, "state", "enqueue-log.jsonl"), jsonl(enqueueLog));
write(join(runDir, "state", "evidence", "commands.jsonl"), jsonl(commands));
write(join(runDir, "state", "evidence", "attempts.jsonl"), jsonl(attempts));
write(join(runDir, "state", "evidence", "outbox.jsonl"), jsonl(outboxRows));
write(join(runDir, "state", "evidence", "audit.jsonl"), jsonl(auditRows));
for (const [name, lines] of effectsByAgent) {
  write(join(runDir, "state", "evidence", "effects", name), `${lines.join("\n")}\n`);
}

// ---------------------------------------------------------------------------
// Sessions, telemetry, and the peer's failure-domain evidence.
// ---------------------------------------------------------------------------

write(
  join(runDir, "state", "era2-sessions.tsv"),
  `${nodes.map((node, index) => `${node.nodeId}\t${at(130 + (index % 5))}`).join("\n")}\n`,
);
write(
  join(runDir, "state", "evidence", "final-sessions.json"),
  JSON.stringify({
    mode: "node_connection",
    all_matched: true,
    observations: nodes.map((node, index) => ({
      node_id: node.nodeId,
      found: true,
      path: "direct",
      connected_at: at(160 + (index % 10)),
    })),
  }),
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
write(join(runDir, "state", "evidence", "snapshot-taken-at"), `${at(320)}\n`);
write(join(runDir, "state", "promoted-at"), `${at(120)}\n`);
write(join(runDir, "state", "stale-transport-probe.json"), JSON.stringify({ status: "rejected" }));
write(join(runDir, "state", "stale-agent-probe.json"), JSON.stringify({ status: "rejected" }));
write(
  join(runDir, "state", "owner-a-terms.tsv"),
  `${fakeUuid(1).replaceAll("-", "")}:worker-a:1:${fakeUuid(9).replaceAll("-", "")}:1\n`,
);
write(join(runDir, "state", "stale-scheduler-term"), "sched-a:1:1\n");
write(
  join(runDir, "state", "evidence", "failure-domain.txt"),
  "failure_domain=fd-b\nalias=fd-beta\n",
);

write(
  join(peerDir, "isolation", "isolation.json"),
  JSON.stringify({
    outage_declared_at: at(50),
    isolated_at: at(51),
    failure_domain: "fd-a",
  }),
);
write(join(peerDir, "isolation", "outage-declared-at"), `${at(50)}\n`);
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
write(join(peerDir, "evidence", "failure-domain.txt"), "failure_domain=fd-a\nalias=fd-alpha\n");

function instanceLine(service, startedSeconds, finishedSeconds, digestByte) {
  const finished =
    finishedSeconds === undefined ? "0001-01-01T00:00:00Z" : at(finishedSeconds);
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
    instanceLine(`agent-fd-a-${String(index).padStart(2, "0")}`, 30, undefined, "e"),
  );
}
write(join(peerDir, "evidence", "instances.tsv"), `${peerInstances.join("\n")}\n`);
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
    instanceLine(`agent-fd-b-${String(index).padStart(2, "0")}`, 125, undefined, "e"),
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

const node1Hex = fakeUuid(1).replaceAll("-", "");
const node2Hex = fakeUuid(2).replaceAll("-", "");
write(
  join(runDir, "outbox", "fencing-history.jsonl"),
  [
    `${node1Hex}:worker-a:1:aa:1:${at(31)}:${at(1)}`,
    `${node2Hex}:worker-a:1:bb:1:${at(31)}:${at(1)}`,
    `${node1Hex}:worker-b:2:cc:2:${at(66)}:${at(36)}`,
    `${node2Hex}:worker-b:2:dd:2:${at(66)}:${at(36)}`,
    `${node1Hex}:worker-b:2:cc:3:${at(96)}:${at(66)}`,
    `${node2Hex}:worker-b:2:dd:3:${at(96)}:${at(66)}`,
  ].join("\n") + "\n",
);
write(
  join(runDir, "outbox", "leadership-history.jsonl"),
  [
    `sched-a:1:1:${at(21)}:${at(1)}`,
    `sched-a:1:1:${at(41)}:${at(21)}`,
    `sched-b:2:2:${at(62)}:${at(42)}`,
    `sched-b:2:2:${at(92)}:${at(62)}`,
  ].join("\n") + "\n",
);

// ---------------------------------------------------------------------------
// Run the builder and verify both authorities.
// ---------------------------------------------------------------------------

function runBuilder(outDir, authority) {
  const result = spawnSync(
    process.execPath,
    [
      join(root, "scripts", "build-g6-evidence.mjs"),
      "--run-dir", runDir,
      "--peer-dir", peerDir,
      "--out-dir", outDir,
      "--slo", join(root, "docs", "acceptance", "g6-slo.yaml"),
      "--environment-id", environmentId,
      "--candidate-sha", candidateSha,
      "--authority", authority,
      "--failure-domain-class", "multi_host",
      "--run-id", "test-run",
    ],
    { encoding: "utf8" },
  );
  if (result.status !== 0) {
    throw new Error(
      `builder failed (${authority}): ${result.stderr}${result.stdout}`,
    );
  }
}

function verifyBundle(outDir, authority) {
  return verifyG6({
    sloText: readFileSync(join(root, "docs", "acceptance", "g6-slo.yaml"), "utf8"),
    evidenceText: readFileSync(join(outDir, "evidence.json"), "utf8"),
    topologyText: readFileSync(join(outDir, "topology.json"), "utf8"),
    manifestText: readFileSync(join(outDir, "release-manifest.json"), "utf8"),
    artifactRoot: outDir,
    expectedAuthority: authority,
    expectedEnvironmentId: environmentId,
    expectedFailureDomainClass: "multi_host",
  });
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
  console.log(
    "G6 readiness evidence builder produced a verifier-passing bundle from synthetic producer state",
  );
} finally {
  rmSync(work, { recursive: true, force: true });
}
