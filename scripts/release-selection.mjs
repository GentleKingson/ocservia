import fs from "node:fs";
import { execFileSync } from "node:child_process";
import { pathToFileURL } from "node:url";

// First match wins. Unclassified paths select both specialized checks.
export const rules = [
  [/^(docs\/(acceptance|reference)\/|docs\/development\/(release-|github-actions|g6-)|SECURITY\.md$)/, ["integration", "resilience"]],
  [/^docs\/.*\.md$|^(README|CONTRIBUTING|CHANGELOG)\.md$|^LICENSE(?:\..*)?$/, []],
  [/^(web\/|openapi\/|control-plane\/gen\/|control-plane\/internal\/(gateway|session|auth|pki|approval)[^/]*\/)/, ["integration"]],
  [/^(proto\/|control-plane\/|rust\/crates\/(contracts|command-authorization|privd-attestation)\/)/, ["integration", "resilience"]],
  [/^(rust\/crates\/|rust\/vendor\/|deploy\/g6-|tools\/g6-harness\/|scripts\/(?:test-)?g6-|scripts\/(?:build|verify)-g6-)/, ["resilience"]],
  [/^scripts\/(?:test-)?release-business-/, ["integration"]],
];

export function selectChecks(paths, fallback = "") {
  const selected = new Set();
  const reasons = {};
  const select = (checks, reason) => {
    for (const check of checks) {
      selected.add(check);
      (reasons[check] ??= []).push(reason);
    }
  };
  if (fallback) select(["integration", "resilience"], fallback);
  else for (const path of paths) {
    const rule = rules.find(([pattern]) => pattern.test(path));
    select(rule?.[1] ?? ["integration", "resilience"], path);
  }
  return Object.fromEntries(["integration", "resilience"].map(check => [check,
    { selected: selected.has(check), reasons: reasons[check] ?? ["not selected: no relevant changes"] }]));
}

export function checkResults(selection, results, required) {
  for (const check of ["integration", "resilience"]) {
    if (typeof selection[check]?.selected !== "boolean") throw new Error(`missing selection: ${check}`);
    const expected = selection[check].selected ? "success" : "skipped";
    if (results[check]?.result !== expected) throw new Error(`${check}: expected ${expected}, got ${results[check]?.result}`);
  }
  for (const job of required) {
    if (results[job]?.result !== "success") throw new Error(`${job}: not successful (${results[job]?.result})`);
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  const git = (...args) => execFileSync("git", args, { encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] }).trim();
  const candidate = git("rev-parse", "HEAD");
  if (candidate !== process.env.GITHUB_SHA) throw new Error("checkout does not match candidate");
  let base = null, paths = [], fallback = "";
  try {
    const pages = JSON.parse(execFileSync("gh", ["api", "--paginate", "--slurp",
      `repos/${process.env.GITHUB_REPOSITORY}/releases`], { encoding: "utf8" }));
    const release = pages.flat().filter(r => !r.draft && !r.prerelease && r.published_at && /^v\d+\.\d+\.\d+$/.test(r.tag_name))
      .sort((a, b) => Date.parse(b.published_at) - Date.parse(a.published_at))[0];
    if (!release) throw new Error("no published stable release");
    base = git("rev-parse", `refs/tags/${release.tag_name}^{commit}`);
    git("merge-base", "--is-ancestor", base, candidate);
    paths = git("diff", "--name-only", "--no-renames", "-z", base, candidate).split("\0").filter(Boolean);
    try { git("cat-file", "-e", `${base}:scripts/release-selection.mjs`); }
    catch { fallback = "first release using selective checks"; }
  } catch { fallback = "published baseline or complete diff unavailable"; }
  const selection = selectChecks(paths, fallback);
  const result = { candidate, base, ...selection };
  fs.appendFileSync(process.env.GITHUB_OUTPUT, `selection=${JSON.stringify(result)}\n` +
    Object.entries(selection).map(([key, value]) => `${key}=${value.selected}\n`).join(""));
  fs.appendFileSync(process.env.GITHUB_STEP_SUMMARY, `Release selection\n\n\`\`\`json\n${JSON.stringify(result, null, 2)}\n\`\`\`\n`);
}
