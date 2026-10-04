#!/usr/bin/env node

import fs from "node:fs";
import path from "node:path";

const imageNames = ["gateway", "control", "transport", "backup", "postgres", "otel"];
const integratedImageNames = ["edge", "relay", "signer", "mysql_backup"];
const supportedPlatforms = ["linux/amd64", "linux/arm64"];
const imageReferencePattern = /^[^\s@]+(?:@sha256:[0-9a-f]{64}|:v[0-9]+\.[0-9]+\.[0-9]+(-rc[.][1-9][0-9]*)?)$/;
const semverPattern = /^[0-9]+\.[0-9]+\.[0-9]+(-rc[.][1-9][0-9]*)?$/;
const commitPattern = /^[0-9a-f]{40}$/;

function fail(message) {
  console.error(`controller release manifest: ${message}`);
  process.exit(2);
}

function usage() {
  console.error(
    "usage: generate-controller-release-manifest.mjs --output <path|-> " +
      "--release-version <version> --release-tag <tag> --source-commit <sha> " +
      "--platform <linux/amd64|linux/arm64> [--migration-dir <path>] " +
      "[--manifest-version <1|2>] --image <name=ref> ...",
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
      if (![...imageNames, ...integratedImageNames].includes(name) || values.images.has(name)) {
        fail(`image must be one unique production name (${imageNames.join(", ")})`);
      }
      values.images.set(name, ref);
      continue;
    }
    if (["--output", "--release-version", "--release-tag", "--source-commit", "--platform", "--migration-dir", "--manifest-version"].includes(argument)) {
      const value = argv[++index];
      if (!value) usage();
      const key = {
        "--output": "output",
        "--release-version": "releaseVersion",
        "--release-tag": "releaseTag",
        "--source-commit": "sourceCommit",
        "--platform": "platform",
        "--migration-dir": "migrationDir",
        "--manifest-version": "manifestVersion",
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
  let schema;
  try {
    schema = fs.readFileSync(path.join(directory, "schema.sql"), "utf8");
  } catch (error) {
    fail(`cannot read schema artifact in ${directory}: ${error.message}`);
  }
  const headers = {};
  for (const line of schema.split("\n")) {
    if (line.startsWith("-- ocservia:step=")) break;
    const match = /^-- ocservia:(artifact|format|engine|epoch|revision)=(.+)$/.exec(line);
    if (!match) continue;
    if (headers[match[1]] !== undefined) fail(`duplicate schema header ${match[1]}`);
    headers[match[1]] = match[2];
  }
  if (headers.artifact !== "schema" || headers.format !== "1" || headers.engine !== "postgresql") {
    fail("unsupported PostgreSQL schema artifact");
  }
  for (const key of ["epoch", "revision"]) {
    if (!/^(0|[1-9][0-9]*)$/.test(headers[key] ?? "") || !Number.isSafeInteger(Number(headers[key]))) {
      fail(`invalid schema ${key}`);
    }
  }
  if (Number(headers.epoch) < 1) fail("invalid schema epoch");
  return { epoch: Number(headers.epoch), revision: Number(headers.revision) };
}

const values = parseArguments(process.argv.slice(2));
const manifestVersion = values.manifestVersion ?? "1";
if (!["1", "2"].includes(manifestVersion)) fail("manifest version must be 1 or 2");
const requiredImages = manifestVersion === "2" ? [...imageNames, ...integratedImageNames] : imageNames;
if ([...values.images.keys()].some((name) => !requiredImages.includes(name))) {
  fail("image is not supported by this manifest version");
}
if (!values.output || !values.releaseVersion || !values.releaseTag || !values.sourceCommit || !values.platform) usage();
if (!semverPattern.test(values.releaseVersion)) fail(`release version is not plain SemVer: ${values.releaseVersion}`);
if (values.releaseTag !== `v${values.releaseVersion}`) {
  fail(`release tag must be v${values.releaseVersion}`);
}
if (!commitPattern.test(values.sourceCommit)) fail("source commit must be a lowercase 40-character Git SHA");
if (!supportedPlatforms.includes(values.platform)) {
  fail(`platform must be one of the supported release platforms (${supportedPlatforms.join(", ")}): ${values.platform}`);
}
for (const name of requiredImages) {
  if (!values.images.has(name)) fail(`missing production image: ${name}`);
  const ref = values.images.get(name);
  if (!imageReferencePattern.test(ref)) fail(`${name} must be a version-tagged or sha256 image reference`);
}

const manifest = {
  manifest_version: Number(manifestVersion),
  release_version: values.releaseVersion,
  release_tag: values.releaseTag,
  source_commit: values.sourceCommit,
  platform: values.platform,
  database_migration: deriveMigrationHead(path.resolve(values.migrationDir)),
  ...(manifestVersion === "2" ? { signer_state_version: 1 } : {}),
  images: Object.fromEntries(requiredImages.map((name) => [name, values.images.get(name)])),
};
const serialized = `${JSON.stringify(manifest, null, 2)}\n`;

if (values.output === "-") {
  process.stdout.write(serialized);
} else {
  fs.writeFileSync(values.output, serialized, { encoding: "utf8", mode: 0o644 });
}
