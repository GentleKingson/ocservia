import { describe, expect, it } from "vitest";
import {
  installCommand,
  validInstallConfig,
  validInstallField,
} from "../src/features/nodes/node-install";

describe("single-token installation", () => {
  it.each(["main", "latest", "v1.2.3;id", "v1.2.3-rc.01", "v1.2.3\nEVIL=1"])(
    "rejects an unpinned or unsafe release %s",
    (release) => {
      expect(installCommand(release)).toBe("");
    },
  );
  it("needs only node identity, environment and pinned release", () => {
    expect(
      validInstallConfig({
        name: "edge-1",
        environment: "production",
        release: "v1.2.3",
      }),
    ).toBe(true);
    expect(validInstallField("name", "node\nEVIL=1")).toBe(false);
    expect(validInstallField("environment", "production\rtest")).toBe(false);
  });
  it("prompts for the token instead of embedding credentials in argv", () => {
    const command = installCommand("v1.2.3-rc.1");
    expect(command).toContain(
      "sudo bash ./install.sh --join-token --version v1.2.3-rc.1",
    );
    expect(command).toContain("--https-only");
    expect(command).not.toMatch(/oit1_|obt1_|SOURCE|install\.env/);
  });
});
