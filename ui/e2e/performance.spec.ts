import fs from "node:fs/promises";
import { expect, test, type Page } from "@playwright/test";
import {
  applyDefinitions,
  failOnUnexpectedPageErrors,
  uniqueSuffix,
  type FixtureDefinition,
} from "./helpers/fixtures";

failOnUnexpectedPageErrors();

/**
 * Live-backend browser performance coverage for E3.
 *
 * These tests measure route readiness, action-to-render, and long-session
 * memory against the real server. They assert correctness (the page became
 * ready, the action rendered, the heap did not explode). They do not encode
 * calibrated SLOs — E4 owns budgets.json.
 *
 * Tests that intercept HTTP are labelled SYNTHETIC. Everything else is live.
 * Chromium-only, matching D2/Q4: this file does not invent Firefox/WebKit
 * projects or skip rules for browsers the suite does not run.
 */

const PRIMARY_ROUTES: { path: string; heading: RegExp }[] = [
  { path: "/jobs", heading: /^Jobs$/ },
  { path: "/triggers", heading: /^Triggers$/ },
  { path: "/system", heading: /^System$/ },
  { path: "/jobdefs", heading: /^Job Definitions$/ },
];

async function record(metric: string, value: number, extra: Record<string, unknown> = {}): Promise<void> {
  const dest = process.env.CAESIUM_PERF_BROWSER_OUT;
  if (!dest) return;
  await fs.appendFile(
    dest,
    `${JSON.stringify({ metric, value, at: new Date().toISOString(), ...extra })}\n`,
  );
}

async function heapUsed(page: Page): Promise<number | null> {
  return page.evaluate(() => {
    const mem = (performance as Performance & { memory?: { usedJSHeapSize: number } }).memory;
    return mem && Number.isFinite(mem.usedJSHeapSize) ? mem.usedJSHeapSize : null;
  });
}

test("live route readiness: primary pages reach their heading", async ({ page }) => {
  for (const route of PRIMARY_ROUTES) {
    const started = Date.now();
    await page.goto(route.path);
    await expect(page.getByRole("heading", { name: route.heading })).toBeVisible();
    const readyMs = Date.now() - started;
    expect(readyMs, `${route.path} never became ready`).toBeGreaterThan(0);
    await record("route_readiness_ms", readyMs, { route: route.path, kind: "live" });
  }
});

test("live action-to-render: triggering a run paints the run heading", async ({ page, request }) => {
  test.slow();

  const alias = `perf-action-${uniqueSuffix()}`;
  const definition = {
    apiVersion: "v1",
    kind: "Job",
    metadata: { alias },
    trigger: { type: "cron", configuration: { cron: "0 0 1 1 *" } },
    steps: [{ name: "noop", image: "alpine:3.23", command: ["sh", "-c", "true"] }],
  } as unknown as FixtureDefinition;
  await applyDefinitions(request, definition);

  await page.goto("/jobs");
  await expect(page.getByRole("heading", { name: "Jobs", exact: true })).toBeVisible();
  const row = page.locator('[data-testid="job-row"]', { hasText: alias }).first();
  await expect(row).toBeVisible();

  const started = Date.now();
  await row.locator('button[title="Trigger run"]').click();
  await page.waitForURL(/\/jobs\/[^/]+\/runs\/[^/]+$/);
  await expect(page.getByRole("heading", { name: /Run / })).toBeVisible();
  const renderMs = Date.now() - started;
  expect(renderMs).toBeGreaterThan(0);
  await record("action_to_render_ms", renderMs, { action: "trigger-run", kind: "live" });
});

test("live long-session memory stays bounded across repeated navigation", async ({ page }) => {
  await page.goto("/jobs");
  await expect(page.getByRole("heading", { name: "Jobs", exact: true })).toBeVisible();

  const first = await heapUsed(page);
  test.skip(first == null, "performance.memory is unavailable (Chromium-only capability)");

  const samples: number[] = [first];
  const cycle = ["/jobs", "/triggers", "/system", "/jobdefs"];
  for (let i = 0; i < 8; i++) {
    for (const path of cycle) {
      await page.goto(path);
      await expect(page.locator("h1").first()).toBeVisible();
    }
    const used = await heapUsed(page);
    expect(used).not.toBeNull();
    samples.push(used as number);
  }

  const last = samples[samples.length - 1];
  expect(last).toBeGreaterThan(0);
  // Smoke bound, not an SLO: an 8x climb across a short navigation loop is a
  // leak, not noise. Calibrated heap budgets belong in E4.
  expect(last / first).toBeLessThan(8);
  await record("long_session_heap_bytes", last, { first, kind: "live", samples: samples.length });
});

test("SYNTHETIC: a large jobs list still reaches a ready heading", async ({ page }) => {
  const now = new Date().toISOString();
  const jobs = Array.from({ length: 200 }, (_, i) => ({
    id: `00000000-0000-4000-8000-${String(i).padStart(12, "0")}`,
    alias: `perf-synth-${String(i).padStart(3, "0")}`,
    trigger_id: `00000000-0000-4000-8001-${String(i).padStart(12, "0")}`,
    labels: {},
    annotations: {},
    paused: false,
    created_at: now,
    updated_at: now,
  }));

  await page.route("**/v1/jobs", async (route) => {
    const url = new URL(route.request().url());
    if (url.pathname.replace(/\/$/, "") !== "/v1/jobs") {
      await route.continue();
      return;
    }
    if (route.request().method() !== "GET") {
      await route.continue();
      return;
    }
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(jobs),
    });
  });

  const started = Date.now();
  await page.goto("/jobs");
  await expect(page.getByRole("heading", { name: "Jobs", exact: true })).toBeVisible();
  await expect(page.getByTestId("job-row").first()).toBeVisible();
  const readyMs = Date.now() - started;
  expect(readyMs).toBeGreaterThan(0);
  const count = await page.getByTestId("job-row").count();
  expect(count).toBeGreaterThan(0);
  await record("route_readiness_ms", readyMs, { route: "/jobs", kind: "synthetic", rows: count });
});
