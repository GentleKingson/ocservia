import { execFileSync } from "node:child_process";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { describe, expect, it } from "vitest";
import {
  installCommand,
  installEnv,
  validInstallConfig,
  validInstallField,
  type NodeInstallConfig,
} from "../src/features/nodes/node-install";

const workspace = "019fc0a4-6d92-765c-a8a1-4af556614fa1";
const config: NodeInstallConfig = {
  name: "edge-1",
  environment: "production",
  release: "v1.2.3",
  controllerEndpointId: "a".repeat(64),
  relayUrl: "https://relay.example.com:443/",
  bootstrapTokenFile: "/protected/bootstrap",
  relayAccessTokenFile: "/protected/relay",
  controllerCommandVerificationKeyFile: "/protected/command.pem",
};

describe("installer configuration", () => {
  it.each([
    ["release", "main"],
    ["release", "latest"],
    ["release", "v1.2.3;id"],
    ["release", "v1.2.3-rc.01"],
    ["controllerEndpointId", "A".repeat(64)],
    ["controllerEndpointId", "a".repeat(63)],
    ["relayUrl", "http://relay.example.com"],
    ["relayUrl", "https://"],
    ["relayUrl", "https://user:pass@relay.example.com"],
    ["relayUrl", "https://relay.example.com?token=x"],
    ["relayUrl", "https://relay.example.com#fragment"],
    ["relayUrl", "https://relay.example.com\\path"],
    ["relayUrl", "https://relay.example.com:invalid"],
    ["bootstrapTokenFile", "relative/file"],
    ["bootstrapTokenFile", "/protected/../key"],
    ["bootstrapTokenFile", "/protected/$(id)"],
    ["name", "node\nRELAY_URL_B=https://other"],
    ["environment", "prod\rtest"],
  ] as const)("rejects %s = %s", (field, value) => {
    expect(validInstallField(field, value)).toBe(false);
    expect(installEnv({ ...config, [field]: value }, workspace)).toBe("");
  });
  it("rejects newline injection in every field and workspace comments", () => {
    for (const field of Object.keys(config) as (keyof NodeInstallConfig)[]) {
      expect(validInstallField(field, `${config[field]}\nEVIL=1`)).toBe(false);
    }
    expect(installEnv(config, `${workspace}\nEVIL=1`)).toBe("");
    expect(
      validInstallConfig({
        ...config,
        relayAccessTokenFile: config.bootstrapTokenFile,
      }),
    ).toBe(false);
  });
  it("pins stable and candidate commands without credential material", () => {
    expect(installCommand("v1.2.3-rc.1")).toContain("--version v1.2.3-rc.1");
    expect(installCommand("main")).toBe("");
    expect(installCommand(config.release)).not.toMatch(/token|SOURCE|private/i);
    expect(installEnv(config, workspace)).not.toContain("obt1_");
  });
  it("passes the actual installer's embedded loader and input validation", () => {
    const directory = mkdtempSync(join(tmpdir(), "node-install-config-"));
    try {
      const input = {
        ...config,
        bootstrapTokenFile: join(directory, "bootstrap"),
        relayAccessTokenFile: join(directory, "relay"),
        controllerCommandVerificationKeyFile: join(directory, "public.pem"),
      };
      writeFileSync(input.bootstrapTokenFile, "synthetic-bootstrap", {
        mode: 0o600,
      });
      writeFileSync(input.relayAccessTokenFile, "synthetic-relay", {
        mode: 0o600,
      });
      const privateKey = execFileSync("openssl", [
        "genpkey",
        "-algorithm",
        "ED25519",
      ]);
      writeFileSync(
        input.controllerCommandVerificationKeyFile,
        execFileSync("openssl", ["pkey", "-pubout"], { input: privateKey }),
        { mode: 0o600 },
      );
      writeFileSync(
        join(directory, "install.env"),
        installEnv(input, workspace),
        { mode: 0o600 },
      );
      const installer = readFileSync(
        new URL("../../deploy/managed-node/install.sh", import.meta.url),
        "utf8",
      );
      const loader = installer.slice(
        installer.indexOf("install_env_die() {"),
        installer.indexOf("# Configuration-derived defaults"),
      );
      const validation = installer.slice(
        installer.indexOf("validate_operator_inputs() {"),
        installer.indexOf("create_launcher_staging() {"),
      );
      const script = `set -euo pipefail\nfail() { exit 1; }\n${loader}\n${validation}\nvalidate_operator_inputs\nvalidate_bootstrap_token_source\nprintf '%s\\n' "$CONTROLLER_ENDPOINT_ID" "$ENROLLMENT_ENVIRONMENT" "$RELAY_URL_A" "$BOOTSTRAP_TOKEN_SOURCE"`;
      const output = execFileSync("bash", ["-c", script], {
        cwd: directory,
        env: { PATH: process.env.PATH },
        encoding: "utf8",
      });
      expect(output).toContain(
        `${input.controllerEndpointId}\n${input.environment}\n${input.relayUrl}\n${input.bootstrapTokenFile}`,
      );
    } finally {
      rmSync(directory, { recursive: true, force: true });
    }
  });
});
