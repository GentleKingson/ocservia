import { defineConfig, devices } from "@playwright/test";

const parsedWorkers = Number(process.env.PLAYWRIGHT_WORKERS);
const workers =
  Number.isInteger(parsedWorkers) && parsedWorkers > 0 ? parsedWorkers : 1;

export default defineConfig({
  testDir: "./e2e",
  outputDir: process.env.PLAYWRIGHT_OUTPUT_DIR ?? "test-results",
  reporter: "line",
  timeout: 30_000,
  expect: { timeout: 10_000 },
  retries: 0,
  workers,
  projects: [
    {
      name: "desktop",
      use: { ...devices["Desktop Chrome"], browserName: "chromium" },
    },
  ],
  use: {
    baseURL: process.env.PLAYWRIGHT_BASE_URL ?? "http://127.0.0.1:4173",
    screenshot: "only-on-failure",
    trace: "off",
    video: "off",
  },
});
