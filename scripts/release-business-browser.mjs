// Real signed gateway/API only: no route interception or TLS exceptions.
import fs from "node:fs";
import crypto from "node:crypto";
import { execFileSync } from "node:child_process";
import { createRequire } from "node:module";
const require = createRequire(new URL("../web/package.json", import.meta.url));
const { chromium, expect } = require("@playwright/test");
const work = process.env.T07_WORK;
const node = process.env.T07_NODE;
const workspace = process.env.T07_WORKSPACE;
const browser = await chromium.launch({ headless: true });
const observations = [];
try {
  const context = await browser.newContext({ baseURL: "https://localhost", locale: "en-US" });
  const page = await context.newPage();
  const approver = await browser.newContext({ baseURL: "https://localhost" });
  const headers = { Origin: "https://localhost", "X-Workspace-ID": workspace };
  const approverLogin = await approver.request.post("/api/v1/auth/login", { headers, data: {
    username: "t07-approver", password: fs.readFileSync(`${work}/private/approver-password`, "utf8").trim(),
  } });
  expect(approverLogin.status()).toBe(204);
  async function approval(action, cert, extra = {}) {
    const response = await context.request.post("/api/v1/approval-requests", { headers, data: {
      action, resource_type: "certificate", resource_id: cert.id, reason: "T07 browser approval", ttl_seconds: 600, ...extra,
    } });
    expect(response.status()).toBe(201);
    const value = await response.json();
    const data = { expected_request_hash: value.request_hash, reason: "T07 separate principal" };
    expect((await context.request.post(`/api/v1/approval-requests/${value.id}:approve`, { headers, data })).status()).toBe(403);
    expect((await approver.request.post(`/api/v1/approval-requests/${value.id}:approve`, { headers, data })).status()).toBe(200);
    return value.id;
  }
  await page.goto(`/nodes/${node}`);
  await expect(page).toHaveURL(/\/login$/);
  await page.getByLabel("Username", { exact: true }).fill("t07-requester");
  await page.getByLabel("Password", { exact: true }).fill(fs.readFileSync(`${work}/private/requester-password`, "utf8").trim());
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(page.getByRole("heading", { name: "t07-native", exact: true })).toBeVisible();
  await expect(page.getByLabel("Workspace")).toContainText("T07");
  const denied = await context.request.get("/api/v1/nodes", { headers: { "X-Workspace-ID": "00000000-0000-7000-8000-000000000072" } });
  expect(denied.status()).toBe(403);
  const password = crypto.randomBytes(24).toString("hex");
  fs.writeFileSync(`${work}/private/browser-vpn-password`, password, { mode: 0o600 });
  const ciphertext = crypto.publicEncrypt({ key: fs.readFileSync(`${work}/user.pub.pem`), oaepHash: "sha256" }, Buffer.from(password)).toString("base64");
  await page.getByTitle("Create user", { exact: true }).click();
  await page.getByLabel("User", { exact: true }).fill("t07-browser");
  await page.getByLabel("Secret key ID").fill("t07-user");
  await page.getByLabel("Sealed password").fill(ciphertext);
  await page.getByLabel("Reason", { exact: true }).fill("T07 browser user");
  const created = page.waitForResponse(response => response.url().endsWith(`/nodes/${node}/users`) && response.request().method() === "POST");
  await page.getByRole("button", { name: "Confirm", exact: true }).click();
  const createResponse = await created;
  expect(createResponse.status()).toBe(202);
  const operation = await createResponse.json();
  await expect(page.locator("strong.succeeded")).toBeVisible({ timeout: 120000 });
  observations.push({ name: "browser_user_mutation", operation });
  await page.reload();
  await expect(page.getByText("t07-browser", { exact: true })).toBeVisible();
  await page.screenshot({ path: `${process.env.ARTIFACT_DIR}/browser-node.png`, fullPage: true });

  await page.getByTitle("Reload Ocserv", { exact: true }).click();
  await page.getByLabel("Reason", { exact: true }).fill("T07 browser approved reload");
  await page.getByLabel("Approval ID").fill(fs.readFileSync(`${work}/browser-approval`, "utf8").trim());
  const reloaded = page.waitForResponse(response => response.url().endsWith(`/nodes/${node}/service:reload`) && response.request().method() === "POST");
  await page.getByRole("button", { name: "Confirm", exact: true }).click();
  const reloadResponse = await reloaded;
  expect(reloadResponse.status()).toBe(202);
  const reload = await reloadResponse.json();
  await expect(page.locator("strong.succeeded")).toBeVisible({ timeout: 120000 });
  observations.push({ name: "browser_approved_reload", operation: reload });

  await page.getByTitle("Certificate lifecycle", { exact: true }).click();
  await page.getByLabel("Common name").fill("t07-browser-client");
  await page.getByLabel("Reason", { exact: true }).fill("T07 browser CSR");
  const csr = page.waitForResponse(response => response.url().endsWith(`/nodes/${node}/certificates`) && response.request().method() === "POST");
  await page.getByRole("button", { name: "Request CSR", exact: true }).click();
  const csrResponse = await csr;
  expect(csrResponse.status()).toBe(202);
  const certificate = await csrResponse.json();
  await expect(page.getByText("csr_ready", { exact: true })).toBeVisible({ timeout: 120000 });
  observations.push({ name: "browser_certificate_csr", certificate_id: certificate.id, operation_id: certificate.operation_id });
  await page.getByRole("button", { name: "Cancel", exact: true }).click();
  await page.reload();
  await page.getByTitle("Certificate lifecycle", { exact: true }).click();
  await expect(page.getByText("csr_ready", { exact: true })).toBeVisible();
  await page.screenshot({ path: `${process.env.ARTIFACT_DIR}/browser-certificate.png`, fullPage: true });
  await page.getByLabel("Approval ID").fill(await approval("certificate.issue", certificate));
  await page.getByLabel("Reason", { exact: true }).fill("T07 browser issue");
  const issuing = page.waitForResponse(response => response.url().endsWith(`/certificates/${certificate.id}:issue`));
  await page.getByRole("button", { name: "Issue certificate", exact: true }).click();
  const issueResponse = await issuing;
  expect(issueResponse.status()).toBe(200);
  const issued = await issueResponse.json();
  await expect(page.getByText("issued", { exact: true })).toBeVisible();
  await expect(page.getByLabel("Approval ID")).toBeVisible();
  const id = BigInt(Date.now()) << 80n | 7n << 76n | BigInt(`0x${crypto.randomBytes(2).toString("hex")}`) % 4096n << 64n | 2n << 62n | BigInt(`0x${crypto.randomBytes(8).toString("hex")}`) % (1n << 62n);
  const artifact = id.toString(16).padStart(32, "0").replace(/(.{8})(.{4})(.{4})(.{4})(.{12})/, "$1-$2-$3-$4-$5");
  const reason = "T07 browser export";
  await page.getByLabel("Approval ID").fill(await approval("certificate.private_key.export", issued, {
    certificate: { expected_version: issued.version, purpose: "certificate_p12", artifact_request_id: artifact, reason },
  }));
  await page.getByLabel("Reason", { exact: true }).fill(reason);
  const exporting = page.waitForResponse(response => response.url().endsWith(`/certificates/${certificate.id}:p12`));
  await page.getByRole("button", { name: "Create P12", exact: true }).click();
  const exportResponse = await exporting;
  expect(exportResponse.status()).toBe(202);
  const grant = await exportResponse.json();
  fs.writeFileSync(`${work}/private/browser-p12-password`, grant.password, { mode: 0o600 });
  fs.writeFileSync(`${work}/private/browser-p12-token`, grant.download_token, { mode: 0o600 });
  await expect(page.getByRole("button", { name: "Download", exact: true })).toBeEnabled({ timeout: 120000 });
  const downloadPromise = page.waitForEvent("download");
  await page.getByRole("button", { name: "Download", exact: true }).click();
  await (await downloadPromise).saveAs(`${work}/private/browser.p12`);
  execFileSync("openssl", ["pkcs12", "-in", `${work}/private/browser.p12`, "-passin", `file:${work}/private/browser-p12-password`, "-noout"], { stdio: "pipe" });
  expect((await context.request.get(`/api/v1/artifacts/${grant.artifact_id}`, { headers: { ...headers, "X-Artifact-Token": grant.download_token } })).status()).toBe(403);
  observations.push({ name: "browser_certificate_issue_export_one_use", certificate_id: issued.id, operation: grant.operation });
  const revokeReason = "T07 browser revoke";
  await page.getByLabel("Approval ID").fill(await approval("certificate.revoke", issued, { certificate: { expected_version: issued.version, reason: revokeReason } }));
  await page.getByLabel("Reason", { exact: true }).fill(revokeReason);
  const revoking = page.waitForResponse(response => response.url().endsWith(`/certificates/${certificate.id}:revoke`));
  await page.getByRole("button", { name: "Revoke", exact: true }).click();
  const revokeResponse = await revoking;
  expect(revokeResponse.status()).toBe(202);
  observations.push({ name: "browser_certificate_revoke", operation: await revokeResponse.json() });
  await expect(page.getByText("revoked", { exact: true })).toBeVisible({ timeout: 120000 });
  observations.push({ name: "browser_login_workspace_forbidden_refresh", status: "PASS" });
} finally {
  fs.writeFileSync(`${process.env.ARTIFACT_DIR}/browser-checkpoints.json`, JSON.stringify(observations));
  await browser.close();
}
