// Real signed gateway/API only: no route interception or TLS exceptions.
import fs from "node:fs";
import crypto from "node:crypto";
import { execFileSync } from "node:child_process";
import { createRequire } from "node:module";
const require = createRequire(new URL("../web/package.json", import.meta.url));
const { chromium, expect } = require("@playwright/test");
const work = process.env.OCSERVIA_BUSINESS_TEST_WORK_DIR;
const node = process.env.OCSERVIA_BUSINESS_TEST_NODE_ID;
const workspace = process.env.OCSERVIA_BUSINESS_TEST_WORKSPACE_ID;
const profile = process.env.BUSINESS_PROFILE ?? "extended";
if (!["smoke", "extended"].includes(profile))
  throw new Error(`Unknown business profile: ${profile}`);
// "rollout" is the extended final stage; it writes its own evidence files.
const rollout = process.argv[2] === "rollout";
const browser = await chromium.launch({ headless: true });
const observations = [];
// The gateway CSP is enforced with no report endpoint, so violations are
// collected in the page. Uploaded diagnostics keep no URL query or fragment.
// Each context reports under a fixed phase, so a late business violation can
// never be counted as part of the control.
const csp = { violations: [], console: [], page_errors: [] };
const redact = (text) =>
  String(text)
    .replace(/([a-z]+:\/\/[^\s?#'"]*)[?#][^\s'"]*/gi, "$1")
    .slice(0, 300);
async function collectCsp(browserContext, phase) {
  await browserContext.exposeBinding("__ocserviaCsp", (_source, violation) =>
    csp.violations.push({ phase, ...violation }),
  );
  await browserContext.addInitScript(() => {
    document.addEventListener("securitypolicyviolation", (event) =>
      window.__ocserviaCsp({
        directive: event.effectiveDirective,
        disposition: event.disposition,
        blocked: event.blockedURI,
        source: event.sourceFile,
        line: event.lineNumber,
      }),
    );
  });
  browserContext.on("console", (message) => {
    if (/content.security.policy/i.test(message.text()))
      csp.console.push({
        phase,
        type: message.type(),
        text: redact(message.text()),
      });
  });
  browserContext.on("weberror", (error) =>
    csp.page_errors.push({ phase, message: redact(error.error().message) }),
  );
}
try {
  const origin =
    process.env.OCSERV_CONTROLLER_PUBLIC_URL || "https://localhost";
  const context = await browser.newContext({
    baseURL: origin,
    locale: "en-US",
  });
  await collectCsp(context, "flow");
  const page = await context.newPage();
  const headers = { Origin: origin, "X-Workspace-ID": workspace };
  async function selectBrowserWorkspace(currentPage) {
    // Wait for the authorized list before deciding whether a selector exists.
    await expect(currentPage.getByTestId("workspace")).toContainText(
      "Business Test",
    );
    const selector = currentPage.getByRole("combobox", {
      name: "Workspace",
      exact: true,
    });
    if (await selector.count()) {
      await selector.selectOption(workspace);
      await expect(selector).toHaveValue(workspace);
    }
    await expect
      .poll(() =>
        currentPage.evaluate(() =>
          sessionStorage.getItem("ocservia.workspace-id"),
        ),
      )
      .toBe(workspace);
  }
  await page.goto(`/nodes/${node}`);
  await expect(page).toHaveURL(/\/login$/);
  await page
    .getByLabel("Username", { exact: true })
    .fill("business-test-requester");
  await page
    .getByLabel("Password", { exact: true })
    .fill(fs.readFileSync(`${work}/private/requester-password`, "utf8").trim());
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await selectBrowserWorkspace(page);
  if (rollout) {
    // The probe placed the next Agent release in the node spool and recreated
    // the Controller with its catalog; the console drives the canary rollout.
    const target = process.env.ROLLOUT_VERSION;
    await page.goto("/nodes");
    await page
      .getByRole("checkbox", {
        name: "Rolling upgrade: business-test-agent",
        exact: true,
      })
      .check();
    await page
      .getByRole("button", { name: "Rolling upgrade (1)", exact: true })
      .click();
    await expect(page.locator("#rollout-target")).toHaveText(target);
    await page
      .locator("#rollout-reason")
      .fill("Business test: browser rolling upgrade");
    const starting = page.waitForResponse(
      (response) =>
        response.url().endsWith("/agent-rollouts") &&
        response.request().method() === "POST",
    );
    await page
      .getByRole("button", { name: "Start rollout", exact: true })
      .click();
    const rolloutResponse = await starting;
    expect(rolloutResponse.status()).toBe(201);
    expect(rolloutResponse.request().headers()["idempotency-key"]).toBeTruthy();
    const created = await rolloutResponse.json();
    await expect(page).toHaveURL(new RegExp(`/rollouts/${created.id}$`));
    await expect(page.getByTestId("rollout-state")).toHaveText("Succeeded", {
      timeout: 900000,
    });
    await page.screenshot({
      path: `${process.env.ARTIFACT_DIR}/browser-rollout.png`,
      fullPage: true,
    });
    observations.push({
      name: "browser_agent_rollout",
      rollout: {
        id: created.id,
        target_version: created.target_version,
      },
    });
  } else {
    await page.goto(`/nodes/${node}`);
    await expect(
      page.getByRole("heading", {
        name: "business-test-agent",
        level: 1,
        exact: true,
      }),
    ).toBeVisible();
    const denied = await context.request.get("/api/v1/nodes", {
      headers: { "X-Workspace-ID": "00000000-0000-7000-8000-000000000072" },
    });
    expect(denied.status()).toBe(403);
    const password = crypto.randomBytes(24).toString("hex");
    fs.writeFileSync(`${work}/private/browser-vpn-password`, password, {
      mode: 0o600,
    });
    await page.getByTitle("Create user", { exact: true }).click();
    await page
      .getByLabel("User", { exact: true })
      .fill("business-test-browser-user");
    await page.getByLabel("Password", { exact: true }).fill(password);
    await page
      .getByLabel("Reason", { exact: true })
      .fill("Business test: browser user");
    const created = page.waitForResponse(
      (response) =>
        response.url().endsWith(`/nodes/${node}/users`) &&
        response.request().method() === "POST",
    );
    await page.getByRole("button", { name: "Confirm", exact: true }).click();
    const createResponse = await created;
    expect(createResponse.status()).toBe(202);
    const operation = await createResponse.json();
    await expect(page.getByTestId("operation-state")).toHaveText("Succeeded", {
      timeout: 120000,
    });
    observations.push({ name: "browser_user_mutation", operation });
    await page.reload();
    await expect(
      page.getByText("business-test-browser-user", { exact: true }),
    ).toBeVisible();
    await page.screenshot({
      path: `${process.env.ARTIFACT_DIR}/browser-node.png`,
      fullPage: true,
    });

    await page.getByTitle("Reload Ocserv", { exact: true }).click();
    await page
      .getByLabel("Reason", { exact: true })
      .fill("Business test: browser direct Admin reload");
    const reloaded = page.waitForResponse(
      (response) =>
        response.url().endsWith(`/nodes/${node}/service:reload`) &&
        response.request().method() === "POST",
    );
    await page.getByRole("button", { name: "Confirm", exact: true }).click();
    const reloadResponse = await reloaded;
    expect(reloadResponse.status()).toBe(202);
    const reload = await reloadResponse.json();
    await expect(page.getByTestId("operation-state")).toHaveText("Succeeded", {
      timeout: 120000,
    });
    observations.push({
      name: "browser_direct_admin_reload",
      operation: reload,
    });
    const reloadRequest = reloadResponse.request();
    const reloadHeaders = await reloadRequest.allHeaders();
    const replayHeaders = {
      ...headers,
      "Idempotency-Key": reloadHeaders["idempotency-key"],
      "If-Match": reloadHeaders["if-match"],
    };
    const knownReplay = await context.request.post(reloadRequest.url(), {
      headers: replayHeaders,
      data: reloadRequest.postDataJSON(),
    });
    expect(knownReplay.status()).toBe(202);
    const replayed = await knownReplay.json();
    expect(replayed.id).toBe(reload.id);
    expect(replayed.command_id).toBe(reload.command_id);
    const changed = await context.request.post(reloadRequest.url(), {
      headers: replayHeaders,
      data: {
        ...reloadRequest.postDataJSON(),
        reason: "Business test: changed replay intent",
      },
    });
    expect(changed.status()).toBe(409);
    expect((await changed.json()).type).toBe(
      "https://ocservia.dev/problems/idempotency-conflict",
    );
    observations.push({
      name: "browser_idempotency_rejects_changed_intent",
      operation_id: reload.id,
    });

    const reference = JSON.parse(
      fs.readFileSync(`${work}/config-reference.json`, "utf8"),
    );
    await page.getByTitle("Configuration plan", { exact: true }).click();
    const configDialog = page.getByRole("dialog", {
      name: "Configuration plan",
      exact: true,
    });
    const configResult = configDialog.getByTestId("config-plan-result");
    await page.getByLabel("TCP port", { exact: true }).fill("44443");
    await page
      .getByLabel("IPv4 network (CIDR)", { exact: true })
      .fill("10.208.0.0/24");
    await page
      .getByLabel("Certificate reference", { exact: true })
      .fill(reference.id);
    await page
      .getByLabel("Private key reference", { exact: true })
      .fill(reference.id);
    await page
      .getByLabel("Reason", { exact: true })
      .fill("Business test: browser complete configuration");
    const planning = page.waitForResponse(
      (response) =>
        response.url().endsWith(`/nodes/${node}/config-plans`) &&
        response.request().method() === "POST",
    );
    await configDialog
      .getByRole("button", { name: "Plan", exact: true })
      .click();
    const planResponse = await planning;
    expect(planResponse.status()).toBe(202);
    let plan = await planResponse.json();
    await expect(configResult.getByText("valid", { exact: true })).toBeVisible({
      timeout: 120000,
    });
    plan = await (
      await context.request.get(`/api/v1/config-plans/${plan.id}`, { headers })
    ).json();
    expect(plan.materialized_hash).toMatch(/^[0-9a-f]{64}$/);
    expect(plan.materialized_hash).not.toBe(plan.candidate_hash);
    expect(plan.warnings).toEqual([]);
    expect(plan.current_unchanged && plan.staging_cleaned).toBe(true);
    await configDialog.evaluate((dialog) => {
      dialog.scrollTop = 0;
    });
    await page.screenshot({
      path: `${process.env.ARTIFACT_DIR}/browser-config-plan.png`,
    });
    await configResult.scrollIntoViewIfNeeded();
    await page.screenshot({
      path: `${process.env.ARTIFACT_DIR}/browser-config-result.png`,
    });
    await page.setViewportSize({ width: 390, height: 844 });
    await expect
      .poll(() =>
        page.evaluate(
          () => document.documentElement.scrollWidth <= window.innerWidth,
        ),
      )
      .toBe(true);
    await configDialog.evaluate((dialog) => {
      dialog.scrollTop = 0;
    });
    await page.screenshot({
      path: `${process.env.ARTIFACT_DIR}/browser-config-plan-mobile.png`,
    });
    await configResult.scrollIntoViewIfNeeded();
    await page.screenshot({
      path: `${process.env.ARTIFACT_DIR}/browser-config-result-mobile.png`,
    });
    await page.setViewportSize({ width: 1280, height: 720 });
    await page
      .locator("#config-apply-reason")
      .fill("Business test: browser exact materialization");
    const applying = page.waitForResponse(
      (response) =>
        response.url().endsWith(`/config-plans/${plan.id}/apply`) &&
        response.request().method() === "POST",
    );
    await configDialog
      .getByRole("button", { name: "Apply", exact: true })
      .click();
    const applyResponse = await applying;
    expect(applyResponse.status()).toBe(202);
    const configOperation = await applyResponse.json();
    await expect(page.getByTestId("operation-state")).toHaveText("Succeeded", {
      timeout: 120000,
    });
    await expect
      .poll(
        async () =>
          (
            await (
              await context.request.get(`/api/v1/nodes/${node}`, { headers })
            ).json()
          ).config_revision,
        { timeout: 120000 },
      )
      .toBe(1);
    observations.push({
      name: "browser_complete_config_apply",
      operation: configOperation,
      plan_id: plan.id,
      candidate_hash: plan.candidate_hash,
      materialized_hash: plan.materialized_hash,
    });
    await page.reload();

    observations.push({
      name: "browser_login_workspace_forbidden_refresh",
      status: "PASS",
    });

    if (profile === "extended") {
      await page.getByTitle("Certificate lifecycle", { exact: true }).click();
      await page.getByLabel("Common name").fill("business-test-browser-client");
      await page
        .getByLabel("Reason", { exact: true })
        .fill("Business test: browser CSR");
      const csr = page.waitForResponse(
        (response) =>
          response.url().endsWith(`/nodes/${node}/certificates`) &&
          response.request().method() === "POST",
      );
      await page
        .getByRole("button", { name: "Request CSR", exact: true })
        .click();
      const csrResponse = await csr;
      expect(csrResponse.status()).toBe(202);
      const certificate = await csrResponse.json();
      await expect(page.getByText("csr_ready", { exact: true })).toBeVisible({
        timeout: 120000,
      });
      observations.push({
        name: "browser_certificate_csr",
        certificate_id: certificate.id,
        operation_id: certificate.operation_id,
      });
      await page.getByRole("button", { name: "Cancel", exact: true }).click();
      await page.reload();
      await page.getByTitle("Certificate lifecycle", { exact: true }).click();
      await expect(page.getByText("csr_ready", { exact: true })).toBeVisible();
      await page.screenshot({
        path: `${process.env.ARTIFACT_DIR}/browser-certificate.png`,
        fullPage: true,
      });
      await page
        .getByLabel("Reason", { exact: true })
        .fill("Business test: browser issue");
      const issuing = page.waitForResponse((response) =>
        response.url().endsWith(`/certificates/${certificate.id}:issue`),
      );
      await page
        .getByRole("button", { name: "Issue certificate", exact: true })
        .click();
      const issueResponse = await issuing;
      expect(issueResponse.status()).toBe(200);
      let issued = await issueResponse.json();
      await expect(page.getByText("issued", { exact: true })).toBeVisible();
      await expect(page.getByLabel("Approval ID")).toHaveCount(0);
      await expect
        .poll(
          async () => {
            const current = await context.request.get(
              `/api/v1/certificates/${certificate.id}`,
              { headers },
            );
            expect(current.status()).toBe(200);
            issued = await current.json();
            return issued.state;
          },
          { timeout: 120000 },
        )
        .toBe("expiring");
      await page.getByRole("button", { name: "Cancel", exact: true }).click();
      await page.getByTitle("Certificate lifecycle", { exact: true }).click();
      await expect(page.getByText("expiring", { exact: true })).toBeVisible();
      const reason = "Business test: browser export";
      await page.getByLabel("Reason", { exact: true }).fill(reason);
      const exporting = page.waitForResponse((response) =>
        response.url().endsWith(`/certificates/${certificate.id}:p12`),
      );
      await page
        .getByRole("button", { name: "Create P12", exact: true })
        .click();
      const exportResponse = await exporting;
      expect(exportResponse.status()).toBe(202);
      expect(
        exportResponse.request().postDataJSON().artifact_request_id,
      ).toMatch(
        /^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/,
      );
      const grant = await exportResponse.json();
      fs.writeFileSync(`${work}/private/browser-p12-password`, grant.password, {
        mode: 0o600,
      });
      fs.writeFileSync(
        `${work}/private/browser-p12-token`,
        grant.download_token,
        { mode: 0o600 },
      );
      await expect(
        page.getByRole("button", { name: "Download", exact: true }),
      ).toBeEnabled({ timeout: 120000 });
      const downloadPromise = page.waitForEvent("download");
      await page.getByRole("button", { name: "Download", exact: true }).click();
      await (await downloadPromise).saveAs(`${work}/private/browser.p12`);
      execFileSync(
        "openssl",
        [
          "pkcs12",
          "-in",
          `${work}/private/browser.p12`,
          "-passin",
          `file:${work}/private/browser-p12-password`,
          "-noout",
        ],
        { stdio: "pipe" },
      );
      expect(
        (
          await context.request.get(`/api/v1/artifacts/${grant.artifact_id}`, {
            headers: { ...headers, "X-Artifact-Token": grant.download_token },
          })
        ).status(),
      ).toBe(403);
      observations.push({
        name: "browser_certificate_issue_export_one_use",
        certificate_id: issued.id,
        operation: grant.operation,
      });
      const revokeReason = "Business test: browser revoke";
      await page.getByLabel("Reason", { exact: true }).fill(revokeReason);
      const revoking = page.waitForResponse((response) =>
        response.url().endsWith(`/certificates/${certificate.id}:revoke`),
      );
      await page.getByRole("button", { name: "Revoke", exact: true }).click();
      const revokeResponse = await revoking;
      expect(revokeResponse.status()).toBe(202);
      observations.push({
        name: "browser_certificate_revoke",
        operation: await revokeResponse.json(),
      });
      await expect(page.getByText("revoked", { exact: true })).toBeVisible({
        timeout: 120000,
      });
    }
  }

  // Positive control in its own context: a deliberate image and inline script
  // must be observed, proving that zero business violations is a real result.
  const controlContext = await browser.newContext({
    baseURL: origin,
    locale: "en-US",
  });
  await collectCsp(controlContext, "control");
  const controlPage = await controlContext.newPage();
  await controlPage.goto("/login");
  await controlPage.evaluate(() => {
    const image = new Image();
    image.src = "https://csp-control.invalid/pixel.png";
    document.body.append(image);
    const script = document.createElement("script");
    script.textContent = "window.__ocserviaCspControl = true;";
    document.head.append(script);
  });
  const control = () =>
    csp.violations.filter((violation) => violation.phase === "control");
  await expect
    .poll(() =>
      ["img-src", "script-src-elem"].every((directive) =>
        control().some((violation) => violation.directive === directive),
      ),
    )
    .toBe(true);
  // Closed contexts report nothing more, so the counts below are final.
  await Promise.all([context.close(), controlContext.close()]);
  const flow = (list) => list.filter((entry) => entry.phase === "flow").length;
  observations.push({
    name: "browser_csp",
    flow_violations: flow(csp.violations),
    flow_console_reports: flow(csp.console),
    flow_page_errors: flow(csp.page_errors),
    control: control().map(
      (violation) => `${violation.directive}/${violation.disposition}`,
    ),
  });
} finally {
  for (const entry of csp.violations)
    Object.assign(entry, {
      blocked: redact(entry.blocked),
      source: redact(entry.source),
    });
  const evidence = rollout ? "browser-rollout" : "browser";
  fs.writeFileSync(
    `${process.env.ARTIFACT_DIR}/${evidence}-csp.json`,
    JSON.stringify(csp),
  );
  fs.writeFileSync(
    `${process.env.ARTIFACT_DIR}/${evidence}-checkpoints.json`,
    JSON.stringify(observations),
  );
  await browser.close();
}
