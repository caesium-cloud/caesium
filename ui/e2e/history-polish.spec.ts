import { expect, test } from "@playwright/test";
import type { Job, JobRun } from "../src/lib/api";
import { applyDefinitions, awaitRun, failOnUnexpectedPageErrors, findJobByAlias, triggerJob, uniqueSuffix } from "./helpers/fixtures";

failOnUnexpectedPageErrors();
test.use({ viewport: { width: 1440, height: 900 } });

test("archive history links reach real executions; SYNTHETIC ages exercise aligned history rows", async ({ page, request }, testInfo) => {
  const alias = `history-layout-${uniqueSuffix()}`;
  await applyDefinitions(request, {
    apiVersion: "v1", kind: "Job", metadata: { alias },
    trigger: { type: "cron", configuration: { cron: "0 0 1 1 *" } },
    steps: [{ name: "check-history", engine: process.env.CAESIUM_E2E_ENGINE || "docker", image: "alpine:3.23", cache: false, command: ["echo", "history"] }],
  });
  const job = await findJobByAlias(request, alias);
  await triggerJob(request, job.id);
  const first = await awaitRun(request, job.id, { status: "succeeded" });
  await triggerJob(request, job.id);
  const latest = await awaitRun(request, job.id, { status: "succeeded" });
  expect(latest.id).not.toBe(first.id);
  // Age only the browser responses; server executions and exact IDs stay real.
  const browserNow = await page.evaluate(() => Date.now());
  await page.clock.setFixedTime(browserNow);
  const age = (run: JobRun) => {
    const started = browserNow - (run.id === latest.id ? 40 : 60) * 60_000 - 30_000;
    return { ...run, started_at: new Date(started).toISOString(), completed_at: new Date(started + 15_000).toISOString() };
  };
  await page.route("**/v1/jobs", async route => {
    const response = await route.fetch();
    const jobs: Job[] = await response.json();
    await route.fulfill({ response, json: jobs.map(item => item.id === job.id ? { ...item, latest_run: age(latest), last_runs: [age(first), age(latest)].map(run => ({ status: run.status, started_at: run.started_at, duration: 15 })) } : item) });
  });
  // Keep the cheap latest-run projection consistent with the aged history,
  // and exclude unrelated retained events from this presentation fixture.
  await page.route(`**/v1/jobs/${job.id}`, async route => {
    const response = await route.fetch();
    await route.fulfill({ response, json: { ...await response.json(), latest_run: age(latest) } });
  });
  await page.route("**/v1/events?**", route => {
    const url = new URL(route.request().url());
    url.searchParams.set("types", "history_layout_fixture");
    return route.continue({ url: url.toString() });
  });
  await page.route(`**/v1/jobs/${job.id}/runs?*`, async route => {
    const response = await route.fetch();
    const runs: JobRun[] = await response.json();
    await route.fulfill({ response, json: runs.map(age) });
  });
  for (const theme of ["dark", "light"]) for (const [width, height] of [[1440, 900], [1280, 800], [390, 844]]) {
    await page.addInitScript(value => localStorage.setItem("caesium-ui-theme", value), theme);
    await page.setViewportSize({ width, height });
    await page.goto(`/jobs?q=${alias}`);
    const strip = page.getByTestId("job-row").locator('[data-history="archived"]');
    await expect(strip).toContainText("Outside 15m");
    const inset = await strip.evaluate(el => Math.min(...[...el.querySelectorAll("[data-status]")].map(mark => mark.getBoundingClientRect().top - el.getBoundingClientRect().top)));
    expect(inset).toBeGreaterThanOrEqual(12);
    await page.getByLabel("History window").selectOption("86400");
    const recent = page.getByTestId("job-row").locator('[data-history="recent"]');
    await expect(recent).toBeVisible();
    expect(await recent.evaluate(el => Math.min(...[...el.querySelectorAll("[data-status]")].map(mark => mark.getBoundingClientRect().top - el.getBoundingClientRect().top)))).toBeGreaterThanOrEqual(12);
    await page.getByLabel("History window").selectOption("900");
    const link = strip.getByRole("link", { name: `View run history for ${alias}` });
    await expect(link).toHaveAttribute("href", `/jobs/${job.id}/runs`);
    await link.focus(); await page.keyboard.press("Enter");
    const dialog = page.getByRole("dialog", { name: "Run History" });
    await expect(dialog).toBeVisible();
    await dialog.evaluate(el => Promise.all(el.getAnimations().map(animation => animation.finished)));
    const rows = page.getByTestId("run-history-row");
    await expect(rows).toHaveCount(2);
    await expect(rows.first()).toContainText("40m ago");
    await expect(rows.last()).toContainText("1h ago");
    for (const cell of ["status", "duration"]) {
      const positions = await rows.locator(`[data-history-cell="${cell}"]`).evaluateAll(nodes => nodes.map(node => node.getBoundingClientRect().x));
      expect(Math.abs(positions[0] - positions[1])).toBeLessThan(1);
    }
    expect(await dialog.evaluate(el => el.scrollWidth - el.clientWidth)).toBeLessThanOrEqual(1);
    await expect(rows.first().getByRole("link", { name: `Open run ${latest.id}` })).toBeInViewport();
    await page.screenshot({ path: testInfo.outputPath(`history-${theme}-${width}.png`) });
    await rows.last().getByRole("link", { name: `Open run ${first.id}` }).click();
    await expect(page).toHaveURL(`/jobs/${job.id}/runs/${first.id}`);
    await expect(page.getByTestId("run-identity")).toContainText(first.id.slice(0, 8));
  }
  await page.unrouteAll({ behavior: "wait" });
});

test("SYNTHETIC dataset counts distinguish zero, a paginated total, and unavailable data", async ({ page }) => {
  let enabled = true;
  await page.route("**/v1/system/features", async route => {
    const response = await route.fetch();
    await route.fulfill({ response, json: { ...await response.json(), freshness_enabled: enabled, data_assertions_enabled: true } });
  });
  let total = 0;
  let unavailable = false;
  await page.route("**/v1/datasets?*", route => {
    expect(new URL(route.request().url()).searchParams.get("limit")).toBe("1");
    return route.fulfill({ status: unavailable ? 403 : 200, json: unavailable ? { message: "Forbidden" } : { datasets: [], total, limit: 1, offset: 0 } });
  });
  await page.route("**/v1/datasets/holds?*", route => route.fulfill({ json: { holds: [], total: 0, limit: 1, offset: 0 } }));
  await page.goto("/jobs");
  const datasets = page.locator('aside nav a[href="/datasets"] [data-nav-count]');
  const holds = page.locator('aside nav a[href="/datasets/holds"] [data-nav-count]');
  await expect(datasets).toHaveText("0"); await expect(holds).toHaveText("0");
  total = 42; await page.reload(); await expect(datasets).toHaveText("42");
  unavailable = true;
  await Promise.all([page.waitForResponse(response => new URL(response.url()).pathname === "/v1/datasets" && response.status() === 403), page.reload()]);
  await expect(datasets).toBeEmpty();
  await expect(holds).toHaveText("0");
  enabled = false;
  await page.reload();
  await expect(holds).toHaveText("0");
  await expect(page.locator('aside nav a[href="/datasets"]')).toHaveCount(0);
  await page.unrouteAll({ behavior: "wait" });
});
