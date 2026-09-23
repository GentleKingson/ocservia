import assert from "node:assert/strict";
import { selectChecks, checkResults } from "./release-selection.mjs";

const selected = paths => Object.values(selectChecks(paths)).map(value => value.selected);
assert.deepEqual(selected(["docs/how-to/enroll-node.md", "README.md"]), [false, false]);
assert.deepEqual(selected(["web/src/App.vue"]), [true, false]);
for (const path of ["proto/command.proto", "toolchains.lock", "unknown/path", "docs/reference/support-policy.md",
  "docs/acceptance/g6-slo.yaml", ".github/workflows/release.yml", "scripts/release-selection.mjs"])
  assert.deepEqual(selected([path]), [true, true], path);
assert.deepEqual(selected(["scripts/test-g6-relay-proof.mjs"]), [false, true]);
assert.deepEqual(selected(["web/src/App.vue", "rust/crates/agent/src/main.rs"]), [true, true]);
assert.deepEqual(Object.values(selectChecks([], "diff failed")).map(value => value.selected), [true, true]);
const required = ["build", "security", "smoke"];
for (const paths of [[], ["web/src/App.vue"], ["proto/shared.proto"]]) {
  const selection = selectChecks(paths);
  const results = Object.fromEntries(required.map(job => [job, { result: "success" }]));
  for (const [job, value] of Object.entries(selection)) results[job] = { result: value.selected ? "success" : "skipped" };
  checkResults(selection, results, required);
  for (const job of Object.keys(results)) {
    for (const state of ["failure", "cancelled", "skipped", "success", undefined]) {
      if (state === results[job].result) continue;
      assert.throws(() => checkResults(selection, { ...results, [job]: { result: state } }, required), undefined, `${job}: ${state}`);
    }
    const missing = { ...results }; delete missing[job];
    assert.throws(() => checkResults(selection, missing, required));
  }
}
assert.throws(() => checkResults({}, {}, []));
console.log("Release selection and selected-job failure/cancellation/missing-result checks passed");
