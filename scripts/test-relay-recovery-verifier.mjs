import assert from "node:assert/strict";
import { cpSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";

const source = process.argv[2];
const verifier = fileURLToPath(new URL("./verify-relay-recovery-experiment.mjs", import.meta.url));
const check = (path) => spawnSync(process.execPath, [verifier, path], { encoding: "utf8" });
assert.equal(check(source).status, 0, "unaltered real-chain evidence must pass");
const cases = [
  ["execute-instead-of-reconcile", (e) => { e.fields.delivery_mode = 1; }],
  ["changed-semantic-payload", (e) => { e.fields.semantic_payload_sha256 = "00".repeat(32); }],
  ["changed-session", (e) => { e.fields.connection_id = "00".repeat(16); }],
  ["changed-message-identity", (e, events) => { e.fields.message_id = events.find((v) => v.fields.event_type === "command_frame_written").fields.message_id; }],
];
for (const [name, mutate] of cases) {
  const path = mkdtempSync(join(tmpdir(), "relay-verifier-"));
  try {
    cpSync(source, path, { recursive: true });
    const file = join(path, "loss/transport-events.jsonl");
    const events = readFileSync(file, "utf8").trim().split("\n").map(JSON.parse);
    const writes = events.filter((event) => event.fields.event_type === "command_frame_written");
    mutate(writes[1], events);
    writeFileSync(file, events.map(JSON.stringify).join("\n") + "\n");
    assert.notEqual(check(path).status, 0, name);
    console.log(`PASS rejects ${name}`);
  } finally {
    rmSync(path, { recursive: true, force: true });
  }
}
for (const name of ["missing-injection", "extra-effect", "wrong-durable-result", "missing-recovery-reason"]) {
  const path = mkdtempSync(join(tmpdir(), "relay-verifier-"));
  try {
    cpSync(source, path, { recursive: true });
    if (name === "missing-injection") {
      const file = join(path, "loss/transport-events.jsonl");
      writeFileSync(file, readFileSync(file, "utf8").split("\n").filter((line) => !line.includes("test_command_result_dropped")).join("\n"));
    } else if (name === "missing-recovery-reason") {
      writeFileSync(join(path, "worker-live.log"), "");
    } else if (name === "extra-effect") {
      const file = join(path, "loss/journal.jsonl");
      const row = JSON.parse(readFileSync(file, "utf8"));
      row.total_executions += 1;
      writeFileSync(file, JSON.stringify(row));
    } else {
      const file = join(path, "loss/database.jsonl");
      const rows = readFileSync(file, "utf8").trim().split("\n").map(JSON.parse);
      rows.at(-1).results[0].result_sha256 = "00".repeat(32);
      writeFileSync(file, rows.map(JSON.stringify).join("\n") + "\n");
    }
    assert.notEqual(check(path).status, 0, name);
    console.log(`PASS rejects ${name}`);
  } finally {
    rmSync(path, { recursive: true, force: true });
  }
}
