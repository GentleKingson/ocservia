import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { execFileSync } from "node:child_process";
const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "upgrade-redaction-"));
try {
  fs.mkdirSync(`${tmp}/secrets`);
  fs.writeFileSync(`${tmp}/secrets/password`, "fixture-password-never-upload\n");
  fs.writeFileSync(`${tmp}/secrets/private-key`, "-----BEGIN PRIVATE KEY-----\nSENSITIVE-KEY-LINE-0123456789\n-----END PRIVATE KEY-----\n");
  fs.writeFileSync(`${tmp}/raw`, "fixture-password-never-upload SENSITIVE-KEY-LINE-0123456789 postgres://user:other@database:5432/test health=failed");
  execFileSync(process.execPath, [new URL("./redact-release-upgrade-log.mjs", import.meta.url).pathname, `${tmp}/raw`, `${tmp}/secrets`, `${tmp}/safe`]);
  const safe = fs.readFileSync(`${tmp}/safe`, "utf8");
  assert(!safe.includes("fixture-password") && !safe.includes("SENSITIVE-KEY") && !safe.includes("user:other"));
  assert(safe.includes("health=failed"));
} finally { fs.rmSync(tmp, { recursive: true }); }
console.log("Upgrade diagnostic credential redaction passed");
