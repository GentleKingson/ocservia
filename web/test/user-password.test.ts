import {
  generateKeyPairSync,
  createHash,
  privateDecrypt,
  constants,
} from "node:crypto";
import type { UserPasswordSealingKey } from "@ocservia/api-client";
import { describe, expect, it } from "vitest";
import { sealUserPassword } from "../src/features/user-password";

const { publicKey, privateKey } = generateKeyPairSync("rsa", {
  modulusLength: 2048,
});
const der = publicKey.export({ type: "spki", format: "der" });
const binding: UserPasswordSealingKey = {
  workspaceId: "workspace",
  nodeId: "node",
  endpointId: "a".repeat(64),
  purpose: "user_password",
  version: 1,
  keyId: "user-key",
  publicKeySha256: createHash("sha256").update(der).digest("hex"),
  publicKeyDer: der.toString("base64"),
};

describe("browser password envelope", () => {
  it("seals SHA256 OAEP with empty label and clears UTF-8 bytes", async () => {
    const bytes = new TextEncoder().encode("a unique 密码 fixture");
    const sealed = await sealUserPassword(bytes, binding, "workspace", "node");
    expect(sealed.keyId).toBe("user-key");
    expect(
      privateDecrypt(
        {
          key: privateKey,
          padding: constants.RSA_PKCS1_OAEP_PADDING,
          oaepHash: "sha256",
        },
        Buffer.from(sealed.ciphertext, "base64"),
      ).toString(),
    ).toBe("a unique 密码 fixture");
    expect(bytes.every((byte) => byte === 0)).toBe(true);
  });
  it.each([
    { nodeId: "other" },
    { workspaceId: "other" },
    { purpose: "certificate_p12_password" },
    { version: 2 },
    { keyId: "" },
    { publicKeySha256: "0".repeat(64) },
    { publicKeyDer: "malformed" },
    { endpointId: "invalid" },
  ])("rejects an invalid binding %j and clears input", async (change) => {
    const bytes = new TextEncoder().encode("temporary secret");
    await expect(
      sealUserPassword(bytes, { ...binding, ...change }, "workspace", "node"),
    ).rejects.toThrow("passwordKeyChanged");
    expect(bytes.every((byte) => byte === 0)).toBe(true);
  });
  it("uses the OAEP byte boundary rather than character count", async () => {
    await expect(
      sealUserPassword(
        new TextEncoder().encode("界".repeat(63) + "a"),
        binding,
        "workspace",
        "node",
      ),
    ).resolves.toHaveProperty("ciphertext");
    const bytes = new TextEncoder().encode("界".repeat(64));
    await expect(
      sealUserPassword(bytes, binding, "workspace", "node"),
    ).rejects.toThrow("passwordTooLong");
    expect(bytes.every((byte) => byte === 0)).toBe(true);
  });
});
