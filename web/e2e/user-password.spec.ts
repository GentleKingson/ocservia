import { expect, test, type Page } from "@playwright/test";
import { createHash, generateKeyPairSync } from "node:crypto";
import { mkdtempSync, writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { spawnSync } from "node:child_process";

const workspaceId = "019fc0a4-6d92-765c-a8a1-4af556614cc1";
const nodeId = "019fc0a4-6d92-765c-a8a1-4af556614cc2";
const node = {
  id: nodeId,
  name: "Password node",
  version: 1,
  effective_actions: { "user.manage": { allowed: true, reason: "available" } },
  trust_status: "active",
  connection_state: "online",
  freshness: "fresh",
  dropped: { security: 0, health: 0, aggregate: 0, raw: 0 },
  session_count: 0,
};
const { publicKey, privateKey } = generateKeyPairSync("rsa", {
  modulusLength: 2048,
});
const der = publicKey.export({ format: "der", type: "spki" });
const binding = {
  workspace_id: workspaceId,
  node_id: nodeId,
  endpoint_id: "a".repeat(64),
  version: 1,
  purpose: "user_password",
  key_id: "browser-key",
  public_key_sha256: createHash("sha256").update(der).digest("hex"),
  public_key_der: der.toString("base64"),
};

async function fixture(page: Page) {
  const requests: string[] = [];
  const logs: string[] = [];
  let version = 0;
  let keyStatus = 200;
  let keyValue = binding;
  const received: Record<string, unknown>[] = [];
  page.on("console", (message) => logs.push(message.text()));
  page.on("request", (request) =>
    requests.push(request.url() + (request.postData() ?? "")),
  );
  await page.route("**/api/v1/**", async (route) => {
    const path = new URL(route.request().url()).pathname;
    let body: unknown = {};
    let status = 200;
    if (path === "/api/v1/workspaces")
      body = {
        items: [
          {
            id: workspaceId,
            name: "Password workspace",
            slug: "password",
            version: 1,
          },
        ],
      };
    else if (path === "/api/v1/nodes")
      body = { items: [node], page: { has_more: false } };
    else if (path === `/api/v1/nodes/${nodeId}`) body = node;
    else if (path.endsWith("/sessions") || path.endsWith("/ip-bans"))
      body = { items: [], page: { has_more: false } };
    else if (path.endsWith("/user-group-state"))
      body = {
        items: version
          ? [
              {
                kind: "user",
                name: "browser-user",
                desired_version: version,
                desired_revision: version,
                desired_enabled: true,
                convergence: "converged",
                recovery_required: false,
              },
            ]
          : [],
      };
    else if (path.endsWith("/user-password-sealing-key")) {
      status = keyStatus;
      body =
        status === 200
          ? keyValue
          : {
              status,
              type: "https://ocservia.dev/problems/sealing-key-unavailable",
            };
    } else if (path.endsWith("/users") || path.endsWith(":rotate-password")) {
      received.push(route.request().postDataJSON() as Record<string, unknown>);
      version++;
      status = 202;
      body = {
        id: `019fc0a4-6d92-765c-a8a1-4af556614cc${String(version + 3)}`,
        node_id: nodeId,
        workspace_id: workspaceId,
        state: "succeeded",
        version,
        created_at: "2026-09-30T00:00:00Z",
        expires_at: "2026-09-30T01:00:00Z",
      };
    } else if (path.startsWith("/api/v1/operations/"))
      body = {
        id: path.split("/").pop(),
        node_id: nodeId,
        workspace_id: workspaceId,
        state: "succeeded",
        version,
        created_at: "2026-09-30T00:00:00Z",
        expires_at: "2026-09-30T01:00:00Z",
      };
    else if (path === "/api/v1/events/stream") {
      await route.abort();
      return;
    }
    await route.fulfill({
      status,
      contentType:
        status >= 400 ? "application/problem+json" : "application/json",
      body: JSON.stringify(body),
    });
  });
  await page.goto(`/nodes/${nodeId}`);
  await expect(
    page.getByRole("heading", { name: "Password node", exact: true }),
  ).toBeVisible();
  return {
    received,
    requests,
    logs,
    setKey(status: number, value = binding) {
      keyStatus = status;
      keyValue = value;
    },
  };
}

test("creates and rotates from normal input with Chromium OAEP to OpenSSL interoperability", async ({
  page,
}) => {
  const f = await fixture(page);
  const directory = mkdtempSync(join(tmpdir(), "ocservia-browser-oaep-"));
  const path = join(directory, "fixture-private.pem");
  writeFileSync(path, privateKey.export({ type: "pkcs8", format: "pem" }), {
    mode: 0o600,
  });
  const passwords = [
    "browser create 密码 fixture",
    "browser rotate 密码 fixture",
  ];
  try {
    for (let index = 0; index < passwords.length; index++) {
      await page
        .getByTitle(index ? "Rotate password" : "Create user", { exact: true })
        .click();
      const dialog = page.getByRole("dialog");
      if (!index)
        await dialog.getByLabel("User", { exact: true }).fill("browser-user");
      await dialog
        .getByLabel("Password", { exact: true })
        .fill(passwords[index] ?? "");
      await dialog
        .getByLabel("Reason", { exact: true })
        .fill("test browser account");
      await dialog
        .getByRole("button", { name: "Confirm", exact: true })
        .click();
      await expect(dialog).toHaveCount(0);
      await expect.poll(() => f.received.length).toBe(index + 1);
      const body = f.received[index];
      if (!body) throw new Error("missing submitted envelope");
      expect(body.expected_version).toBe(index);
      const envelope = body.sealed_password as {
        version: number;
        purpose: string;
        key_id: string;
        ciphertext: string;
      };
      expect(envelope).toMatchObject({
        version: 1,
        purpose: "user_password",
        key_id: "browser-key",
      });
      const decrypt = spawnSync(
        "openssl",
        [
          "pkeyutl",
          "-decrypt",
          "-inkey",
          path,
          "-pkeyopt",
          "rsa_padding_mode:oaep",
          "-pkeyopt",
          "rsa_oaep_md:sha256",
          "-pkeyopt",
          "rsa_mgf1_md:sha256",
        ],
        { input: Buffer.from(envelope.ciphertext, "base64") },
      );
      expect(decrypt.status).toBe(0);
      expect(decrypt.stdout.toString()).toBe(passwords[index]);
      decrypt.stdout.fill(0);
    }
    const storage = await page.evaluate(() =>
      JSON.stringify({
        local: Object.entries(localStorage),
        session: Object.entries(sessionStorage),
      }),
    );
    for (const password of passwords) {
      expect(f.requests.join("\n")).not.toContain(password);
      expect(f.logs.join("\n")).not.toContain(password);
      expect(storage).not.toContain(password);
    }
  } finally {
    rmSync(directory, { recursive: true });
  }
});

test("clears passwords on failed reads, invalid bindings, byte overflow, cancel and F5", async ({
  page,
}) => {
  const f = await fixture(page);
  await page.getByTitle("Create user", { exact: true }).click();
  const dialog = page.getByRole("dialog");
  await dialog.getByLabel("User", { exact: true }).fill("browser-user");
  await dialog.getByLabel("Reason", { exact: true }).fill("test error path");
  for (const scenario of [
    {
      status: 503,
      value: binding,
      password: "temporary fixture",
      message: "verified public key is unavailable",
    },
    {
      status: 403,
      value: binding,
      password: "temporary fixture",
      message: "do not have permission",
    },
    {
      status: 200,
      value: { ...binding, node_id: workspaceId },
      password: "temporary fixture",
      message: "binding changed or is invalid",
    },
    {
      status: 200,
      value: binding,
      password: "界".repeat(64),
      message: "UTF-8 byte limit",
    },
  ]) {
    f.setKey(scenario.status, scenario.value);
    await dialog
      .getByLabel("Password", { exact: true })
      .fill(scenario.password);
    await dialog.getByRole("button", { name: "Confirm", exact: true }).click();
    await expect(dialog.getByRole("alert")).toContainText(scenario.message);
    await expect(dialog.getByLabel("Password", { exact: true })).toHaveValue(
      "",
    );
    expect(f.received).toHaveLength(0);
  }
  await dialog
    .getByLabel("Password", { exact: true })
    .fill("cancelled fixture");
  await page.keyboard.press("Escape");
  await page.getByTitle("Create user", { exact: true }).click();
  await expect(dialog.getByLabel("Password", { exact: true })).toHaveValue("");
  await dialog.getByLabel("Password", { exact: true }).fill("reload fixture");
  await page.reload();
  await expect(dialog).toHaveCount(0);
  await page.getByTitle("Create user", { exact: true }).click();
  await expect(dialog.getByLabel("Password", { exact: true })).toHaveValue("");
});
