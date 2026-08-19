#!/usr/bin/env node

import {
  cpSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { sha256Digest, verifyG6 } from "./g6-contract-lib.mjs";

const root = new URL("../", import.meta.url);
const read = (path) => readFileSync(new URL(path, root), "utf8");
const parse = (path) => JSON.parse(read(path));
const clone = (value) => structuredClone(value);
const serialize = (value) => `${JSON.stringify(value, null, 2)}\n`;

function mutate(target, mutation) {
  const parts = mutation.path.split(".");
  const key = parts.pop();
  let parent = target;
  for (const part of parts) parent = parent[part];
  if (mutation.operation === "replace" && !(key in parent)) {
    throw new Error(`replace target is missing: ${mutation.path}`);
  }
  if (!new Set(["add", "replace"]).has(mutation.operation)) {
    throw new Error(`unsupported fixture mutation: ${mutation.operation}`);
  }
  parent[key] = mutation.value;
}

const allowedFixtureFields = new Set([
  "base",
  "topology_mutations",
  "evidence_mutations",
  "mutations",
  "slo_replacements",
  "expected_error",
  "expected_failure_reason",
  "expected_authority",
  "expected_failure_domain_class",
  "artifact_root",
  "artifact_files",
  "rebind_overridden_artifacts",
  "expected_all_measurements_pass",
]);

function buildArtifactRoot(mode, overrides) {
  const realRoot = fileURLToPath(new URL("testdata/g6/artifacts/", root));
  const overrideEntries = Object.entries(overrides ?? {});
  if (mode !== "symlink" && overrideEntries.length === 0) return realRoot;
  const tempRoot = mkdtempSync(join(tmpdir(), "g6-artifacts-"));
  cpSync(realRoot, tempRoot, { recursive: true });
  for (const [name, content] of overrideEntries) {
    writeFileSync(join(tempRoot, name), content);
  }
  if (mode === "symlink") {
    rmSync(join(tempRoot, "resource-samples.csv"));
    symlinkSync(
      join(realRoot, "resource-samples.csv"),
      join(tempRoot, "resource-samples.csv"),
    );
  }
  return tempRoot;
}

// Re-binds evidence digests to tampered artifact bytes: the attack modeled
// here swaps artifact content AND updates the evidence digest, so only the
// per-record environment/candidate binding or artifact recomputation can
// catch it.
function rebindOverriddenArtifacts(evidence, overrides) {
  for (const [name, content] of Object.entries(overrides)) {
    const nextDigest = sha256Digest(content);
    const entry = evidence.artifacts.find((artifact) => artifact.name === name);
    if (!entry) throw new Error(`override for undeclared artifact: ${name}`);
    const previousDigest = entry.digest;
    entry.digest = nextDigest;
    for (const measurement of Object.values(evidence.measurements)) {
      if (measurement.source_artifact_digest === previousDigest) {
        measurement.source_artifact_digest = nextDigest;
      }
    }
    for (const observation of Object.values(evidence.observations)) {
      if (observation.source_artifact_digest === previousDigest) {
        observation.source_artifact_digest = nextDigest;
      }
    }
  }
}

function rebindDeclaredArtifacts(evidence, overrides) {
  for (const [name, content] of Object.entries(overrides)) {
    const entry = evidence.artifacts.find((artifact) => artifact.name === name);
    if (!entry) continue;
    const nextDigest = sha256Digest(content);
    const previousDigest = entry.digest;
    entry.digest = nextDigest;
    for (const measurement of Object.values(evidence.measurements)) {
      if (measurement.source_artifact_digest === previousDigest) {
        measurement.source_artifact_digest = nextDigest;
      }
    }
    for (const observation of Object.values(evidence.observations)) {
      if (observation.source_artifact_digest === previousDigest) {
        observation.source_artifact_digest = nextDigest;
      }
    }
  }
}

const baseSloText = read("docs/acceptance/g6-slo.yaml");
const manifestText = read("testdata/g6/release-manifest.json");
const baseTopology = parse("testdata/g6/topology.json");
const baseEvidence = parse("testdata/g6/evidence-pass.json");

function buildModernReconnectFixture() {
  const sessionsText = read("testdata/g6/artifacts/agent-sessions.json");
  const authorityText = read("testdata/g6/artifacts/authority-cut.json");
  const epochEventsText = read("testdata/g6/artifacts/epoch-events.jsonl");
  const timelineText = read("testdata/g6/artifacts/timeline.jsonl");
  const sessions = JSON.parse(sessionsText);
  const epochEvents = epochEventsText
    .trimEnd()
    .split("\n")
    .map(JSON.parse);
  const reconnectFields = [
    "reconnect_owner_instance",
    "reconnect_owner_incarnation",
    "reconnect_connection_id",
    "reconnect_owner_epoch",
  ];
  if (
    !sessions.sessions.every((session) =>
      reconnectFields.every((field) => Object.hasOwn(session, field)),
    )
  ) {
    throw new Error("generated G6 sessions lack durable reconnect bindings");
  }
  return {
    artifacts: {
      "agent-sessions.json": sessionsText,
      "authority-cut.json": authorityText,
      "epoch-events.jsonl": epochEventsText,
      "timeline.jsonl": timelineText,
    },
    sessionByNode: new Map(
      sessions.sessions.map((session) => [session.node, clone(session)]),
    ),
    epochEvents,
  };
}

const modernReconnectFixture = buildModernReconnectFixture();
// Negative fixtures intentionally retain the pre-reconnect base artifact
// digests. Translate those references to the regenerated base while leaving
// their semantic kind swaps and tampering intact.
const legacyReconnectDigestByName = new Map([
  [
    "resource-samples.csv",
    "sha256:125b6bf53f30ad86d9b61ff388d93c9308821fc26f05be662b3bd16c86acbdf0",
  ],
  [
    "timeline.jsonl",
    "sha256:1ff4b29c99aaec5360460ea473d295ca9e9e515d8f8d6ed0ae3170a74ba962ca",
  ],
  [
    "epoch-events.jsonl",
    "sha256:27f572589bcab84594581e342f516ef26c13d7e72f40d3a4dbd30d6b1188e674",
  ],
  [
    "authority-cut.json",
    "sha256:cb401b1620092cf94991c0b900405f3943a353b283ad732316b9fbabeb4059e7",
  ],
  [
    "agent-sessions.json",
    "sha256:258d2f9b465ce78955844f5e06539a42f7fc4b06c2cc4a1a7c1008bb1f93461a",
  ],
  [
    "relay-transitions.jsonl",
    "sha256:7885e7e0d8d4c2c05021fc06dd88cedc75f3088cf82301f27214d2bf2fa64598",
  ],
  [
    "command-trace.jsonl",
    "sha256:aa4cae6abc87801a45491100dfe06478b6ef81248656f069ca05f86bf26b1e6c",
  ],
  [
    "postgres-recovery.json",
    "sha256:95cfc2fdaf6c3ee952b1424cfbd7654523bdcee455c678a7714d331d0ddd352a",
  ],
  [
    "http-samples.csv",
    "sha256:1e9b73c0fe40c55c39539c311bbe2f9627390358bae5a31734a58afb82301971",
  ],
  [
    "audit-correlation.json",
    "sha256:9c851bada1eba8b95e62484a1e156cea6b681372d10271ad16bf8de33e8efff6",
  ],
]);
const modernDigestByLegacyDigest = new Map();
for (const [name, content] of Object.entries(modernReconnectFixture.artifacts)) {
  const legacyDigest = legacyReconnectDigestByName.get(name);
  if (legacyDigest) {
    modernDigestByLegacyDigest.set(legacyDigest, sha256Digest(content));
  }
}
modernDigestByLegacyDigest.set(
  legacyReconnectDigestByName.get("resource-samples.csv"),
  sha256Digest(read("testdata/g6/artifacts/resource-samples.csv")),
);
modernDigestByLegacyDigest.set(
  legacyReconnectDigestByName.get("relay-transitions.jsonl"),
  sha256Digest(read("testdata/g6/artifacts/relay-transitions.jsonl")),
);
modernDigestByLegacyDigest.set(
  legacyReconnectDigestByName.get("command-trace.jsonl"),
  sha256Digest(read("testdata/g6/artifacts/command-trace.jsonl")),
);
modernDigestByLegacyDigest.set(
  legacyReconnectDigestByName.get("postgres-recovery.json"),
  sha256Digest(read("testdata/g6/artifacts/postgres-recovery.json")),
);
modernDigestByLegacyDigest.set(
  legacyReconnectDigestByName.get("http-samples.csv"),
  sha256Digest(read("testdata/g6/artifacts/http-samples.csv")),
);
modernDigestByLegacyDigest.set(
  legacyReconnectDigestByName.get("audit-correlation.json"),
  sha256Digest(read("testdata/g6/artifacts/audit-correlation.json")),
);

function modernizeLegacySourceDigests(evidence) {
  for (const artifact of evidence.artifacts) {
    const digest = modernDigestByLegacyDigest.get(artifact.digest);
    if (digest) artifact.digest = digest;
  }
  for (const record of [
    ...Object.values(evidence.measurements),
    ...Object.values(evidence.observations),
  ]) {
    const digest = modernDigestByLegacyDigest.get(record.source_artifact_digest);
    if (digest) record.source_artifact_digest = digest;
  }
}

function modernizeFixtureOverrides(name, overrides = {}) {
  const modernized = { ...overrides };
  if (modernized["command-trace.jsonl"]) {
    const records = read("testdata/g6/artifacts/command-trace.jsonl")
      .trimEnd()
      .split("\n")
      .map(JSON.parse);
    const append = (record) => {
      records.push({
        sequence: records.at(-1).sequence + 1,
        timestamp: "2026-08-14T00:02:50.000000Z",
        environment_id: "g6-12345678",
        candidate_sha: "1".repeat(40),
        ...record,
      });
    };
    if (name === "evidence-command-duplicate-effect.json") {
      append({
        record_type: "effect",
        command_id: "cmd-001",
        idempotency_key: "idem-001",
        effect_id: "fx-duplicate-001",
      });
    } else if (name === "evidence-command-duplicate-result.json") {
      append({
        record_type: "result",
        command_id: "cmd-001",
        outcome: "success",
      });
    } else if (name === "evidence-command-unmatched-result.json") {
      append({
        record_type: "result",
        command_id: "cmd-999",
        outcome: "success",
      });
    } else if (name === "evidence-command-undispatched-miss.json") {
      append({
        record_type: "enqueued",
        command_id: "cmd-601",
        idempotency_key: "idem-http-601",
      });
    }
    modernized["command-trace.jsonl"] = `${records
      .map(JSON.stringify)
      .join("\n")}\n`;
  }
  if (modernized["resource-samples.csv"]) {
    const canonical = read("testdata/g6/artifacts/resource-samples.csv")
      .trimEnd()
      .split("\n");
    if (name === "evidence-samples-artifact-gap-fail.json") {
      const displaced = canonical
        .filter((line) => line.startsWith("2026-08-14T00:00:50Z,"))
        .map((line) =>
          line.replace("2026-08-14T00:00:50Z", "2026-08-14T00:00:02Z"),
        );
      const remaining = canonical.filter(
        (line) => !line.startsWith("2026-08-14T00:00:50Z,"),
      );
      remaining.splice(7, 0, ...displaced);
      modernized["resource-samples.csv"] = `${remaining.join("\n")}\n`;
    } else if (name === "evidence-samples-artifact-malformed.json") {
      canonical[1] = canonical[1].replace(
        "2026-08-14T00:00:00Z",
        "2026-08-14 00:00:00",
      );
      modernized["resource-samples.csv"] = `${canonical.join("\n")}\n`;
    }
  }
  if (modernized["http-samples.csv"]) {
    if (
      new Set([
        "evidence-command-missing-from-trace.json",
        "evidence-command-undispatched-miss.json",
      ]).has(name)
    ) {
      const canonical = read("testdata/g6/artifacts/http-samples.csv")
        .trimEnd()
        .split("\n");
      canonical.push(
        [
          "2026-08-14T00:04:59Z",
          "enqueue",
          "ok",
          "0.05",
          "idem-http-601.attempt-1",
          "idem-http-601",
          "1",
          "3",
          "610",
          "202",
          "",
          "",
          "cmd-601",
          "g6-12345678",
          "1".repeat(40),
        ].join(","),
      );
      modernized["http-samples.csv"] = `${canonical.join("\n")}\n`;
    }
  }
  if (modernized["relay-transitions.jsonl"]) {
    const records = read("testdata/g6/artifacts/relay-transitions.jsonl")
      .trimEnd()
      .split("\n")
      .map(JSON.parse);
    const replacementPath = records.find(
      (record) =>
        record.event_type === "path_active" && record.relay === "relay-b",
    );
    if (name === "evidence-relay-takeover-order.json") {
      const predecessorPath = records.find(
        (record) =>
          record.event_type === "path_active" && record.relay === "relay-a",
      );
      replacementPath.relay = "relay-a";
      replacementPath.path_detail = "iroh/relay-a";
      delete replacementPath.relay_b_started_at;
      replacementPath.topology_mode = predecessorPath.topology_mode;
      replacementPath.topology_network_name =
        predecessorPath.topology_network_name;
      replacementPath.topology_agent_service =
        predecessorPath.topology_agent_service;
      replacementPath.topology_network_internal =
        predecessorPath.topology_network_internal;
      replacementPath.topology_agent_default_network_connected =
        predecessorPath.topology_agent_default_network_connected;
      replacementPath.topology_ready_at = predecessorPath.topology_ready_at;
      replacementPath.relay_b_disabled_at = predecessorPath.relay_b_disabled_at;
    } else if (name === "evidence-relay-unauthenticated-takeover.json") {
      replacementPath.authenticated = false;
    }
    modernized["relay-transitions.jsonl"] = `${records
      .map(JSON.stringify)
      .join("\n")}\n`;
  }
  if (modernized["epoch-events.jsonl"]) {
    const records = modernReconnectFixture.epochEvents.map(clone);
    if (name === "evidence-artifact-candidate-swap.json") {
      records[0].candidate_sha = "2".repeat(40);
    } else if (name === "evidence-epoch-clock-regression.json") {
      const firstTakeover = records.find(
        (record) =>
          record.subject === "connection_owner" &&
          record.event_type === "owner_registered" &&
          record.epoch === 2,
      );
      firstTakeover.timestamp = "2026-08-14T00:00:10.050000Z";
    } else if (name === "evidence-epoch-duplicate-sequence.json") {
      records.at(-1).sequence = records.at(-2).sequence;
    } else if (name === "evidence-epoch-stale-accept-recorded.json") {
      const staleAccept = records.find(
        (record) =>
          record.subject === "connection_owner" &&
          record.event_type === "owner_accept" &&
          record.accepted === false,
      );
      staleAccept.accepted = true;
    } else {
      // Preserve any future independently-authored override rather than
      // silently replacing its intended mutation with the canonical stream.
      records.splice(
        0,
        records.length,
        ...modernized["epoch-events.jsonl"]
          .trimEnd()
          .split("\n")
          .map(JSON.parse),
      );
    }
    modernized["epoch-events.jsonl"] = `${records.map(JSON.stringify).join("\n")}\n`;
  }
  if (modernized["timeline.jsonl"]) {
    try {
      const canonicalTimeline = modernReconnectFixture.artifacts["timeline.jsonl"]
        .trimEnd()
        .split("\n")
        .map(JSON.parse);
      const modernTimeline = new Map(
        canonicalTimeline.map((event) => [event.event_id, event]),
      );
      if (name === "evidence-timeline-event-order.json") {
        canonicalTimeline[0].event_id = "primary_failure_injected";
        canonicalTimeline[1].event_id = "load_started";
        modernized["timeline.jsonl"] = `${canonicalTimeline
          .map(JSON.stringify)
          .join("\n")}\n`;
      } else if (name === "evidence-timeline-required-event-missing.json") {
        modernized["timeline.jsonl"] = `${canonicalTimeline
          .filter((event) => event.event_id !== "load_stopped")
          .map(JSON.stringify)
          .join("\n")}\n`;
      } else if (name === "evidence-timeline-artifact-malformed.json") {
        canonicalTimeline[0].timestamp = "2026-08-14T00:00:00:00Z";
        modernized["timeline.jsonl"] = `${canonicalTimeline
          .map(JSON.stringify)
          .join("\n")}\n`;
      } else {
        const records = modernized["timeline.jsonl"]
          .trimEnd()
          .split("\n")
          .map(JSON.parse);
        for (const event of records) {
          const canonical = modernTimeline.get(event.event_id);
          if (
            canonical &&
            (new Set([
              "bulk_disconnect_injected",
              "reconnect_started",
              "reconnect_completed",
            ]).has(event.event_id) ||
              canonical.sequence >= 34)
          ) {
            event.timestamp = canonical.timestamp;
          }
        }
        modernized["timeline.jsonl"] = `${records
          .map(JSON.stringify)
          .join("\n")}\n`;
      }
    } catch {
      // Preserve deliberately malformed fixture bytes for the parser test.
    }
  }
  if (modernized["agent-sessions.json"]) {
    const useCanonicalSessions = new Set([
      "evidence-agent-late-reconnect-claimed-fast.json",
      "evidence-topology-too-few-agents.json",
    ]).has(name);
    const sessions = JSON.parse(
      useCanonicalSessions
        ? modernReconnectFixture.artifacts["agent-sessions.json"]
        : modernized["agent-sessions.json"],
    );
    if (name === "evidence-topology-too-few-agents.json") {
      sessions.sessions = sessions.sessions.slice(0, 49);
    }
    for (const session of sessions.sessions) {
      const canonical = modernReconnectFixture.sessionByNode.get(session.node);
      if (!canonical) continue;
      if (name === "evidence-agent-late-reconnect-claimed-fast.json") {
        Object.assign(session, canonical);
        continue;
      }
      for (const field of [
        "owner_instance",
        "owner_incarnation",
        "connection_id",
        "owner_epoch",
        "owner_lease_until",
        "reconnect_owner_instance",
        "reconnect_owner_incarnation",
        "reconnect_connection_id",
        "reconnect_owner_epoch",
      ]) {
        session[field] = canonical[field];
      }
      session.connected_at = canonical.connected_at;
      session.reconnected_at = canonical.reconnected_at;
    }
    if (name === "evidence-agent-late-reconnect-claimed-fast.json") {
      sessions.sessions[0].reconnected_at = "2026-08-14T00:04:09.999999Z";
    }
    modernized["agent-sessions.json"] = serialize(sessions);
  }
  if (modernized["authority-cut.json"]) {
    const canonicalAuthority = JSON.parse(
      modernReconnectFixture.artifacts["authority-cut.json"],
    );
    const authority =
      new Set([
        "evidence-agent-late-reconnect-claimed-fast.json",
        "evidence-topology-too-few-agents.json",
      ]).has(name)
        ? clone(canonicalAuthority)
        : JSON.parse(modernized["authority-cut.json"]);
    if (name === "evidence-topology-too-few-agents.json") {
      authority.owners = authority.owners.slice(0, 49);
      authority.transport_bracket.before =
        authority.transport_bracket.before.slice(0, 49);
      authority.transport_bracket.after =
        authority.transport_bracket.after.slice(0, 49);
    }
    const ownerByNode = new Map(
      canonicalAuthority.owners.map((owner) => [owner.node, owner]),
    );
    const beforeByNode = new Map(
      canonicalAuthority.transport_bracket.before.map((observation) => [
        observation.node,
        observation,
      ]),
    );
    const afterByNode = new Map(
      canonicalAuthority.transport_bracket.after.map((observation) => [
        observation.node,
        observation,
      ]),
    );
    Object.assign(authority.scheduler, canonicalAuthority.scheduler);
    for (const owner of authority.owners) {
      const canonical = ownerByNode.get(owner.node);
      if (canonical) Object.assign(owner, canonical);
    }
    for (const [side, canonicalByNode] of [
      ["before", beforeByNode],
      ["after", afterByNode],
    ]) {
      for (const observation of authority.transport_bracket[side]) {
        const canonical = canonicalByNode.get(observation.node);
        if (canonical) Object.assign(observation, canonical);
      }
    }
    modernized["authority-cut.json"] = serialize(authority);
  }
  if (
    name === "evidence-topology-too-few-agents.json" &&
    modernized["telemetry-snapshot.json"]
  ) {
    const telemetry = JSON.parse(
      read("testdata/g6/artifacts/telemetry-snapshot.json"),
    );
    telemetry.agents = telemetry.agents.slice(0, 49);
    modernized["telemetry-snapshot.json"] = serialize(telemetry);
  }
  if (modernized["postgres-recovery.json"]) {
    const legacy = JSON.parse(modernized["postgres-recovery.json"]);
    const recovery = parse("testdata/g6/artifacts/postgres-recovery.json");
    recovery.recovery.present_txids = legacy.recovery.present_txids;
    modernized["postgres-recovery.json"] = serialize(recovery);
  }
  if (modernized["audit-correlation.json"]) {
    const correlation = JSON.parse(modernized["audit-correlation.json"]);
    for (const write of correlation.writes) {
      const suffix = write.write_id.match(/^cmd-([0-9]+)$/)?.[1];
      if (!suffix) continue;
      const requestId = `idem-http-${suffix}.attempt-${suffix === "001" ? 2 : 1}`;
      write.intent_request_id = requestId;
      write.result_request_id = requestId;
    }
    modernized["audit-correlation.json"] = serialize(correlation);
  }
  return modernized;
}

function verifyWithArtifactOverrides(evidence, overrides = {}) {
  const artifactRoot = buildArtifactRoot(null, overrides);
  try {
    return verifyG6({
      sloText: baseSloText,
      evidenceText: serialize(evidence),
      topologyText: serialize(baseTopology),
      manifestText,
      artifactRoot,
      expectedAuthority: "production_readiness",
      expectedEnvironmentId: "g6-12345678",
      expectedFailureDomainClass: "multi_host",
    });
  } finally {
    if (artifactRoot.startsWith(join(tmpdir(), "g6-artifacts-"))) {
      rmSync(artifactRoot, { recursive: true, force: true });
    }
  }
}

function resourceSamplesWithFinalValues(changes) {
  const lines = read("testdata/g6/artifacts/resource-samples.csv")
    .trimEnd()
    .split("\n");
  const header = lines[0].split(",");
  const componentColumn = header.indexOf("component");
  const rows = lines.slice(1).map((line) => line.split(","));
  for (const [component, fields] of Object.entries(changes)) {
    const matching = rows.filter((row) => row[componentColumn] === component);
    if (matching.length < 2) {
      throw new Error(`resource fixture lacks a growth window for ${component}`);
    }
    const baseline = matching[0];
    const end = matching.at(-1);
    for (const [field, nextValue] of Object.entries(fields)) {
      const column = header.indexOf(field);
      if (column < 0) throw new Error(`resource fixture lacks column ${field}`);
      end[column] = String(nextValue(Number(baseline[column])));
    }
  }
  return `${[header, ...rows].map((row) => row.join(",")).join("\n")}\n`;
}

const baseVerdict = verifyWithArtifactOverrides(baseEvidence);
if (!baseVerdict.passed) {
  throw new Error(
    `positive fixture must produce a final G6 pass once every metric has a verified producer: ${baseVerdict.failure_reasons.join("; ")}`,
  );
}
if (baseVerdict.schema_version !== "ocservia.g6-verdict.v2") {
  throw new Error("positive fixture verdict must use the v2 contract");
}
for (const [name, result] of Object.entries(baseVerdict.measurement_results)) {
  if (!result.derivation) {
    throw new Error(`positive fixture metric lacks a producer: ${name}`);
  }
  if (!result.passed) {
    throw new Error(`positive fixture metric did not pass: ${name}`);
  }
}
for (const [name, result] of Object.entries(baseVerdict.observation_results)) {
  if (!result.passed) {
    throw new Error(`positive fixture observation did not pass: ${name}`);
  }
}
if (
  baseVerdict.measurement_results.database_rto_seconds.actual !== 62
) {
  throw new Error(
    "database RTO must derive from the conservative same-FD-B database clock boundary",
  );
}
if (
  baseVerdict.measurement_results.enqueue_success_ratio.actual !== 1 ||
  baseVerdict.measurement_results.enqueue_success_ratio.sample_count !== 600
) {
  throw new Error(
    "the stale-revision retry must remain one successful logical enqueue sample",
  );
}

function expectInlineRejection(label, evidence, overrides, expected) {
  let rejected;
  try {
    verifyWithArtifactOverrides(evidence, overrides);
  } catch (error) {
    rejected = String(error?.message ?? error);
  }
  if (!rejected?.includes(expected)) {
    throw new Error(
      `${label}: expected rejection containing ${expected}; got ${rejected ?? "not rejected"}`,
    );
  }
}

function expectHttpRetryRejection(label, mutateRows, expected) {
  const lines = read("testdata/g6/artifacts/http-samples.csv")
    .trimEnd()
    .split("\n");
  const header = lines[0].split(",");
  const rows = lines.slice(1).map((line) => line.split(","));
  const column = (name) => {
    const index = header.indexOf(name);
    if (index < 0) throw new Error(`HTTP fixture lacks ${name}`);
    return index;
  };
  const retryRows = rows.filter(
    (row) => row[column("idempotency_key")] === "idem-http-001",
  );
  if (retryRows.length !== 2) {
    throw new Error("positive HTTP fixture lacks the stale-revision retry pair");
  }
  mutateRows({ rows, retryRows, column });
  const override = `${[header, ...rows]
    .map((row) => row.join(","))
    .join("\n")}\n`;
  const evidence = clone(baseEvidence);
  rebindOverriddenArtifacts(evidence, { "http-samples.csv": override });
  expectInlineRejection(
    label,
    evidence,
    { "http-samples.csv": override },
    expected,
  );
}

function expectAuditRequestIdRejection(label, mutateCorrelation, expected) {
  const correlation = parse("testdata/g6/artifacts/audit-correlation.json");
  mutateCorrelation(correlation);
  const override = serialize(correlation);
  const evidence = clone(baseEvidence);
  rebindOverriddenArtifacts(evidence, { "audit-correlation.json": override });
  expectInlineRejection(
    label,
    evidence,
    { "audit-correlation.json": override },
    expected,
  );
}

expectHttpRetryRejection(
  "stale revision retry with wrong problem type",
  ({ retryRows, column }) => {
    retryRows[0][column("problem_type")] =
      "https://ocservia.dev/problems/conflict";
  },
  "retries an outcome other than the known pre-mutation stale-revision conflict",
);
expectHttpRetryRejection(
  "stale revision retry with wrong problem detail",
  ({ retryRows, column }) => {
    retryRows[0][column("problem_detail")] = "some other conflict";
  },
  "retries an outcome other than the known pre-mutation stale-revision conflict",
);
expectHttpRetryRejection(
  "stale revision retry without revision refresh",
  ({ retryRows, column }) => {
    retryRows[1][column("requested_revision")] =
      retryRows[0][column("requested_revision")];
  },
  "did not advance the stale requested revision",
);
expectHttpRetryRejection(
  "stale revision retry with a regressed revision",
  ({ retryRows, column }) => {
    retryRows[1][column("requested_revision")] = "9";
  },
  "did not advance the stale requested revision",
);
expectHttpRetryRejection(
  "stale revision retry before the conflict response completed",
  ({ retryRows, column }) => {
    retryRows[1][column("timestamp")] = retryRows[0][column("timestamp")];
  },
  "begins before the stale 409 response completed",
);
expectHttpRetryRejection(
  "stale revision retry with changed idempotency key",
  ({ retryRows, column }) => {
    retryRows[1][column("idempotency_key")] = "idem-http-changed";
  },
  "request_id does not bind its idempotency key and ordinal",
);
expectHttpRetryRejection(
  "arbitrarily rebound enqueue attempt request identity",
  ({ retryRows, column }) => {
    retryRows[1][column("request_id")] = "arbitrary-attempt-id";
  },
  "request_id does not bind its idempotency key and ordinal",
);
expectHttpRetryRejection(
  "stale revision retry with duplicate ordinal",
  ({ retryRows, column }) => {
    retryRows[1][column("attempt_ordinal")] = "1";
    retryRows[1][column("request_id")] = "idem-http-001.attempt-1";
  },
  "repeats enqueue request_id",
);
expectHttpRetryRejection(
  "stale revision retry with skipped ordinal",
  ({ retryRows, column }) => {
    retryRows[1][column("attempt_ordinal")] = "3";
    retryRows[1][column("request_id")] = "idem-http-001.attempt-3";
  },
  "enqueue attempt ordinals must be contiguous",
);
expectHttpRetryRejection(
  "stale revision retry beyond attempt limit",
  ({ retryRows, column }) => {
    retryRows[1][column("attempt_ordinal")] = "4";
    retryRows[1][column("request_id")] = "idem-http-001.attempt-4";
  },
  "attempt_ordinal exceeds its bounded attempt limit",
);
expectHttpRetryRejection(
  "retry after non-conflict HTTP status",
  ({ retryRows, column }) => {
    retryRows[0][column("http_status")] = "503";
  },
  "retries an outcome other than the known pre-mutation stale-revision conflict",
);
expectHttpRetryRejection(
  "duplicate enqueue attempt request identity",
  ({ retryRows, column }) => {
    retryRows[1][column("request_id")] = retryRows[0][column("request_id")];
  },
  "repeats enqueue request_id",
);
expectHttpRetryRejection(
  "incomplete stale revision retry chain",
  ({ rows, retryRows }) => {
    rows.splice(rows.indexOf(retryRows[1]), 1);
  },
  "ends with an incomplete stale-revision retry chain",
);
expectHttpRetryRejection(
  "inconsistent configured retry bound",
  ({ retryRows, column }) => {
    retryRows[1][column("attempt_limit")] = "2";
  },
  "attempt_limit must equal the formal three-attempt bound",
);

expectAuditRequestIdRejection(
  "audit intent with a missing request identity",
  (correlation) => {
    delete correlation.writes[0].intent_request_id;
  },
  "is missing fields: intent_request_id",
);
expectAuditRequestIdRejection(
  "audit intent with an arbitrary request identity",
  (correlation) => {
    correlation.writes[0].intent_request_id = "wrong-request-id";
  },
  "audit intent for accepted enqueue cmd-001 does not retain the terminal HTTP request_id",
);
expectAuditRequestIdRejection(
  "audit result rebound to another accepted request identity",
  (correlation) => {
    correlation.writes[0].result_request_id =
      correlation.writes[1].result_request_id;
  },
  "audit result for accepted enqueue cmd-001 does not retain the terminal HTTP request_id",
);

const delayedRetryLines = read("testdata/g6/artifacts/http-samples.csv")
  .trimEnd()
  .split("\n");
const delayedRetryHeader = delayedRetryLines[0].split(",");
const delayedRetryRows = delayedRetryLines
  .slice(1)
  .map((line) => line.split(","));
const delayedRetryColumn = (name) => delayedRetryHeader.indexOf(name);
delayedRetryRows.find(
  (row) =>
    row[delayedRetryColumn("idempotency_key")] === "idem-http-001" &&
    row[delayedRetryColumn("attempt_ordinal")] === "2",
)[delayedRetryColumn("timestamp")] = "2026-08-14T00:00:10Z";
const delayedRetryOverride = `${[delayedRetryHeader, ...delayedRetryRows]
  .map((row) => row.join(","))
  .join("\n")}\n`;
const delayedRetryEvidence = clone(baseEvidence);
rebindOverriddenArtifacts(delayedRetryEvidence, {
  "http-samples.csv": delayedRetryOverride,
});
const delayedRetryVerdict = verifyWithArtifactOverrides(delayedRetryEvidence, {
  "http-samples.csv": delayedRetryOverride,
});
if (
  !delayedRetryVerdict.passed ||
  delayedRetryVerdict.measurement_results.enqueue_latency_seconds_p95.actual !==
    baseVerdict.measurement_results.enqueue_latency_seconds_p95.actual
) {
  throw new Error(
    "client-side revision refresh time contaminated the terminal accepted POST latency SLO",
  );
}

const mismatchedHttpCommandTrace = read(
  "testdata/g6/artifacts/command-trace.jsonl",
)
  .trimEnd()
  .split("\n")
  .map(JSON.parse);
mismatchedHttpCommandTrace.find(
  (record) =>
    record.record_type === "enqueued" && record.command_id === "cmd-001",
).idempotency_key = "idem-http-mismatched";
const mismatchedHttpCommandTraceOverride = `${mismatchedHttpCommandTrace
  .map(JSON.stringify)
  .join("\n")}\n`;
const mismatchedHttpCommandEvidence = clone(baseEvidence);
rebindOverriddenArtifacts(mismatchedHttpCommandEvidence, {
  "command-trace.jsonl": mismatchedHttpCommandTraceOverride,
});
expectInlineRejection(
  "successful retry command idempotency binding",
  mismatchedHttpCommandEvidence,
  { "command-trace.jsonl": mismatchedHttpCommandTraceOverride },
  "does not retain the HTTP idempotency key",
);

function expectPostgresRecoveryRejection(label, mutateRecovery, expected) {
  const recovery = parse("testdata/g6/artifacts/postgres-recovery.json");
  mutateRecovery(recovery);
  const override = serialize(recovery);
  const evidence = clone(baseEvidence);
  rebindOverriddenArtifacts(evidence, { "postgres-recovery.json": override });
  expectInlineRejection(
    label,
    evidence,
    { "postgres-recovery.json": override },
    expected,
  );
}

expectPostgresRecoveryRejection(
  "service restoration rebound away from promotion",
  (recovery) => {
    recovery.service_restored_at = "2026-08-14T00:01:59.999999Z";
  },
  "service restoration must exactly match the promoted primary boundary",
);
expectPostgresRecoveryRejection(
  "nonpositive same-clock database RTO",
  (recovery) => {
    recovery.rto_started_at = recovery.service_restored_at;
  },
  "must restore service after its same-clock RTO boundary",
);

function expectRelayArtifactRejection(label, mutateRecords, expected) {
  const records = read("testdata/g6/artifacts/relay-transitions.jsonl")
    .trimEnd()
    .split("\n")
    .map(JSON.parse);
  mutateRecords(records);
  records.forEach((record, index) => {
    record.sequence = index + 1;
  });
  const override = `${records.map(JSON.stringify).join("\n")}\n`;
  const evidence = clone(baseEvidence);
  rebindOverriddenArtifacts(evidence, { "relay-transitions.jsonl": override });
  expectInlineRejection(
    label,
    evidence,
    { "relay-transitions.jsonl": override },
    expected,
  );
}

expectRelayArtifactRejection(
  "missing live pre-fault relay session",
  (records) => {
    const index = records.findIndex(
      (record) =>
        record.event_type === "path_active" && record.relay === "relay-a",
    );
    records.splice(index, 1);
  },
  "lacks a live pre-fault session through relay-a",
);
expectRelayArtifactRejection(
  "missing controlled relay topology proof",
  (records) => {
    delete records.find(
      (record) =>
        record.event_type === "path_active" && record.relay === "relay-a",
    ).topology_mode;
  },
  "entry is missing fields: topology_mode",
);
expectRelayArtifactRejection(
  "substituted controlled relay topology proof",
  (records) => {
    records.find(
      (record) =>
        record.event_type === "path_active" && record.relay === "relay-a",
    ).topology_mode = "default-network";
  },
  "relay-a traffic lacks the controlled topology proof",
);
expectRelayArtifactRejection(
  "non-internal controlled relay topology",
  (records) => {
    records.find(
      (record) =>
        record.event_type === "path_active" && record.relay === "relay-a",
    ).topology_network_internal = false;
  },
  "relay-a controlled topology attributes are invalid",
);
expectRelayArtifactRejection(
  "relay-b disabled after the relay-a proof",
  (records) => {
    const predecessor = records.find(
      (record) =>
        record.event_type === "path_active" && record.relay === "relay-a",
    );
    predecessor.relay_b_disabled_at = predecessor.timestamp;
  },
  "relay-a controlled topology boundaries are invalid",
);
expectRelayArtifactRejection(
  "missing cut-after relay-b start proof",
  (records) => {
    delete records.find(
      (record) =>
        record.event_type === "path_active" && record.relay === "relay-b",
    ).relay_b_started_at;
  },
  "entry is missing fields: relay_b_started_at",
);
expectRelayArtifactRejection(
  "relay-b started before the relay fault cut",
  (records) => {
    const failure = records.find(
      (record) => record.event_type === "relay_failed",
    );
    records.find(
      (record) =>
        record.event_type === "path_active" && record.relay === "relay-b",
    ).relay_b_started_at = failure.fault_cut_at;
  },
  "must record authenticated traffic through a replacement relay",
);
expectRelayArtifactRejection(
  "wrong relay command binding",
  (records) => {
    records.find(
      (record) =>
        record.event_type === "path_active" && record.relay === "relay-b",
    ).command_id = "cmd-001";
  },
  "relay authenticated traffic is not exactly bound to its successful command and durable effect",
);
expectRelayArtifactRejection(
  "wrong authenticated relay path",
  (records) => {
    records.find(
      (record) =>
        record.event_type === "path_active" && record.relay === "relay-b",
    ).path_detail = "iroh/relay-a";
  },
  "relay path_detail does not identify relay-b",
);
expectRelayArtifactRejection(
  "wrong relay session fence",
  (records) => {
    records.find(
      (record) =>
        record.event_type === "path_active" && record.relay === "relay-b",
    ).connection_id = "f".repeat(32);
  },
  "relay traffic has no causal durable owner registration",
);
expectRelayArtifactRejection(
  "nonpositive authoritative relay clock boundary",
  (records) => {
    const failure = records.find((record) => record.event_type === "relay_failed");
    records.find(
      (record) =>
        record.event_type === "path_active" && record.relay === "relay-b",
    ).timestamp = failure.timestamp;
  },
  "relay-b traffic does not follow its bounded start",
);
expectRelayArtifactRejection(
  "expired relay authority at fault cut",
  (records) => {
    const failure = records.find(
      (record) => record.event_type === "relay_failed",
    );
    failure.authority_lease_until = failure.fault_cut_at;
  },
  "failed relay proof or authority was not live at its boundary",
);
expectRelayArtifactRejection(
  "expired pre-fault relay session proof",
  (records) => {
    const failure = records.find(
      (record) => record.event_type === "relay_failed",
    );
    failure.owner_lease_until = failure.timestamp;
  },
  "failed relay proof or authority was not live at its boundary",
);
expectRelayArtifactRejection(
  "pre-fault command wrote through the wrong relay",
  (records) => {
    records.find(
      (record) =>
        record.event_type === "path_active" && record.relay === "relay-a",
    ).path_detail = "iroh/relay-b";
  },
  "relay path_detail does not identify relay-a",
);

const preFaultRelayTrace = read("testdata/g6/artifacts/command-trace.jsonl")
  .trimEnd()
  .split("\n")
  .map(JSON.parse);
preFaultRelayTrace.find(
  (record) =>
    record.record_type === "enqueued" && record.command_id === "cmd-relay-proof",
).timestamp = "2026-08-14T00:02:39.000000Z";
preFaultRelayTrace.find(
  (record) =>
    record.record_type === "dispatched" && record.command_id === "cmd-relay-proof",
).timestamp = "2026-08-14T00:02:39.500000Z";
const preFaultRelayTraceOverride = `${preFaultRelayTrace
  .map(JSON.stringify)
  .join("\n")}\n`;
const preFaultRelayTraceEvidence = clone(baseEvidence);
rebindOverriddenArtifacts(preFaultRelayTraceEvidence, {
  "command-trace.jsonl": preFaultRelayTraceOverride,
});
expectInlineRejection(
  "pre-fault cached command cannot prove relay recovery",
  preFaultRelayTraceEvidence,
  { "command-trace.jsonl": preFaultRelayTraceOverride },
  "relay authenticated traffic is not exactly bound to its successful command and durable effect",
);

// Relay RTO is derived from the promoted database's raw clock boundaries, not
// cross-runner timeline labels. Collapsing those labels to one instant must not
// change the five-second artifact-derived result.
const skewedRelayTimeline = read("testdata/g6/artifacts/timeline.jsonl")
  .trimEnd()
  .split("\n")
  .map(JSON.parse);
for (const event of skewedRelayTimeline.filter((record) =>
  ["relay_a_failed", "relay_b_active"].includes(record.event_id),
)) {
  event.timestamp = "2026-08-14T00:00:24.500000Z";
}
const skewedRelayTimelineOverride = `${skewedRelayTimeline
  .map(JSON.stringify)
  .join("\n")}\n`;
const skewedRelayTimelineEvidence = clone(baseEvidence);
rebindOverriddenArtifacts(skewedRelayTimelineEvidence, {
  "timeline.jsonl": skewedRelayTimelineOverride,
});
const skewedRelayVerdict = verifyWithArtifactOverrides(
  skewedRelayTimelineEvidence,
  { "timeline.jsonl": skewedRelayTimelineOverride },
);
if (
  !skewedRelayVerdict.passed ||
  skewedRelayVerdict.measurement_results.relay_takeover_seconds.actual !== 5
) {
  throw new Error("relay takeover trusted cross-runner timeline clock labels");
}

function expectInflightSnapshotRejection(label, mutateTrace, expected) {
  const records = read("testdata/g6/artifacts/command-trace.jsonl")
    .trimEnd()
    .split("\n")
    .map(JSON.parse);
  mutateTrace(records);
  const override = `${records.map(JSON.stringify).join("\n")}\n`;
  const evidence = clone(baseEvidence);
  rebindOverriddenArtifacts(evidence, { "command-trace.jsonl": override });
  expectInlineRejection(
    label,
    evidence,
    { "command-trace.jsonl": override },
    expected,
  );
}

expectInflightSnapshotRejection(
  "missing all-fleet inflight snapshot",
  (records) => {
    const index = records.findIndex(
      (record) => record.record_type === "inflight_snapshot",
    );
    records.splice(index, 1);
    records.forEach((record, recordIndex) => {
      record.sequence = recordIndex + 1;
    });
  },
  "must contain one inflight snapshot",
);
expectInflightSnapshotRejection(
  "tampered all-fleet inflight snapshot",
  (records) => {
    const snapshot = records.find(
      (record) => record.record_type === "inflight_snapshot",
    );
    snapshot.commands[0].node_id = snapshot.commands[1].node_id;
  },
  "repeats a command or managed node",
);
expectInflightSnapshotRejection(
  "terminal result tied with inflight snapshot",
  (records) => {
    const snapshot = records.find(
      (record) => record.record_type === "inflight_snapshot",
    );
    const result = records.find(
      (record) =>
        record.record_type === "result" &&
        record.command_id === snapshot.commands[0].command_id,
    );
    result.timestamp = snapshot.timestamp;
  },
  "is not result-free at its boundary",
);

// Evidence-window containment is an exact timestamp boundary. Millisecond
// Date parsing must not admit records one microsecond outside the signed run.
const beforeWindowTimeline = modernReconnectFixture.artifacts["timeline.jsonl"]
  .trimEnd()
  .split("\n")
  .map(JSON.parse);
beforeWindowTimeline[0].timestamp = "2026-08-13T23:59:59.999999Z";
const beforeWindowOverride = `${beforeWindowTimeline.map(JSON.stringify).join("\n")}\n`;
const beforeWindowEvidence = clone(baseEvidence);
rebindOverriddenArtifacts(beforeWindowEvidence, {
  "timeline.jsonl": beforeWindowOverride,
});
expectInlineRejection(
  "structured timestamp one microsecond before the evidence window",
  beforeWindowEvidence,
  { "timeline.jsonl": beforeWindowOverride },
  "timestamp precedes the evidence window",
);

const afterWindowTimeline = modernReconnectFixture.artifacts["timeline.jsonl"]
  .trimEnd()
  .split("\n")
  .map(JSON.parse);
afterWindowTimeline.at(-1).timestamp = "2026-08-14T00:05:30.000001Z";
const afterWindowOverride = `${afterWindowTimeline.map(JSON.stringify).join("\n")}\n`;
const afterWindowEvidence = clone(baseEvidence);
rebindOverriddenArtifacts(afterWindowEvidence, {
  "timeline.jsonl": afterWindowOverride,
});
expectInlineRejection(
  "structured timestamp one microsecond after the evidence window",
  afterWindowEvidence,
  { "timeline.jsonl": afterWindowOverride },
  "timestamp escapes the evidence window",
);

const reversedWindowEvidence = clone(baseEvidence);
reversedWindowEvidence.started_at = "2026-08-14T00:00:00.000002Z";
reversedWindowEvidence.finished_at = "2026-08-14T00:00:00.000001Z";
expectInlineRejection(
  "same-millisecond reversed evidence window",
  reversedWindowEvidence,
  {},
  "evidence finished_at must be later than started_at",
);

function expectSchedulerEpochRejection(label, mutateEvents, expected) {
  const events = modernReconnectFixture.epochEvents.map(clone);
  mutateEvents(events);
  events.forEach((event, index) => {
    event.sequence = index + 1;
  });
  const override = `${events.map((event) => JSON.stringify(event)).join("\n")}\n`;
  const evidence = clone(baseEvidence);
  rebindOverriddenArtifacts(evidence, { "epoch-events.jsonl": override });
  let rejected;
  try {
    verifyWithArtifactOverrides(evidence, { "epoch-events.jsonl": override });
  } catch (error) {
    rejected = String(error?.message ?? error);
  }
  if (!rejected?.includes(expected)) {
    throw new Error(
      `${label} scheduler evidence was not rejected as expected: ${rejected ?? "not rejected"}`,
    );
  }
}

expectSchedulerEpochRejection(
  "mismatched exact term",
  (events) => {
    const commit = events.find(
      (event) =>
        event.subject === "scheduler" &&
        event.event_type === "leader_commit" &&
        event.epoch === 2 &&
        event.accepted,
    );
    commit.incarnation = "1800000000000000001";
  },
  "scheduler commit does not match its exact acquired term",
);
expectSchedulerEpochRejection(
  "duplicate maintenance id",
  (events) => {
    const commits = events.filter(
      (event) =>
        event.subject === "scheduler" &&
        event.event_type === "leader_commit" &&
        event.accepted,
    );
    commits[1].maintenance_id = commits[0].maintenance_id;
  },
  "scheduler maintenance ids must strictly increase",
);
expectSchedulerEpochRejection(
  "non-positive maintenance incarnation",
  (events) => {
    const commit = events.find(
      (event) =>
        event.subject === "scheduler" &&
        event.event_type === "leader_commit" &&
        event.accepted,
    );
    commit.incarnation = "0";
  },
  "epoch event scheduler incarnation must be a canonical positive decimal string",
);
expectSchedulerEpochRejection(
  "non-positive maintenance id",
  (events) => {
    const commit = events.find(
      (event) =>
        event.subject === "scheduler" &&
        event.event_type === "leader_commit" &&
        event.accepted,
    );
    commit.maintenance_id = "0";
  },
  "epoch event scheduler maintenance_id must be a canonical positive decimal string",
);
expectSchedulerEpochRejection(
  "accepted commit without maintenance id",
  (events) => {
    const commit = events.find(
      (event) =>
        event.subject === "scheduler" &&
        event.event_type === "leader_commit" &&
        event.accepted,
    );
    delete commit.maintenance_id;
  },
  "is missing fields: maintenance_id",
);
expectSchedulerEpochRejection(
  "accepted commit without marker completion",
  (events) => {
    const commit = events.find(
      (event) =>
        event.subject === "scheduler" &&
        event.event_type === "leader_commit" &&
        event.accepted,
    );
    delete commit.marker_completed_at;
  },
  "is missing fields: marker_completed_at",
);
expectSchedulerEpochRejection(
  "maintenance marker after committed observation",
  (events) => {
    const commit = events.find(
      (event) =>
        event.subject === "scheduler" &&
        event.event_type === "leader_commit" &&
        event.epoch === 2 &&
        event.accepted,
    );
    commit.marker_completed_at = "2026-08-14T00:01:25.000002Z";
  },
  "scheduler maintenance marker completed after its committed observation",
);
expectSchedulerEpochRejection(
  "missing replacement maintenance",
  (events) => {
    const index = events.findIndex(
      (event) =>
        event.subject === "scheduler" &&
        event.event_type === "leader_commit" &&
        event.epoch === 2 &&
        event.accepted,
    );
    events.splice(index, 1);
  },
  "leadership epoch 2 has no exact-term maintenance completion",
);
expectSchedulerEpochRejection(
  "live completion after final cut",
  (events) => {
    const commit = events.find(
      (event) =>
        event.subject === "scheduler" &&
        event.event_type === "leader_commit" &&
        event.epoch === 2 &&
        event.accepted,
    );
    commit.timestamp = "2026-08-14T00:05:11Z";
    events.sort((left, right) =>
      left.timestamp.localeCompare(right.timestamp),
    );
  },
  "authority cut live scheduler term has no exact-term maintenance completion",
);
expectSchedulerEpochRejection(
  "rejected commit with forged maintenance id",
  (events) => {
    const rejected = events.find(
      (event) =>
        event.subject === "scheduler" &&
        event.event_type === "leader_commit" &&
        !event.accepted,
    );
    rejected.maintenance_id = "3";
  },
  "has unsupported fields: maintenance_id",
);

const mismatchedSchedulerCut = JSON.parse(
  modernReconnectFixture.artifacts["authority-cut.json"],
);
mismatchedSchedulerCut.scheduler.maintenance_id = "999";
const mismatchedSchedulerCutOverride = serialize(mismatchedSchedulerCut);
const mismatchedSchedulerCutEvidence = clone(baseEvidence);
rebindOverriddenArtifacts(mismatchedSchedulerCutEvidence, {
  "authority-cut.json": mismatchedSchedulerCutOverride,
});
let mismatchedSchedulerCutRejected;
try {
  verifyWithArtifactOverrides(mismatchedSchedulerCutEvidence, {
    "authority-cut.json": mismatchedSchedulerCutOverride,
  });
} catch (error) {
  mismatchedSchedulerCutRejected = String(error?.message ?? error);
}
if (
  !mismatchedSchedulerCutRejected?.includes(
    "authority cut live scheduler term has no exact-term maintenance completion",
  )
) {
  throw new Error(
    `a mismatched live scheduler maintenance id bypassed the authority cut: ${mismatchedSchedulerCutRejected ?? "not rejected"}`,
  );
}

const mismatchedSchedulerTimestampCut = JSON.parse(
  modernReconnectFixture.artifacts["authority-cut.json"],
);
mismatchedSchedulerTimestampCut.scheduler.maintenance_completed_at =
  "2026-08-14T00:01:24.999999Z";
const mismatchedSchedulerTimestampCutOverride = serialize(
  mismatchedSchedulerTimestampCut,
);
const mismatchedSchedulerTimestampCutEvidence = clone(baseEvidence);
rebindOverriddenArtifacts(mismatchedSchedulerTimestampCutEvidence, {
  "authority-cut.json": mismatchedSchedulerTimestampCutOverride,
});
expectInlineRejection(
  "mismatched live scheduler marker timestamp",
  mismatchedSchedulerTimestampCutEvidence,
  { "authority-cut.json": mismatchedSchedulerTimestampCutOverride },
  "authority cut live scheduler term has no exact-term maintenance completion",
);

const postCutSchedulerEvents = modernReconnectFixture.epochEvents.map(clone);
const postCutSchedulerCommit = postCutSchedulerEvents.find(
  (event) =>
    event.subject === "scheduler" &&
    event.event_type === "leader_commit" &&
    event.epoch === 2 &&
    event.accepted,
);
postCutSchedulerCommit.timestamp = "2026-08-14T00:05:10.200000001Z";
postCutSchedulerCommit.marker_completed_at =
  "2026-08-14T00:05:10.200000001Z";
postCutSchedulerEvents.sort((left, right) =>
  left.timestamp.localeCompare(right.timestamp),
);
postCutSchedulerEvents.forEach((event, index) => {
  event.sequence = index + 1;
});
const postCutSchedulerAuthority = JSON.parse(
  modernReconnectFixture.artifacts["authority-cut.json"],
);
postCutSchedulerAuthority.scheduler.maintenance_completed_at =
  postCutSchedulerCommit.marker_completed_at;
const postCutSchedulerOverrides = {
  "epoch-events.jsonl": `${postCutSchedulerEvents.map(JSON.stringify).join("\n")}\n`,
  "authority-cut.json": serialize(postCutSchedulerAuthority),
};
const postCutSchedulerEvidence = clone(baseEvidence);
rebindOverriddenArtifacts(postCutSchedulerEvidence, postCutSchedulerOverrides);
expectInlineRejection(
  "scheduler maintenance one nanosecond after the authority cut",
  postCutSchedulerEvidence,
  postCutSchedulerOverrides,
  "scheduler maintenance completed after the cut",
);

const decreasingGrowthMetrics = [
  "transportd_rss_growth_ratio",
  "transportd_fd_growth",
  "transportd_tokio_task_growth",
  "agent_rss_growth_ratio",
  "agent_fd_growth",
  "agent_tokio_task_growth",
  "database_connection_growth",
];
for (const metric of decreasingGrowthMetrics) {
  if (
    baseEvidence.measurements[metric].actual <= 0 ||
    baseVerdict.measurement_results[metric].actual !==
      baseEvidence.measurements[metric].actual
  ) {
    throw new Error(`positive resource growth changed unexpectedly for ${metric}`);
  }
}

const decreasingResourceSamples = resourceSamplesWithFinalValues({
  transportd: {
    rss_bytes: (baseline) => baseline - 1,
    fd_count: (baseline) => baseline - 1,
    tasks: (baseline) => baseline - 1,
  },
  agent: {
    rss_bytes: (baseline) => baseline - 1,
    fd_count: (baseline) => baseline - 1,
    tasks: (baseline) => baseline - 1,
  },
  postgres: {
    db_connections: (baseline) => baseline - 1,
  },
});
const decreasingEvidence = clone(baseEvidence);
for (const metric of decreasingGrowthMetrics) {
  decreasingEvidence.measurements[metric].actual = 0;
}
rebindOverriddenArtifacts(decreasingEvidence, {
  "resource-samples.csv": decreasingResourceSamples,
});
const decreasingVerdict = verifyWithArtifactOverrides(decreasingEvidence, {
  "resource-samples.csv": decreasingResourceSamples,
});
if (!decreasingVerdict.passed) {
  throw new Error(
    `decreasing resource counters must clamp to zero growth: ${decreasingVerdict.failure_reasons.join("; ")}`,
  );
}
for (const metric of decreasingGrowthMetrics) {
  if (decreasingVerdict.measurement_results[metric].actual !== 0) {
    throw new Error(`decreasing resource counter did not clamp to zero: ${metric}`);
  }
}

const overLimitResourceSamples = resourceSamplesWithFinalValues({
  transportd: { rss_bytes: (baseline) => baseline * 2 },
});
const overLimitEvidence = clone(baseEvidence);
overLimitEvidence.measurements.transportd_rss_growth_ratio.actual = 1;
rebindOverriddenArtifacts(overLimitEvidence, {
  "resource-samples.csv": overLimitResourceSamples,
});
const overLimitVerdict = verifyWithArtifactOverrides(overLimitEvidence, {
  "resource-samples.csv": overLimitResourceSamples,
});
if (
  overLimitVerdict.passed ||
  overLimitVerdict.measurement_results.transportd_rss_growth_ratio.actual !== 1 ||
  overLimitVerdict.measurement_results.transportd_rss_growth_ratio.passed
) {
  throw new Error("positive resource growth above the SLO must still fail");
}

const negativeMeasurementEvidence = clone(baseEvidence);
negativeMeasurementEvidence.measurements.transportd_rss_growth_ratio.actual = -1;
let negativeMeasurementRejected = false;
try {
  verifyWithArtifactOverrides(negativeMeasurementEvidence);
} catch (error) {
  negativeMeasurementRejected = String(error?.message ?? error).includes(
    "must be a non-negative finite number",
  );
}
if (!negativeMeasurementRejected) {
  throw new Error("negative evidence measurements must remain invalid");
}

const completeResourceLines = read("testdata/g6/artifacts/resource-samples.csv")
  .trimEnd()
  .split("\n");
const finalResourceTimestamp = completeResourceLines.at(-1).split(",", 1)[0];
const sparseFinalTick = `${completeResourceLines
  .filter((_, index) => index !== completeResourceLines.length - 1)
  .join("\n")}\n`;
const sparseFinalTickEvidence = clone(baseEvidence);
rebindOverriddenArtifacts(sparseFinalTickEvidence, {
  "resource-samples.csv": sparseFinalTick,
});
expectInlineRejection(
  "hung sampler with a sparse final tick",
  sparseFinalTickEvidence,
  { "resource-samples.csv": sparseFinalTick },
  "has an incomplete sampler tick",
);

const staleFinalTick = `${completeResourceLines
  .filter(
    (line, index) =>
      index === 0 || !line.startsWith(`${finalResourceTimestamp},`),
  )
  .join("\n")}\n`;
const staleFinalTickEvidence = clone(baseEvidence);
rebindOverriddenArtifacts(staleFinalTickEvidence, {
  "resource-samples.csv": staleFinalTick,
});
expectInlineRejection(
  "sampler stopped without a fresh final complete tick",
  staleFinalTickEvidence,
  { "resource-samples.csv": staleFinalTick },
  "must end within five seconds of the graceful sampler stop boundary",
);

function expectReconnectSessionRejection(label, mutateSession, expectedError) {
  const sessions = JSON.parse(
    modernReconnectFixture.artifacts["agent-sessions.json"],
  );
  mutateSession(sessions.sessions[0], sessions);
  const override = serialize(sessions);
  const evidence = clone(baseEvidence);
  rebindOverriddenArtifacts(evidence, { "agent-sessions.json": override });
  let rejected;
  try {
    verifyWithArtifactOverrides(evidence, { "agent-sessions.json": override });
  } catch (error) {
    rejected = String(error?.message ?? error);
  }
  if (!rejected?.includes(expectedError)) {
    throw new Error(
      `${label}: expected rejection containing ${expectedError}; got ${rejected ?? "no rejection"}`,
    );
  }
}

expectReconnectSessionRejection(
  "transport reconnect before durable registration",
  (session) => {
    session.reconnected_at = "2026-08-14T00:03:20.000001Z";
  },
  "transport reconnect predates the durable owner registration",
);
expectReconnectSessionRejection(
  "reconnect at bulk-disconnect boundary",
  (session) => {
    session.reconnected_at = JSON.parse(
      modernReconnectFixture.artifacts["agent-sessions.json"],
    ).reconnect_storm.bulk_disconnect_at;
  },
  "must reconnect after the bulk disconnect",
);
expectReconnectSessionRejection(
  "transport reconnect beyond the 120-second storm bound",
  (session) => {
    session.reconnected_at = "2026-08-14T00:05:00.000001Z";
  },
  "transport reconnect falls outside the timeline storm",
);
expectReconnectSessionRejection(
  "reconnect registration tuple mismatch",
  (_session, sessions) => {
    sessions.sessions[1].reconnect_connection_id = "f".repeat(32);
  },
  "reconnect tuple has no durable owner registration",
);

const incompleteTakeoverEvents = modernReconnectFixture.artifacts[
  "epoch-events.jsonl"
]
  .trimEnd()
  .split("\n")
  .map((line) => JSON.parse(line));
const missingTakeoverNode = JSON.parse(
  modernReconnectFixture.artifacts["agent-sessions.json"],
).sessions[1].node;
const missingExpiry = incompleteTakeoverEvents.find(
  (event) =>
    event.subject === "connection_owner" &&
    event.event_type === "owner_lease_expired" &&
    event.node === missingTakeoverNode,
);
if (!missingExpiry) {
  throw new Error("generated G6 fixture lacks a population-wide owner expiry");
}
missingExpiry.event_type = "owner_retired";
const incompleteTakeoverOverride = `${incompleteTakeoverEvents
  .map((event) => JSON.stringify(event))
  .join("\n")}\n`;
const incompleteTakeoverEvidence = clone(baseEvidence);
rebindOverriddenArtifacts(incompleteTakeoverEvidence, {
  "epoch-events.jsonl": incompleteTakeoverOverride,
});
let incompleteTakeoverRejected;
try {
  verifyWithArtifactOverrides(incompleteTakeoverEvidence, {
    "epoch-events.jsonl": incompleteTakeoverOverride,
  });
} catch (error) {
  incompleteTakeoverRejected = String(error?.message ?? error);
}
if (
  !incompleteTakeoverRejected?.includes(
    "has no completed lease-expiry connection-owner takeover",
  )
) {
  throw new Error(
    `a retirement-only managed-node reconnect bypassed full-population owner takeover proof: ${incompleteTakeoverRejected ?? "not rejected"}`,
  );
}

// A completed takeover elsewhere in the run cannot stand in for the formal
// connection-owner scenario. Move its opening boundary past the durable
// expiries while keeping the timeline itself ordered and fully bound.
const lateOwnerBoundaryEvents = modernReconnectFixture.artifacts[
  "timeline.jsonl"
]
  .trimEnd()
  .split("\n")
  .map((line) => JSON.parse(line));
lateOwnerBoundaryEvents.find(
  (event) => event.event_id === "owner_a_paused",
).timestamp = "2026-08-14T00:00:10.150000Z";
const lateOwnerBoundaryOverride = `${lateOwnerBoundaryEvents
  .map((event) => JSON.stringify(event))
  .join("\n")}\n`;
const lateOwnerBoundaryEvidence = clone(baseEvidence);
rebindOverriddenArtifacts(lateOwnerBoundaryEvidence, {
  "timeline.jsonl": lateOwnerBoundaryOverride,
});
let lateOwnerBoundaryRejected;
try {
  verifyWithArtifactOverrides(lateOwnerBoundaryEvidence, {
    "timeline.jsonl": lateOwnerBoundaryOverride,
  });
} catch (error) {
  lateOwnerBoundaryRejected = String(error?.message ?? error);
}
if (
  !lateOwnerBoundaryRejected?.includes(
    "has no completed lease-expiry connection-owner takeover within the owner failover timeline",
  )
) {
  throw new Error(
    `an out-of-scenario owner takeover bypassed the formal timeline boundary: ${lateOwnerBoundaryRejected ?? "not rejected"}`,
  );
}

// A telemetry sample one microsecond beyond the artifact's declared age bound
// is stale. Millisecond Date parsing would round this back onto the boundary.
const telemetryBoundary = parse("testdata/g6/artifacts/telemetry-snapshot.json");
telemetryBoundary.agents.at(-1).last_telemetry_at =
  "2026-08-14T00:03:29.999999Z";
const telemetryBoundaryOverride = serialize(telemetryBoundary);
const telemetryBoundaryEvidence = clone(baseEvidence);
telemetryBoundaryEvidence.measurements.telemetry_fresh_ratio.actual = 49 / 50;
rebindOverriddenArtifacts(telemetryBoundaryEvidence, {
  "telemetry-snapshot.json": telemetryBoundaryOverride,
});
const telemetryBoundaryVerdict = verifyWithArtifactOverrides(
  telemetryBoundaryEvidence,
  { "telemetry-snapshot.json": telemetryBoundaryOverride },
);
if (
  telemetryBoundaryVerdict.passed ||
  telemetryBoundaryVerdict.measurement_results.telemetry_fresh_ratio.passed ||
  telemetryBoundaryVerdict.measurement_results.telemetry_fresh_ratio.actual !==
    49 / 50
) {
  throw new Error(
    "telemetry freshness admitted a sample one microsecond beyond its age bound",
  );
}

// A pending row due one microsecond after the snapshot is not overdue. Keep
// the separate terminal-ratio failure visible while pinning the exact due-row
// classification to the recomputed zero count.
const outboxBoundary = parse("testdata/g6/artifacts/outbox-snapshot.json");
outboxBoundary.rows[0] = {
  ...outboxBoundary.rows[0],
  created_at: "2026-08-14T00:04:59.999999Z",
  due_at: "2026-08-14T00:05:00.000001Z",
  state: "pending",
};
const outboxBoundaryOverride = serialize(outboxBoundary);
const outboxBoundaryEvidence = clone(baseEvidence);
outboxBoundaryEvidence.measurements.terminal_or_reconciled_operation_ratio.actual =
  599 / 600;
rebindOverriddenArtifacts(outboxBoundaryEvidence, {
  "outbox-snapshot.json": outboxBoundaryOverride,
});
const outboxBoundaryVerdict = verifyWithArtifactOverrides(
  outboxBoundaryEvidence,
  { "outbox-snapshot.json": outboxBoundaryOverride },
);
if (
  outboxBoundaryVerdict.measurement_results.unreconciled_outbox_rows.actual !== 0 ||
  !outboxBoundaryVerdict.measurement_results.unreconciled_outbox_rows.passed
) {
  throw new Error("outbox due-row classification discarded sub-millisecond order");
}

// PITR restoration must not predate the post-restore-point marker, including
// when both timestamps occupy the same millisecond.
const pitrBoundary = parse("testdata/g6/artifacts/pitr-report.json");
pitrBoundary.marker_b.written_at = "2026-08-14T00:00:30.000002Z";
pitrBoundary.restore.restored_at = "2026-08-14T00:00:30.000001Z";
const pitrBoundaryOverride = serialize(pitrBoundary);
const pitrBoundaryEvidence = clone(baseEvidence);
rebindOverriddenArtifacts(pitrBoundaryEvidence, {
  "pitr-report.json": pitrBoundaryOverride,
});
let pitrBoundaryRejected;
try {
  verifyWithArtifactOverrides(pitrBoundaryEvidence, {
    "pitr-report.json": pitrBoundaryOverride,
  });
} catch (error) {
  pitrBoundaryRejected = String(error?.message ?? error);
}
if (!pitrBoundaryRejected?.includes("restore must complete after the last marker")) {
  throw new Error(
    `PITR same-millisecond reverse order was not rejected: ${pitrBoundaryRejected ?? "not rejected"}`,
  );
}

function expectDurationBoundaryFailure(label, metric, actual, overrides, sampleCount) {
  const evidence = clone(baseEvidence);
  evidence.measurements[metric].actual = actual;
  if (sampleCount !== undefined) {
    evidence.measurements[metric].sample_count = sampleCount;
  }
  if (metric === "connection_owner_takeover_seconds") {
    evidence.measurements.concurrent_active_connection_owners.sample_count += 2;
  }
  rebindOverriddenArtifacts(evidence, overrides);
  const verdict = verifyWithArtifactOverrides(evidence, overrides);
  const result = verdict.measurement_results[metric];
  if (
    verdict.passed ||
    result.passed ||
    result.actual !== actual ||
    result.limit >= actual
  ) {
    throw new Error(`${label} did not fail at the first microsecond over its limit`);
  }
}

const ownerBoundaryEvents = modernReconnectFixture.artifacts["epoch-events.jsonl"]
  .trimEnd()
  .split("\n")
  .map(JSON.parse);
const boundaryOwnerNode = "e".repeat(32);
ownerBoundaryEvents.push(
  {
    timestamp: "2026-08-14T00:00:30.000000Z",
    subject: "connection_owner",
    event_type: "owner_registered",
    node: boundaryOwnerNode,
    instance: "worker-boundary",
    incarnation: "1700000000000000098",
    connection_id: "d".repeat(32),
    epoch: 1,
    lease_until: "2026-08-14T00:00:40.000000Z",
  },
  {
    timestamp: "2026-08-14T00:00:40.000000Z",
    subject: "connection_owner",
    event_type: "owner_lease_expired",
    node: boundaryOwnerNode,
    epoch: 1,
  },
  {
    timestamp: "2026-08-14T00:01:10.000001Z",
    subject: "connection_owner",
    event_type: "owner_registered",
    node: boundaryOwnerNode,
    instance: "worker-boundary",
    incarnation: "1700000000000000098",
    connection_id: "c".repeat(32),
    epoch: 2,
    lease_until: "2026-08-14T00:02:00.000000Z",
    session_connected_at: "2026-08-14T00:01:10.000001Z",
  },
);
ownerBoundaryEvents.sort(
  (left, right) => Date.parse(left.timestamp) - Date.parse(right.timestamp),
);
ownerBoundaryEvents.forEach((event, index) => {
  event.sequence = index + 1;
  event.environment_id = "g6-12345678";
  event.candidate_sha = "1".repeat(40);
});
const ownerBoundaryOverride = `${ownerBoundaryEvents.map(JSON.stringify).join("\n")}\n`;
expectDurationBoundaryFailure(
  "connection-owner takeover precision",
  "connection_owner_takeover_seconds",
  30.000001,
  { "epoch-events.jsonl": ownerBoundaryOverride },
  baseEvidence.measurements.connection_owner_takeover_seconds.sample_count + 1,
);

const schedulerBoundaryEvents = modernReconnectFixture.artifacts[
  "epoch-events.jsonl"
]
  .trimEnd()
  .split("\n")
  .map(JSON.parse);
const schedulerBoundaryCommit = schedulerBoundaryEvents.find(
  (event) =>
    event.subject === "scheduler" &&
    event.event_type === "leader_commit" &&
    event.epoch === 2 &&
    event.accepted === true,
);
const schedulerStaleCommit = schedulerBoundaryEvents.find(
  (event) =>
    event.subject === "scheduler" &&
    event.event_type === "leader_commit" &&
    event.accepted === false,
);
if (!schedulerBoundaryCommit || !schedulerStaleCommit) {
  throw new Error("generated G6 fixture lacks scheduler maintenance boundary records");
}
schedulerBoundaryCommit.marker_completed_at =
  "2026-08-14T00:01:45.000000Z";
schedulerBoundaryCommit.timestamp = "2026-08-14T00:01:45.000001Z";
schedulerStaleCommit.timestamp = "2026-08-14T00:01:45.000002Z";
schedulerBoundaryEvents.sort(
  (left, right) => Date.parse(left.timestamp) - Date.parse(right.timestamp),
);
schedulerBoundaryEvents.forEach((event, index) => {
  event.sequence = index + 1;
});
const schedulerBoundaryOverride = `${schedulerBoundaryEvents
  .map(JSON.stringify)
  .join("\n")}\n`;
const schedulerBoundaryAuthority = JSON.parse(
  modernReconnectFixture.artifacts["authority-cut.json"],
);
schedulerBoundaryAuthority.scheduler.maintenance_completed_at =
  schedulerBoundaryCommit.marker_completed_at;
expectDurationBoundaryFailure(
  "scheduler takeover precision",
  "scheduler_takeover_seconds",
  30.000001,
  {
    "epoch-events.jsonl": schedulerBoundaryOverride,
    "authority-cut.json": serialize(schedulerBoundaryAuthority),
  },
);

const reconnectBoundaryTimeline = modernReconnectFixture.artifacts["timeline.jsonl"]
  .trimEnd()
  .split("\n")
  .map(JSON.parse);
reconnectBoundaryTimeline.find(
  (event) => event.event_id === "bulk_disconnect_injected",
).timestamp = "2026-08-14T00:02:09.099999Z";
const reconnectBoundarySessions = JSON.parse(
  modernReconnectFixture.artifacts["agent-sessions.json"],
);
reconnectBoundarySessions.reconnect_storm.bulk_disconnect_at =
  "2026-08-14T00:02:09.099999Z";
expectDurationBoundaryFailure(
  "reconnect storm precision",
  "reconnect_storm_recovery_seconds",
  120.000001,
  {
    "timeline.jsonl": `${reconnectBoundaryTimeline.map(JSON.stringify).join("\n")}\n`,
    "agent-sessions.json": serialize(reconnectBoundarySessions),
  },
);

const relayBoundaryEvents = read("testdata/g6/artifacts/relay-transitions.jsonl")
  .trimEnd()
  .split("\n")
  .map(JSON.parse);
const relayBoundarySuccess = relayBoundaryEvents.find(
  (event) =>
    event.event_type === "path_active" && event.relay === "relay-b",
);
const laterRelayEvents = relayBoundaryEvents.filter(
  (event) => event.sequence > relayBoundarySuccess.sequence,
);
relayBoundarySuccess.timestamp = "2026-08-14T00:03:10.000001Z";
laterRelayEvents.forEach((event, index) => {
  event.timestamp = `2026-08-14T00:03:${String(11 + index).padStart(2, "0")}.000000Z`;
});
const relayBoundaryOverride = `${relayBoundaryEvents.map(JSON.stringify).join("\n")}\n`;
expectDurationBoundaryFailure(
  "relay takeover precision",
  "relay_takeover_seconds",
  30.000001,
  { "relay-transitions.jsonl": relayBoundaryOverride },
);

const reversedNanosecondEvents = modernReconnectFixture.artifacts[
  "epoch-events.jsonl"
]
  .trimEnd()
  .split("\n")
  .map(JSON.parse);
reversedNanosecondEvents.at(-2).timestamp = "2026-08-14T00:04:09.000002Z";
reversedNanosecondEvents.at(-1).timestamp = "2026-08-14T00:04:09.000001Z";
const reversedNanosecondOverride = `${reversedNanosecondEvents
  .map(JSON.stringify)
  .join("\n")}\n`;
const reversedNanosecondEvidence = clone(baseEvidence);
rebindOverriddenArtifacts(reversedNanosecondEvidence, {
  "epoch-events.jsonl": reversedNanosecondOverride,
});
let reversedNanosecondRejected;
try {
  verifyWithArtifactOverrides(reversedNanosecondEvidence, {
    "epoch-events.jsonl": reversedNanosecondOverride,
  });
} catch (error) {
  reversedNanosecondRejected = String(error?.message ?? error);
}
if (!reversedNanosecondRejected?.includes("timestamps must not decrease")) {
  throw new Error(
    `same-millisecond reverse ordering was not rejected: ${reversedNanosecondRejected ?? "not rejected"}`,
  );
}

// Retirement is just as final as lease expiry for an authority term. Exercise
// a non-topology owner so this regression isolates the stale-accept metric
// instead of failing later through the final session cross-check.
const retiredNode = "f".repeat(32);
const retiredEvents = modernReconnectFixture.artifacts["epoch-events.jsonl"]
  .trimEnd()
  .split("\n")
  .map((line) => JSON.parse(line));
for (const record of [
  {
    timestamp: "2026-08-14T00:04:20Z",
    subject: "connection_owner",
    event_type: "owner_registered",
    node: retiredNode,
    instance: "worker-retired",
    incarnation: "1700000000000000099",
    connection_id: "e".repeat(32),
    epoch: 1,
    lease_until: "2026-08-14T00:05:00Z",
  },
  {
    timestamp: "2026-08-14T00:04:21Z",
    subject: "connection_owner",
    event_type: "owner_retired",
    node: retiredNode,
    epoch: 1,
  },
  {
    timestamp: "2026-08-14T00:04:22Z",
    subject: "connection_owner",
    event_type: "owner_accept",
    node: retiredNode,
    instance: "worker-retired",
    epoch: 1,
    accepted: true,
  },
]) {
  retiredEvents.push({
    sequence: retiredEvents.length + 1,
    environment_id: "g6-12345678",
    candidate_sha: "1".repeat(40),
    ...record,
  });
}
const retiredOverride = `${retiredEvents
  .map((record) => JSON.stringify(record))
  .join("\n")}\n`;
const retiredEvidence = clone(baseEvidence);
rebindOverriddenArtifacts(retiredEvidence, {
  "epoch-events.jsonl": retiredOverride,
});
let retiredRejected = false;
let retiredError;
try {
  verifyWithArtifactOverrides(retiredEvidence, {
    "epoch-events.jsonl": retiredOverride,
  });
} catch (error) {
  retiredError = String(error?.message ?? error);
  retiredRejected = String(error?.message ?? error).includes(
    "evidence measurement stale_owner_accepts does not match the artifact-derived value",
  );
}
if (!retiredRejected) {
  throw new Error(
    `a retired owner accepted work without failing the stale-authority metric: ${retiredError ?? "not rejected"}`,
  );
}

const fixtureDirectory = new URL("../testdata/g6/", import.meta.url);
const cases = readdirSync(fixtureDirectory)
  .filter((name) => name.startsWith("evidence-") && name.endsWith(".json"))
  .filter((name) => name !== "evidence-pass.json")
  .sort();

for (const name of cases) {
  const fixture = parse(`testdata/g6/${name}`);
  for (const field of Object.keys(fixture)) {
    if (!allowedFixtureFields.has(field)) {
      throw new Error(`${name}: unsupported fixture field: ${field}`);
    }
  }
  const evidence = clone(baseEvidence);
  const topology = clone(baseTopology);
  let sloText = baseSloText;
  for (const mutation of fixture.topology_mutations ?? [])
    mutate(topology, mutation);
  for (const mutation of fixture.evidence_mutations ?? [])
    mutate(evidence, mutation);
  for (const mutation of fixture.mutations ?? []) mutate(evidence, mutation);
  modernizeLegacySourceDigests(evidence);
  rebindDeclaredArtifacts(evidence, modernReconnectFixture.artifacts);
  for (const replacement of fixture.slo_replacements ?? []) {
    if (!sloText.includes(replacement.from)) {
      throw new Error(`${name}: SLO replacement source is absent`);
    }
    sloText = sloText.replace(replacement.from, replacement.to);
  }

  const topologyText = serialize(topology);
  if ((fixture.topology_mutations ?? []).length > 0) {
    evidence.topology_digest = sha256Digest(topologyText);
  }
  if ((fixture.slo_replacements ?? []).length > 0) {
    evidence.slo_contract_digest = sha256Digest(sloText);
  }

  const fixtureArtifacts = modernizeFixtureOverrides(
    name,
    fixture.artifact_files,
  );
  const artifactRoot = buildArtifactRoot(fixture.artifact_root, fixtureArtifacts);
  if (fixture.rebind_overridden_artifacts) {
    rebindOverriddenArtifacts(evidence, fixtureArtifacts);
  }
  let verdict;
  let rejected;
  try {
    verdict = verifyG6({
      sloText,
      evidenceText: serialize(evidence),
      topologyText,
      manifestText,
      artifactRoot,
      expectedAuthority: fixture.expected_authority ?? "production_readiness",
      expectedEnvironmentId: "g6-12345678",
      expectedFailureDomainClass:
        fixture.expected_failure_domain_class ?? "multi_host",
    });
  } catch (error) {
    rejected = error;
  } finally {
    if (artifactRoot.startsWith(join(tmpdir(), "g6-artifacts-"))) {
      rmSync(artifactRoot, { recursive: true, force: true });
    }
  }

  const expectedError =
    name === "evidence-topology-too-few-agents.json"
      ? "command trace inflight snapshot does not cover the exact managed session population"
      : fixture.expected_error;
  if (expectedError) {
    if (!rejected?.message.includes(expectedError)) {
      throw new Error(
        `${name}: expected rejection containing ${expectedError}; got ${rejected?.message ?? "no rejection"}`,
      );
    }
    continue;
  }
  if (rejected) throw new Error(`${name}: ${rejected.message}`);
  if (verdict.passed)
    throw new Error(`${name}: forged or failing evidence produced PASS`);
  if (!verdict.failure_reasons.includes(fixture.expected_failure_reason)) {
    throw new Error(`${name}: missing expected failure reason`);
  }
  if (fixture.expected_all_measurements_pass) {
    const failed = Object.entries(verdict.measurement_results).filter(
      ([, result]) => !result.passed,
    );
    if (failed.length > 0) {
      throw new Error(
        `${name}: expected every metric verdict to pass, failed: ${failed.map(([metric]) => metric).join(", ")}`,
      );
    }
  }
}

console.log(
  `G6 verifier awarded the positive fixture a final pass and rejected ${cases.length} file fixtures plus the retired-owner regression`,
);
