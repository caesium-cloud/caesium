import { defineConfig } from "@playwright/test";

export default defineConfig({
  testDir: "./e2e",
  timeout: 120_000,
  expect: {
    timeout: 15_000,
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
      testIgnore: "**/auth/**/*.spec.ts",
    },
    {
      name: "auth",
      testMatch: "**/auth/**/*.spec.ts",
    },
  ],
});
