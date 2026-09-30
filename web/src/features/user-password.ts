import type { UserPasswordSealingKey } from "@ocservia/api-client";

// Takes ownership of the UTF-8 buffer; clear it on every outcome. The caller
// clears the password input before awaiting network or cryptographic work.
export async function sealUserPassword(
  plaintext: Uint8Array<ArrayBuffer>,
  binding: Omit<UserPasswordSealingKey, "purpose" | "version"> & {
    purpose: string;
    version: number;
  },
  workspaceId: string | undefined,
  nodeId: string,
): Promise<{ keyId: string; ciphertext: string }> {
  let der: Uint8Array<ArrayBuffer> | undefined;
  try {
    const browserCrypto = Reflect.get(globalThis, "crypto") as
      Crypto | undefined;
    const subtle = Reflect.get(browserCrypto ?? {}, "subtle") as
      SubtleCrypto | undefined;
    if (!subtle) throw new Error("passwordCryptoUnavailable");
    if (
      !workspaceId ||
      binding.workspaceId !== workspaceId ||
      binding.nodeId !== nodeId ||
      binding.purpose !== "user_password" ||
      binding.version !== 1 ||
      !/^[0-9a-f]{64}$/.test(binding.endpointId) ||
      !binding.keyId ||
      binding.keyId.length > 128 ||
      !/^[0-9a-f]{64}$/.test(binding.publicKeySha256) ||
      !binding.publicKeyDer ||
      binding.publicKeyDer.length > 1368
    )
      throw new Error("passwordKeyChanged");
    try {
      const binary = atob(binding.publicKeyDer);
      if (btoa(binary) !== binding.publicKeyDer) throw new Error();
      der = Uint8Array.from(binary, (value) => value.charCodeAt(0));
      const digest = new Uint8Array(await subtle.digest("SHA-256", der));
      if (
        Array.from(digest, (byte) => byte.toString(16).padStart(2, "0")).join(
          "",
        ) !== binding.publicKeySha256
      )
        throw new Error();
    } catch {
      throw new Error("passwordKeyChanged");
    }
    let key: CryptoKey;
    try {
      key = await subtle.importKey(
        "spki",
        der,
        { name: "RSA-OAEP", hash: "SHA-256" },
        false,
        ["encrypt"],
      );
    } catch {
      throw new Error("passwordKeyChanged");
    }
    const algorithm = key.algorithm as RsaHashedKeyAlgorithm;
    if (
      algorithm.modulusLength < 2048 ||
      algorithm.modulusLength > 4096 ||
      Array.from(algorithm.publicExponent).join(",") !== "1,0,1"
    )
      throw new Error("passwordKeyChanged");
    if (
      !plaintext.length ||
      plaintext.length > Math.ceil(algorithm.modulusLength / 8) - 2 * 32 - 2
    )
      throw new Error("passwordTooLong");
    const ciphertext = new Uint8Array(
      await subtle.encrypt({ name: "RSA-OAEP" }, key, plaintext),
    );
    return {
      keyId: binding.keyId,
      ciphertext: btoa(String.fromCharCode(...ciphertext)),
    };
  } finally {
    plaintext.fill(0);
    der?.fill(0);
  }
}
