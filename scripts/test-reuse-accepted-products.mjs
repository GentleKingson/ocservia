import assert from "node:assert/strict";
import { acceptedRun, producerArtifact } from "./reuse-accepted-products.mjs";
const sha = "a".repeat(40);
const run = { id: 123, run_attempt: 2, head_sha: sha, head_branch: "main", event: "workflow_dispatch",
  conclusion: "success", path: ".github/workflows/release-upgrade.yml" };
assert.equal(acceptedRun(run, sha), true);
for (const [field, value] of Object.entries({ head_sha: "b".repeat(40), head_branch: "branch", event: "pull_request",
  conclusion: "failure", path: ".github/workflows/other.yml" }))
  assert.equal(acceptedRun({ ...run, [field]: value }, sha), false);
const receipt = { source_commit: sha, version: "1.0.2", run_id: "123", run_attempt: "1",
  products: { amd64: { result: "success", outputs: { "controller-id": "456", "controller-sha256": "c".repeat(64) } } } };
assert.deepEqual(producerArtifact(receipt, sha, "1.0.2", run, "amd64", "controller"), { id: "456", digest: "c".repeat(64) });
for (const [field, value] of Object.entries({ source_commit: "b".repeat(40), version: "1.0.3", run_id: "124", run_attempt: "3" }))
  assert.throws(() => producerArtifact({ ...receipt, [field]: value }, sha, "1.0.2", run, "amd64", "controller"));
assert.throws(() => producerArtifact(receipt, sha, "1.0.2", run, "arm64", "controller"));
receipt.products.amd64.result = "failure";
assert.throws(() => producerArtifact(receipt, sha, "1.0.2", run, "amd64", "controller"));
console.log("Only successful main acceptance with exact SHA/version and original producer identity can be promoted");
