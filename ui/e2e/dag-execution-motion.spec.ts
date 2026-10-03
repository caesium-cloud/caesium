import { expect, test } from "@playwright/test";
import { applyDefinitions, awaitRun, failOnUnexpectedPageErrors, findJobByAlias, triggerJob, uniqueSuffix } from "./helpers/fixtures";

failOnUnexpectedPageErrors();
test.use({ viewport: { width: 1440, height: 900 }, reducedMotion: "no-preference" });

test("live execution advances between clock ticks and stops without reloading", async ({ page, request }, testInfo) => {
  const alias = `dag-motion-${uniqueSuffix()}`;
  await applyDefinitions(request, {
    apiVersion: "v1", kind: "Job", metadata: { alias },
    trigger: { type: "cron", configuration: { cron: "0 0 1 1 *" } },
    steps: [
      { name: "prepare", image: "alpine:3.23", engine: process.env.CAESIUM_E2E_ENGINE || "docker", cache: false, command: ["echo", "ready"] },
      { name: "process", image: "alpine:3.23", engine: process.env.CAESIUM_E2E_ENGINE || "docker", cache: false, command: ["sh", "-c", "sleep 24; echo done"] },
      { name: "finish", image: "alpine:3.23", engine: process.env.CAESIUM_E2E_ENGINE || "docker", cache: false, command: ["echo", "finished"] },
    ],
  });
  const job = await findJobByAlias(request, alias);
  await triggerJob(request, job.id);
  const run = await awaitRun(request, job.id, { status: "running" });
  // Simulate a connected event stream which misses the terminal message. REST
  // reconciliation must still end the live UI, as can happen across replicas.
  await page.route("**/v1/events?**", route => route.fulfill({ contentType: "text/event-stream", body: ": connected\n\n" }));
  await page.goto(`/jobs/${job.id}/runs/${run.id}`);
  const bar = page.locator('[data-testid="run-timeline-bar"].cs-live-bar');
  await expect(bar).toBeVisible();
  const samples = await bar.evaluate(async element => {
    const points: { width: number; time: number }[] = [];
    for (let i = 0; i < 24; i++) {
      await new Promise(requestAnimationFrame);
      points.push({ width: element.getBoundingClientRect().width, time: performance.now() });
    }
    return points;
  });
  // A 1Hz jump yields only one or two widths here. Require frame-level motion.
  expect(new Set(samples.map(point => point.width)).size).toBeGreaterThan(12);
  await testInfo.attach("frame-samples", { body: JSON.stringify(samples), contentType: "application/json" });
  await page.screenshot({ path: testInfo.outputPath("live.png") });
  const transform = await page.locator(".react-flow__viewport").getAttribute("style");
  await page.emulateMedia({ reducedMotion: "reduce" });
  await expect.poll(() => page.locator(".cs-electron-running, .cs-edge-running .react-flow__edge-path, .cs-live-bar").evaluateAll(nodes => nodes.every(node => node.getAnimations().length === 0))).toBe(true);
  await page.emulateMedia({ reducedMotion: "no-preference" });
  await awaitRun(request, job.id, { status: "succeeded" });
  await expect(page.getByTestId("run-timeline-now")).toHaveCount(0);
  await expect(page.locator(".cs-electron-running, .cs-edge-running, [data-testid=run-timeline-bar].cs-live-bar")).toHaveCount(0);
  await expect(page.locator(".react-flow__viewport")).toHaveAttribute("style", transform!);
  await page.screenshot({ path: testInfo.outputPath("terminal.png") });

  // Rendering regression: preserve an incomplete task snapshot after the
  // parent failed. This is deliberately synthetic; no server rows are edited.
  const response = await request.get(`/v1/jobs/${job.id}/runs/${run.id}`);
  const snapshot = await response.json();
  snapshot.status = "failed";
  snapshot.error = "database is locked";
  snapshot.tasks[1].status = "running";
  snapshot.tasks[1].completed_at = undefined;
  snapshot.tasks[2].status = "pending";
  snapshot.tasks[2].started_at = undefined;
  snapshot.tasks[2].completed_at = undefined;
  await page.route(`**/v1/jobs/${job.id}/runs/${run.id}`, route => route.fulfill({ json: snapshot }));
  await page.reload();
  await expect(page.getByTestId("dag-counters")).toContainText("1 unconfirmed");
  await expect(page.getByTestId("dag-counters")).toContainText("1 not started");
  await expect(page.getByLabel("Outcome unknown", { exact: true })).toHaveCount(1);
  await expect(page.locator(".cs-electron-running, .cs-edge-running, [data-testid=run-timeline-bar].cs-live-bar")).toHaveCount(0);
  const row = page.getByTestId("run-timeline-task-row").filter({ hasText: "process" });
  await expect(row).toContainText("observed");
  const node = page.locator(`.react-flow__node[data-id="${snapshot.tasks[1].task_id}"]`);
  await node.click();
  await expect(page.getByTestId("task-detail-panel")).toContainText("Outcome unknown");
  await expect(page.getByTestId("task-detail-panel")).toContainText("Last reported task state: running");
  await page.getByRole("button", { name: "Close task panel" }).click();
  for (const theme of ["dark", "light"]) {
    await page.evaluate(value => { localStorage.setItem("caesium-ui-theme", value); }, theme);
    await page.reload();
    for (const [width, height] of [[1440, 900], [1280, 800], [390, 844]]) {
      await page.setViewportSize({ width, height });
      await expect(page.getByTestId("run-timeline-task-row").first()).toBeVisible();
      expect(await page.locator("main").evaluate(el => el.scrollWidth - el.clientWidth)).toBeLessThanOrEqual(1);
      const ticks = page.getByTestId("timeline-tick");
      const last = await ticks.last().boundingBox();
      const chart = await ticks.last().locator("..").boundingBox();
      expect(last!.x + last!.width).toBeLessThanOrEqual(chart!.x + chart!.width + 1);
      await page.screenshot({ path: testInfo.outputPath(`incomplete-${theme}-${width}.png`) });
    }
    await page.goto(`/jobs/${job.id}`);
    await expect(page.getByTestId("run-task-state-notice")).toContainText("database is locked");
    await expect(page.getByTestId("dag-counters")).toContainText("1 unconfirmed");
    await expect(page.locator(".cs-electron-running, .cs-edge-running")).toHaveCount(0);
    for (const [width, height] of [[1440, 900], [390, 844]]) {
      await page.setViewportSize({ width, height });
      await page.screenshot({ path: testInfo.outputPath(`incomplete-job-${theme}-${width}.png`) });
    }
    await page.goto(`/jobs/${job.id}/runs/${run.id}`);
  }
});
