import assert from "node:assert/strict";
import { readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { verifyRelayProof } from "./g6-relay-proof.mjs";

const root = process.argv[2];
const json = (path) => JSON.parse(readFileSync(join(root, path), "utf8"));
const lines = (path) => readFileSync(join(root, path), "utf8").trim().split("\n").filter(Boolean).map(JSON.parse);
const hex = (value) => value.replaceAll("-", "").toLowerCase();
let retainedSession;
for (const [scenario, count] of [["control", 1], ["loss", 2]]) {
  const events = lines(`${scenario}/transport-events.jsonl`);
  const writes = events.filter((e) => e.fields.event_type === "command_frame_written").map((e) => e.fields);
  assert.equal(writes.length, count, "exact explained dispatch count");
  const first = writes[0];
  const proof = json(`${scenario}/existing-gate-proof.json`);
  assert.deepEqual(proof.deliveries, writes, "runtime proof retains every write");
  verifyRelayProof(proof, json(`${scenario}/session-before.json`).observations[0], json(`${scenario}/agent-proof.json`));
  for (const position of ["before", "after"]) {
    const probe = json(`${scenario}/session-${position}.json`);
    assert.equal(probe.all_matched, true);
    assert.equal(probe.observations.length, 1);
    const session = probe.observations[0];
    assert.equal(session.path, "relay");
    assert.match(session.path_detail, /relay-b/);
    const term = [session.node_id, session.endpoint_id, session.owner_fence_id,
      session.connection_id, session.owner_epoch, session.agent_instance_id];
    retainedSession ??= term;
    assert.deepEqual(term, retainedSession, "same session/owner across both cases");
    for (const write of writes) {
      assert.equal(write.node_id, hex(session.node_id));
      assert.equal(write.owner_fence_id, hex(session.owner_fence_id));
      assert.equal(write.connection_id, hex(session.connection_id));
      assert.equal(write.owner_epoch, session.owner_epoch);
      assert.equal(write.path, "relay");
      assert.match(write.path_detail, /relay-b/);
    }
  }
  assert.equal(first.delivery_mode, 1);
  assert.equal(first.required_capability, "synthetic.noop");
  assert.ok(first.expected_revision > 0 && first.sequence > 0);
  assert.match(first.semantic_payload_sha256, /^[0-9a-f]{64}$/);
  assert.equal(first.semantic_payload_hash_version, 2);
  if (scenario === "loss") {
    const second = writes[1];
    assert.notEqual(second.message_id, first.message_id);
    assert.equal(second.delivery_mode, 2, "second dispatch must be reconcile-only");
    for (const field of ["command_id", "node_id", "operation_id", "idempotency_key",
      "semantic_payload_sha256", "semantic_payload_hash_version", "expected_revision", "sequence", "required_capability"]) {
      assert.equal(second[field], first[field], `unchanged logical field ${field}`);
    }
  }
  const dropped = events.filter((e) => e.fields.event_type === "test_command_result_dropped");
  assert.equal(dropped.length, scenario === "loss" ? 1 : 0);
  if (dropped.length) assert.equal(dropped[0].fields.message_id, first.message_id);
  const responses = events.filter((e) => e.fields.event_type === "command_response_received");
  assert.equal(responses.length, count);
  for (const response of responses) assert.equal(response.fields.state, 1);
  assert.equal(responses[0].fields.replayed, false);
  if (scenario === "loss") assert.equal(responses[1].fields.replayed, true);
  const snapshots = lines(`${scenario}/database.jsonl`);
  const final = snapshots.at(-1);
  assert.equal(final.command_state, "succeeded");
  assert.equal(final.operation_state, "succeeded");
  assert.equal(final.outbox.attempts, count);
  assert.ok(final.outbox.published_at);
  assert.equal(final.outbox.locked_by, null);
  assert.equal(final.attempts.length, count);
  for (const attempt of final.attempts) assert.equal(attempt.state, "sent");
  assert.equal(final.results.length, 1);
  const result = final.results[0];
  assert.equal(result.state, "succeeded");
  assert.equal(result.replayed, scenario === "loss");
  assert.equal(result.payload_sha256, first.semantic_payload_sha256);
  assert.equal(result.idempotency_key, first.idempotency_key);
  const agentEvents = readFileSync(join(root, "agent-live.log"), "utf8").split("\n").flatMap((line) => {
    try { return [JSON.parse(line)]; } catch { return []; }
  }).filter((event) => event.fields?.command_id === first.command_id);
  const effects = agentEvents.filter((event) => event.fields.event_type === "synthetic_effect_observed").map((event) => event.fields);
  assert.equal(effects.length, count);
  for (const [index, effect] of effects.entries()) {
    assert.equal(effect.message_id, writes[index].message_id);
    assert.equal(effect.effect_present, true);
    assert.equal(effect.journal_total_executions, scenario === "control" ? 1 : 2);
    assert.equal(effect.result_sha256, result.result_sha256);
    assert.equal(effect.effect_result_sha256, result.result_sha256);
    assert.equal(effect.effect_payload_sha256, first.semantic_payload_sha256);
    assert.equal(effect.effect_executed_at, effects[0].effect_executed_at);
  }
  const journal = lines(`${scenario}/journal.jsonl`);
  assert.equal(journal.length, 1);
  assert.equal(hex(journal[0].command_id), first.command_id);
  assert.equal(journal[0].state, "succeeded");
  assert.ok(journal[0].effect_executed_at);
  assert.equal(journal[0].total_executions, scenario === "control" ? 1 : 2);
  if (scenario === "loss") {
    assert.ok(final.operation_events.some((e) => e.state === "unknown"));
    const worker = readFileSync(join(root, "worker-live.log"), "utf8");
    const recovery = worker.split("\n").flatMap((line) => {
      try { return [JSON.parse(line.slice(line.indexOf("{")))]; } catch { return []; }
    }).filter((event) => event.event_type === "command_reconciliation_prepared" && event.command_id === final.command_id);
    assert.equal(recovery.length, 1, "one actual recovery reason");
    assert.equal(recovery[0].reason, "sent_result_timeout");
    assert.equal(recovery[0].prior_message_id, first.message_id);
    assert.equal(recovery[0].message_id, writes[1].message_id);
    assert.equal(recovery[0].prior_attempt, 1);
    assert.equal(recovery[0].outbox_id, final.outbox.id);
    assert.equal(readFileSync(join(root, "loss/existing-gate-result"), "utf8").trim(), "accepted");
  } else {
    assert.equal(readFileSync(join(root, "control/existing-gate-result"), "utf8").trim(), "accepted");
  }
}
writeFileSync(join(root, "targeted-verdict.json"), JSON.stringify({
  status: "passed", authority: "targeted_experiment_only",
  control_dispatches: 1, fault_dispatches: 2, total_effects: 2,
  formal_gate_modified: true, historical_attempt1_cause: "unknown",
}, null, 2) + "\n");
console.log("PASS: real same-session loss/reconcile chain; not a formal G6 verdict");
