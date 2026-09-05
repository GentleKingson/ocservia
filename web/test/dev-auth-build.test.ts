import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { resolveConfig } from "vite";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const configFile = fileURLToPath(new URL("../vite.config.ts", import.meta.url));
const token = "fictional-build-guard-token";
let root: string;

beforeEach(() => {
  root = mkdtempSync(join(tmpdir(), "ocservia-build-env-"));
  vi.stubEnv("VITE_DEV_AUTH_TOKEN", undefined);
});

afterEach(() => {
  vi.unstubAllEnvs();
  rmSync(root, { recursive: true, force: true });
});

describe("development auth build boundary", () => {
  it.each(["production", "staging", "development"])(
    "rejects a process token in %s builds",
    async (mode) => {
      vi.stubEnv("VITE_DEV_AUTH_TOKEN", token);
      await expect(
        resolveConfig({ root, configFile, mode }, "build"),
      ).rejects.toThrow("VITE_DEV_AUTH_TOKEN must not be set for builds");
    },
  );

  it.each([".env", ".env.local", ".env.production", ".env.production.local"])(
    "rejects a token from %s",
    async (file) => {
      writeFileSync(join(root, file), `VITE_DEV_AUTH_TOKEN=${token}\n`);
      await expect(
        resolveConfig({ root, configFile, mode: "production" }, "build"),
      ).rejects.toThrow("VITE_DEV_AUTH_TOKEN must not be set for builds");
    },
  );

  it("allows a release without a token", async () => {
    const config = await resolveConfig(
      { root, configFile, mode: "production" },
      "build",
    );
    expect(config.env.VITE_DEV_AUTH_TOKEN).toBeUndefined();
  });

  it("preserves development proxy authentication", async () => {
    vi.stubEnv("VITE_DEV_AUTH_TOKEN", token);
    const config = await resolveConfig({ root, configFile }, "serve");
    expect(config.server.proxy?.["/api"]).toMatchObject({
      headers: { Authorization: `Bearer ${token}` },
    });
  });
});
