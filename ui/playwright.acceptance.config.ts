import { defineConfig, devices } from "@playwright/test";

// Vertical slice acceptance (WP23) against an installed all-in-one host; see docs/operators/all-in-one.md.
const baseURL = process.env.SAMUORI_ACCEPTANCE_URL;
if (!baseURL)
  throw new Error("set SAMUORI_ACCEPTANCE_URL to the installed Samuori");

export default defineConfig({
  testDir: "e2e/acceptance",
  fullyParallel: false,
  workers: 1,
  retries: 0,
  forbidOnly: true,
  timeout: 5 * 60_000,
  expect: { timeout: 30_000 },
  outputDir: process.env.SAMUORI_ACCEPTANCE_OUTPUT ?? "test-results",
  reporter: [["list"]],
  use: { baseURL, trace: "retain-on-failure", actionTimeout: 60_000 },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
});
