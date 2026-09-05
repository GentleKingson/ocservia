import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { verifyRelayProof } from "./g6-relay-proof.mjs";

const root = process.argv[2];
const json = (path) => JSON.parse(readFileSync(join(root, path), "utf8"));
const original = json("loss/existing-gate-proof.json");
const originalAgent = json("loss/agent-proof.json");
const observation = json("loss/session-before.json").observations[0];
verifyRelayProof(original, observation, originalAgent);
for (const [name, mutate] of [
  ["extra delivery", (p) => p.deliveries.push(p.deliveries[1])],
  ["execute follow-up", (p) => { p.deliveries[1].delivery_mode = 1; }],
  ["reused message", (p) => { p.deliveries[1].message_id = p.message_id; }],
  ["changed semantics", (p) => { p.deliveries[1].semantic_payload_sha256 = "00".repeat(32); }],
  ["changed fence", (p) => { p.deliveries[1].owner_fence_id = "00".repeat(16); }],
  ["direct path", (p) => { p.deliveries[1].path = "direct"; }],
  ["missing reason", (p) => { p.recovery.reasons = []; }],
  ["wrong reason", (p) => { p.recovery.reasons[0].reason = "owner_timeout"; }],
  ["uncorrelated reason", (p) => { p.recovery.reasons[0].prior_message_id = "00".repeat(16); }],
  ["missing committed attempt", (p) => { p.recovery.database.attempts.pop(); }],
  ["missing unknown transition", (p) => { p.recovery.database.operation_events = []; }],
  ["locked outbox", (p) => { p.recovery.database.outbox.locked_by = "worker"; }],
  ["nonreplayed result", (p) => { p.recovery.database.results[0].replayed = false; }],
  ["changed result digest", (p) => { p.recovery.database.results[0].result_sha256 = "00".repeat(32); }],
  ["missing journal", (_p, a) => { a.journal = []; }],
  ["extra effect", (_p, a) => { a.journal[0].total_executions += 1; }],
  ["missing effect observation", (_p, a) => { a.events = a.events.filter((e) => e.event_type !== "synthetic_effect_observed"); }],
]) {
  const proof = structuredClone(original);
  const agent = structuredClone(originalAgent);
  mutate(proof, agent);
  assert.throws(() => verifyRelayProof(proof, observation, agent), name);
  console.log(`PASS rejects ${name}`);
}
