import { defineConfig, devices } from "@playwright/test";

// Specs that also run in Firefox and WebKit: login/Workspace, rollout
// lifecycle, dialogs, form focus and keyboard navigation. Playwright's patched
// engines are not the supported Safari/Firefox releases themselves.
const crossEngineSpecs =
  /(^|\/)(login|auth-workspace|agent-rollout|node-forms|user-password|shell-navigation)\.spec\.ts$/;

export default defineConfig({
  testDir: "./e2e",
  outputDir: process.env.PLAYWRIGHT_OUTPUT_DIR ?? "test-results",
  reporter: process.env.PLAYWRIGHT_HTML_OUTPUT_DIR
    ? [
        ["line"],
        [
          "html",
          {
            outputFolder: process.env.PLAYWRIGHT_HTML_OUTPUT_DIR,
            open: "never",
          },
        ],
      ]
    : "line",
  timeout: 30_000,
  expect: { timeout: 10_000 },
  retries: 0,
  workers: 1,
  // Each project names its engine; a global browserName would override them.
  projects: [
    {
      name: "desktop",
      use: { ...devices["Desktop Chrome"], browserName: "chromium" },
    },
    // iPhone 13 viewport, touch and user agent in Chromium: not Safari coverage.
    {
      name: "mobile",
      use: { ...devices["iPhone 13"], browserName: "chromium" },
    },
    {
      name: "firefox",
      testMatch: crossEngineSpecs,
      use: { ...devices["Desktop Firefox"], browserName: "firefox" },
    },
    {
      name: "webkit",
      testMatch: crossEngineSpecs,
      use: { ...devices["Desktop Safari"], browserName: "webkit" },
    },
  ],
  use: {
    baseURL: process.env.PLAYWRIGHT_BASE_URL ?? "http://127.0.0.1:4173",
    screenshot: "only-on-failure",
    trace: "retain-on-failure",
    video: "retain-on-failure",
  },
});
