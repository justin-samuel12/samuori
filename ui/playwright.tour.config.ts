import { defineConfig } from "@playwright/test";
import { join } from "node:path";

// The screenshot tour against the throwaway server deploy/dev/screenshots.sh starts (docs/screenshots/README.md).
if (!process.env.SAMUORI_TOUR_URL || !process.env.SAMUORI_TOUR_DATA)
  throw new Error("run the tour with make screenshots");

export default defineConfig({
  testDir: "e2e/tour",
  workers: 1,
  retries: 0,
  forbidOnly: true,
  timeout: 60_000,
  expect: { timeout: 15_000 },
  outputDir: join(process.env.SAMUORI_TOUR_DATA, "test-results"),
  reporter: [["list"]],
  use: { browserName: "chromium", actionTimeout: 15_000 },
});
