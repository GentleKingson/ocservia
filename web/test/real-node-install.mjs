// Hosted acceptance only; uses Playwright's library API, never test reporters,
// screenshots, traces, video, storageState files or mocked HTTP responses.
import { chromium } from "@playwright/test";
import { createHash } from "node:crypto";
import { mkdir, readFile, writeFile } from "node:fs/promises";
import path from "node:path";
import process from "node:process";
import console from "node:console";
import { URL } from "node:url";

const work = process.env.OCSERVIA_BUSINESS_TEST_WORK_DIR;
const workspace = process.env.OCSERVIA_BUSINESS_TEST_WORKSPACE_ID;
const phase = process.argv[2];
const read = async (file) => (await readFile(file, "utf8")).trim();
function check(condition) {
  if (!condition) throw new Error("node-install contract failed");
}
async function main() {
  check(process.env.NODE_INSTALL_E2E === "true");
  check(phase === "prepare" || phase === "issue");
  check(!process.env.DEBUG && !process.env.PWDEBUG);
  const state = path.join(work, "state");
  if (phase === "issue") {
    // Persist before any request: rerunning this step cannot issue another token.
    await writeFile(
      path.join(state, "bootstrap-issuance-attempted"),
      "attempted\n",
      {
        flag: "wx",
        mode: 0o600,
      },
    );
  }
  const agent = path.join(
    process.env.RUNNER_TEMP,
    `ocservia-real-e2e-node-${process.env.RUN_ID}`,
  );
  const fields = {
    name: "github-hosted-node",
    environment: "development",
    release: "v0.0.0",
    controllerEndpointId: await read(
      path.join(state, "controller-endpoint-id"),
    ),
    relayUrl: await read(path.join(work, "relay/url")),
    bootstrapTokenFile: path.join(agent, "secrets/bootstrap-token"),
    relayAccessTokenFile: path.join(agent, "relay/token"),
    controllerCommandVerificationKeyFile: path.join(
      agent,
      "controller-ready/command.pub.pem",
    ),
  };
  process.env.TMPDIR = path.join(work, "private/browser");
  await mkdir(process.env.TMPDIR, { recursive: true, mode: 0o700 });
  const browser = await chromium.launch();
  try {
    // Disposable local test CA; HTTPS and the real secure cookie remain in use.
    const context = await browser.newContext({
      baseURL: "https://localhost:8443",
      ignoreHTTPSErrors: true,
      serviceWorkers: "block",
    });
    const page = await context.newPage();
    page.setDefaultTimeout(30_000);
    const posts = [];
    let logs = "";
    page.on("console", (message) => {
      logs += message.text();
    });
    page.on("request", (request) => {
      if (
        new URL(request.url()).pathname === "/api/v1/node-bootstrap-tokens" &&
        request.method() === "POST"
      ) {
        posts.push(request);
      }
    });
    await page.goto("/nodes");
    await page
      .getByLabel("Username", { exact: true })
      .fill("business-test-requester");
    await page
      .getByLabel("Password", { exact: true })
      .fill(await read(path.join(work, "private/requester-password")));
    await page.getByRole("button", { name: "Sign in", exact: true }).click();
    await page.getByRole("heading", { name: "Nodes", exact: true }).waitFor();
    // A single authorized Workspace is displayed as text, not a selector.
    const selector = page.locator("#workspace-select");
    if (await selector.count()) await selector.selectOption(workspace);
    await page
      .getByRole("button", { name: "Install node", exact: true })
      .click();
    await page
      .locator("#install-workspace")
      .filter({ hasText: workspace })
      .waitFor();
    for (const [key, value] of Object.entries(fields)) {
      await page.locator(`#install-${key}`).fill(value);
    }
    await page
      .getByRole("button", { name: "Prepare install.env", exact: true })
      .click();
    const config = await page
      .getByLabel("install.env (no secrets)", { exact: true })
      .inputValue();
    // An exact literal allowlist also excludes injected keys, expansions and secrets.
    const expected = `# Node: ${fields.name}\n# Workspace: ${workspace}\n# Release: ${fields.release}\nCONTROLLER_ENDPOINT_ID=${fields.controllerEndpointId}\nENROLLMENT_ENVIRONMENT=development\nRELAY_URL_A=${fields.relayUrl}\nRELAY_URL_B=\nBOOTSTRAP_TOKEN_SOURCE=${fields.bootstrapTokenFile}\nRELAY_ACCESS_TOKEN_SOURCE=${fields.relayAccessTokenFile}\nCONTROLLER_COMMAND_VERIFICATION_KEY_SOURCE=${fields.controllerCommandVerificationKeyFile}\n`;
    check(config === expected && posts.length === 0);
    const configPath = path.join(work, "outbox/controller-ready/install.env");
    if (phase === "prepare") {
      await writeFile(configPath, config, { flag: "wx", mode: 0o600 });
      return;
    }
    check(config === (await readFile(configPath, "utf8")));
    const responsePromise = page.waitForResponse(
      (response) =>
        new URL(response.url()).pathname === "/api/v1/node-bootstrap-tokens" &&
        response.request().method() === "POST",
    );
    // This click has no retry loop. HTTP failure or unknown result fails the job.
    const [response] = await Promise.all([
      responsePromise,
      page
        .getByRole("button", { name: "Create bootstrap token", exact: true })
        .click(),
    ]);
    check(response.status() === 201 && posts.length === 1);
    const request = posts[0];
    const body = request.postDataJSON();
    check(request.headers()["x-workspace-id"] === workspace);
    check(
      JSON.stringify(Object.keys(body).sort()) ===
        JSON.stringify([
          "environment",
          "expected_node_name",
          "reason",
          "ttl_seconds",
          "workspace_id",
        ]),
    );
    check(
      body.workspace_id === workspace &&
        body.ttl_seconds === 900 &&
        body.environment === fields.environment &&
        body.expected_node_name === fields.name,
    );
    const result = await response.json();
    check(/^obt1_[A-Za-z0-9_-]{43}$/.test(result.token));
    check(
      /^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(
        result.id,
      ),
    );
    const tokenField = page.getByLabel("Bootstrap token (shown once)", {
      exact: true,
    });
    await tokenField.waitFor();
    check((await tokenField.inputValue()) === result.token);
    check(
      await page
        .getByRole("button", { name: "Create bootstrap token", exact: true })
        .isDisabled(),
    );
    check(
      !config.includes(result.token) &&
        !logs.includes(result.token) &&
        !page.url().includes(result.token),
    );
    check(
      !(
        await page.evaluate(() =>
          JSON.stringify([globalThis.localStorage, globalThis.sessionStorage]),
        )
      ).includes(result.token),
    );
    const target = path.join(work, "outbox/enrollment-token");
    await mkdir(target, { recursive: true, mode: 0o700 });
    await writeFile(
      path.join(target, "enrollment-token"),
      result.token + "\n",
      { flag: "wx", mode: 0o600 },
    );
    await writeFile(path.join(state, "token-id"), result.id + "\n", {
      mode: 0o600,
    });
    await writeFile(
      path.join(state, "bootstrap-token-sha256"),
      createHash("sha256").update(result.token).digest("hex") + "\n",
      { mode: 0o600 },
    );
    await page
      .getByRole("button", {
        name: "Token saved — installation instructions",
        exact: true,
      })
      .click();
    check((await tokenField.count()) === 0 && posts.length === 1);
    const evidence = path.join(
      process.env.RUNNER_TEMP,
      "real-e2e-evidence-controller/node-install-ui.json",
    );
    await writeFile(
      evidence,
      JSON.stringify({
        login: true,
        config: true,
        bootstrap_posts: posts.length,
        workspace_match: true,
        ttl_seconds: 900,
        expected_endpoint_id_absent: true,
        token_cleared: true,
        release_asset_download: "NOT_TESTED",
      }) + "\n",
      { mode: 0o600 },
    );
  } finally {
    await browser.close();
  }
}
// Do not expose Playwright call logs, request bodies, DOM or cookies on failure.
main().catch(() => {
  console.error(
    "node-install browser acceptance failed; sensitive details withheld; do not retry issuance",
  );
  process.exitCode = 1;
});
