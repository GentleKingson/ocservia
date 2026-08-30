#!/usr/bin/env node

import fs from "node:fs";
import path from "node:path";

const imageNames = ["gateway", "control", "transport", "backup", "postgres", "otel"];
const imageDigestPattern = /^[^\s@]+@sha256:[0-9a-f]{64}$/;
const semverPattern = /^[0-9]+\.[0-9]+\.[0-9]+$/;
const commitPattern = /^[0-9a-f]{40}$/;

function fail(message) {
  console.error(`controller release manifest: ${message}`);
  process.exit(2);
}

function usage() {
  console.error(
    "usage: generate-controller-release-manifest.mjs --output <path|-> " +
      "--release-version <version> --release-tag <tag> --source-commit <sha> " +
      "[--migration-dir <path>] --image <name=ref> ...",
  );
  process.exit(2);
}

function parseArguments(argv) {
  const values = { images: new Map(), migrationDir: "control-plane/migrations" };
  for (let index = 0; index < argv.length; index += 1) {
    const argument = argv[index];
    if (argument === "--image") {
      const value = argv[++index];
      if (!value || !value.includes("=")) usage();
      const separator = value.indexOf("=");
      const name = value.slice(0, separator);
      const ref = value.slice(separator + 1);
      if (!imageNames.includes(name) || values.images.has(name)) {
        fail(`image must be one unique production name (${imageNames.join(", ")})`);
      }
      values.images.set(name, ref);
      continue;
    }
    if (["--output", "--release-version", "--release-tag", "--source-commit", "--migration-dir"].includes(argument)) {
      const value = argv[++index];
      if (!value) usage();
      const key = {
        "--output": "output",
        "--release-version": "releaseVersion",
        "--release-tag": "releaseTag",
        "--source-commit": "sourceCommit",
        "--migration-dir": "migrationDir",
      }[argument];
      if (values[key] !== undefined && key !== "migrationDir") fail(`${argument} was provided more than once`);
      values[key] = value;
      continue;
    }
    if (argument === "--help" || argument === "-h") usage();
    usage();
  }
  return values;
}

function deriveMigrationHead(directory) {
  let entries;
  try {
    entries = fs.readdirSync(directory, { withFileTypes: true });
  } catch (error) {
    fail(`cannot read migration directory ${directory}: ${error.message}`);
  }

  const migrations = [];
  const versions = new Set();
  for (const entry of entries) {
    if (!entry.isFile()) continue;
    if (!entry.name.endsWith(".up.sql")) continue;
    const match = /^(\d+)_.+\.up\.sql$/.exec(entry.name);
    if (!match) fail(`invalid migration name ${entry.name}`);
    const version = Number.parseInt(match[1], 10);
    if (!Number.isSafeInteger(version)) fail(`migration version is too large: ${match[1]}`);
    if (versions.has(version)) fail(`duplicate migration version ${match[1]}`);
    versions.add(version);
    migrations.push(version);
  }
  if (migrations.length === 0) fail(`no up migrations found in ${directory}`);
  return Math.max(...migrations);
}

const values = parseArguments(process.argv.slice(2));
if (!values.output || !values.releaseVersion || !values.releaseTag || !values.sourceCommit) usage();
if (!semverPattern.test(values.releaseVersion)) fail(`release version is not plain SemVer: ${values.releaseVersion}`);
if (values.releaseTag !== `v${values.releaseVersion}`) {
  fail(`release tag must be v${values.releaseVersion}`);
}
if (!commitPattern.test(values.sourceCommit)) fail("source commit must be a lowercase 40-character Git SHA");
for (const name of imageNames) {
  if (!values.images.has(name)) fail(`missing production image: ${name}`);
  const ref = values.images.get(name);
  if (!imageDigestPattern.test(ref)) fail(`${name} must be a full sha256 image digest`);
}

const manifest = {
  manifest_version: 1,
  release_version: values.releaseVersion,
  release_tag: values.releaseTag,
  source_commit: values.sourceCommit,
  database_migration: deriveMigrationHead(path.resolve(values.migrationDir)),
  images: Object.fromEntries(imageNames.map((name) => [name, values.images.get(name)])),
};
const serialized = `${JSON.stringify(manifest, null, 2)}\n`;

if (values.output === "-") {
  process.stdout.write(serialized);
} else {
  fs.writeFileSync(values.output, serialized, { encoding: "utf8", mode: 0o644 });
}
