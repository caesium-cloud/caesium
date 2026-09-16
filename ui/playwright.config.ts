import { defineConfig } from "@playwright/test";

export default defineConfig({
  testDir: "./e2e",
  timeout: 120_000,
  expect: {
    timeout: 15_000,
    toHaveScreenshot: {
      // Small tolerance for sub-pixel anti-aliasing noise between otherwise
      // identical Linux/Chromium renders — NOT a substitute for generating
      // baselines on the CI-equivalent platform (see ui/e2e/visual.spec.ts).
      maxDiffPixelRatio: 0.02,
      animations: "disabled",
    },
  },
  fullyParallel: false,
  // Retries collect diagnostics; recovery must not turn a CI failure green.
  retries: process.env.CI ? 2 : 0,
  failOnFlakyTests: !!process.env.CI,
  reporter: process.env.CI
    ? [
        ["line"],
        ["html", { outputFolder: "playwright-report", open: "never" }],
        ["json", { outputFile: "playwright-results.json" }],
      ]
    : "list",
  use: {
    baseURL: process.env.PLAYWRIGHT_BASE_URL || "http://127.0.0.1:8080",
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
    video: "retain-on-failure",
  },
  projects: [
    {
      name: "default",
      testIgnore: ["**/auth/**/*.spec.ts", "**/network-recovery.spec.ts"],
    },
    {
      // Run offline/reconnect tests last, in a separate worker/browser. On
      // Linux Chromium, a network-change event can affect neighboring pages.
      name: "network-recovery",
      testMatch: "**/network-recovery.spec.ts",
      dependencies: ["default"],
    },
    {
      name: "auth",
      testMatch: "**/auth/**/*.spec.ts",
    },
  ],
});
