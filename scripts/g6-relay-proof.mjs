import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { pathToFileURL } from "node:url";

const hex = (value) => value.replaceAll("-", "").toLowerCase();

// Runtime verifies Controller/transport evidence. The final builder additionally
// requires the remote Agent's journal evidence before granting G6 authority.
export function verifyRelayProof(proof, observation, agent) {
  const writes = proof.deliveries;
  assert.ok(Array.isArray(writes) && [1, 2].includes(writes.length), "bounded relay deliveries");
  const first = writes[0];
  for (const [key, value] of Object.entries(first)) assert.deepEqual(proof[key], value, `first dispatch ${key}`);
  assert.equal(first.delivery_mode, 1);
  assert.equal(first.required_capability, "synthetic.noop");
  assert.equal(first.semantic_payload_hash_version, 2);
  assert.match(first.semantic_payload_sha256, /^[0-9a-f]{64}$/);
  assert.ok(Number.isSafeInteger(first.sequence) && first.sequence > 0);
  assert.ok(Number.isSafeInteger(first.expected_revision) && first.expected_revision > 0);
  for (const key of ["command_id", "operation_id", "node_id", "idempotency_key", "message_id"]) assert.match(first[key], /^[0-9a-f]{32}$/);
  for (const write of writes) {
    assert.equal(write.event_type, "command_frame_written");
    assert.equal(write.path, "relay");
    assert.match(write.path_detail, /relay-b/);
    for (const key of ["node_id", "owner_fence_id", "connection_id"]) assert.equal(write[key], hex(observation[key]));
    assert.equal(write.owner_epoch, observation.owner_epoch);
    for (const key of ["command_id", "operation_id", "node_id", "idempotency_key", "semantic_payload_sha256", "semantic_payload_hash_version", "sequence", "expected_revision", "required_capability"]) assert.equal(write[key], first[key], `unchanged ${key}`);
  }
  if (writes.length === 1) {
    assert.equal(proof.recovery, undefined);
    return;
  }
  const second = writes[1];
  assert.equal(second.delivery_mode, 2, "only reconcile-only is proven");
  assert.match(second.message_id, /^[0-9a-f]{32}$/);
  assert.notEqual(second.message_id, first.message_id);
  const { database: db, reasons, responses } = proof.recovery;
  for (const key of ["command_id", "operation_id", "node_id"]) assert.equal(hex(db[key]), first[key]);
  assert.equal(db.command_state, "succeeded");
  assert.equal(db.operation_state, "succeeded");
  assert.equal(db.outbox.attempts, 2);
  assert.ok(db.outbox.published_at);
  assert.equal(db.outbox.locked_by, null);
  assert.equal(hex(db.outbox.command_id), first.command_id);
  assert.equal(db.attempts.length, 2);
  for (const [index, attempt] of db.attempts.entries()) {
    assert.equal(attempt.attempt_number, index + 1);
    assert.equal(attempt.state, "sent");
    assert.equal(hex(attempt.command_id), first.command_id);
  }
  assert.ok(db.operation_events.some((event) => event.state === "unknown"));
  assert.equal(reasons.length, 1, "one correlated recovery reason");
  const reason = reasons[0];
  assert.equal(reason.event_type, "command_reconciliation_prepared");
  assert.equal(reason.reason, "sent_result_timeout");
  assert.equal(reason.prior_attempt, 1);
  assert.equal(reason.outbox_id, db.outbox.id);
  for (const key of ["command_id", "operation_id", "node_id"]) assert.equal(hex(reason[key]), first[key]);
  assert.equal(reason.prior_message_id, first.message_id);
  assert.equal(reason.message_id, second.message_id);
  assert.equal(reason.semantic_payload_sha256, first.semantic_payload_sha256);
  assert.equal(db.results.length, 1);
  const result = db.results[0];
  assert.equal(result.state, "succeeded");
  assert.equal(result.replayed, true);
  assert.equal(result.idempotency_key, first.idempotency_key);
  assert.equal(result.payload_sha256, first.semantic_payload_sha256);
  assert.match(result.result_sha256, /^[0-9a-f]{64}$/);
  const replay = responses.filter((response) => response.message_id === second.message_id);
  assert.ok(responses.length >= 1 && responses.length <= 2);
  assert.equal(replay.length, 1);
  assert.equal(replay[0].state, 1);
  assert.equal(replay[0].delivery_mode, 2);
  assert.equal(replay[0].replayed, true);
  const initial = responses.filter((response) => response.message_id === first.message_id);
  assert.ok(initial.length <= 1);
  if (initial.length) {
    assert.equal(initial[0].delivery_mode, 1);
    assert.equal(initial[0].replayed, false);
  }
  for (const response of responses) {
    assert.equal(response.command_id, first.command_id);
    assert.equal(response.node_id, first.node_id);
    assert.ok(writes.some((write) => write.message_id === response.message_id));
    assert.equal(response.state, 1);
  }
  if (agent === undefined) return;
  assert.equal(agent.journal.length, 1);
  const journal = agent.journal[0];
  assert.equal(hex(journal.command_id), first.command_id);
  assert.equal(hex(journal.idempotency_key), first.idempotency_key);
  assert.equal(journal.state, "succeeded");
  assert.ok(journal.effect_executed_at);
  assert.equal(journal.total_executions, journal.effect_rows, "no unaccounted synthetic effects");
  assert.ok(journal.total_executions > 0);
  const effects = agent.events.filter((event) => event.event_type === "synthetic_effect_observed");
  assert.equal(effects.length, 2);
  assert.ok(!agent.events.some((event) => event.event_type === "synthetic_effect_observation_failed"));
  const results = agent.events.filter((event) => event.event_type === "command_delivery_result");
  assert.equal(results.length, 2);
  for (const [index, effect] of effects.entries()) {
    assert.equal(results[index].command_id, first.command_id);
    assert.equal(results[index].message_id, writes[index].message_id);
    assert.equal(results[index].delivery_mode, writes[index].delivery_mode);
    assert.equal(results[index].state, 1);
    assert.equal(results[index].replayed, index === 1);
    assert.equal(effect.command_id, first.command_id);
    assert.equal(effect.message_id, writes[index].message_id);
    assert.equal(effect.idempotency_key, first.idempotency_key);
    assert.equal(effect.effect_present, true);
    assert.equal(effect.effect_executed_at, effects[0].effect_executed_at);
    assert.equal(effect.effect_executed_at, journal.effect_executed_at);
    assert.equal(effect.result_sha256, result.result_sha256);
    assert.equal(effect.effect_result_sha256, result.result_sha256);
    assert.equal(effect.effect_payload_sha256, first.semantic_payload_sha256);
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  const json = (path) => JSON.parse(readFileSync(path, "utf8"));
  verifyRelayProof(json(process.argv[2]), json(process.argv[3]).observations[0], process.argv[4] ? json(process.argv[4]) : undefined);
}
