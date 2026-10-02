import { expect, test } from "@playwright/test";
import { applyAndRun, applyDefinitions, awaitRun, failOnUnexpectedPageErrors, findJobByAlias, triggerJob, uniqueSuffix } from "./helpers/fixtures";

failOnUnexpectedPageErrors();
test.use({ viewport: { width: 1440, height: 900 }, reducedMotion: "no-preference" });

test("real run events update the fleet strip and shared instruments stop at completion", async ({ page, request }, testInfo) => {
  const alias = `motion-live-${uniqueSuffix()}`;
  await applyDefinitions(request, {
    apiVersion: "v1", kind: "Job", metadata: { alias },
    trigger: { type: "cron", configuration: { cron: "0 0 1 1 *" } },
    steps: [
      { name: "prepare", image: "alpine:3.23", cache: false, command: ["echo", "ready"] },
      { name: "process", image: "alpine:3.23", cache: false, command: ["sh", "-c", "sleep 20; echo processed"] },
      { name: "finish", image: "alpine:3.23", cache: false, command: ["echo", "done"] },
    ],
  });
  const job = await findJobByAlias(request, alias);
  await triggerJob(request, job.id);
  await awaitRun(request, job.id, { status: "succeeded" });
  await page.goto(`/jobs?q=${alias}`);
  const row = page.getByTestId("job-row");
  await expect(row.locator('[data-history="recent"] [data-status="succeeded"]')).toHaveCount(1);
  await page.getByLabel("History window").selectOption("3600");
  await expect(page).toHaveURL(/history=3600/);
  await page.reload();
  await expect(page.getByLabel("History window")).toHaveValue("3600");
  await page.getByLabel("History window").selectOption("900");

  // Trigger through the actual REST surface while Jobs stays open. No reload or mocked SSE.
  await triggerJob(request, job.id);
  const run = await awaitRun(request, job.id, { status: "running" });
  const liveStrip = row.locator('[data-history="recent"] .cs-live-bar');
  await expect(liveStrip).toBeVisible();
  await expect(row.locator(":scope > div").nth(3)).not.toHaveText("-");
  await expect(page.getByTestId("history-now-head")).toHaveCount(1);
  const elapsed = await liveStrip.innerText();
  await expect.poll(() => liveStrip.innerText()).not.toBe(elapsed);
  await page.screenshot({ path: testInfo.outputPath("fleet-live.png") });

  const detail = await page.context().newPage();
  await detail.goto(`/jobs/${job.id}/runs/${run.id}`);
  await expect(detail.locator('[data-testid="run-timeline-bar"].cs-live-bar')).toBeVisible();
  await expect(detail.getByTestId("run-timeline-now")).toHaveCount(1);
  const electron = detail.locator(".cs-electron-running");
  await expect(electron).toBeVisible();
  const position = await electron.evaluate(el => getComputedStyle(el).offsetDistance);
  await expect.poll(() => electron.evaluate(el => getComputedStyle(el).offsetDistance)).not.toBe(position);
  await expect.poll(() => detail.locator(".cs-live-bar, .cs-now-head, .cs-electron-running").evaluateAll(nodes => Math.max(...nodes.flatMap(node => node.getAnimations().map(animation => {
    const duration = Number(animation.effect!.getTiming().duration);
    const delta = Math.abs(Number(animation.currentTime) % duration - Date.now() % duration);
    return Math.min(delta, duration - delta);
  }))))).toBeLessThan(100);
  await detail.screenshot({ path: testInfo.outputPath("execution-live.png") });
  await detail.emulateMedia({ reducedMotion: "reduce" });
  await expect.poll(() => detail.locator(".cs-live-bar, .cs-now-head, .cs-electron-running, .cs-wave").evaluateAll(nodes => nodes.every(node => node.getAnimations().length === 0))).toBe(true);
  await expect(detail.locator(".cs-duration-bar")).toHaveCSS("transition-duration", "0s");
  await detail.emulateMedia({ reducedMotion: "no-preference" });

  await awaitRun(request, job.id, { status: "succeeded" });
  await expect(liveStrip).toHaveCount(0);
  await expect(row.locator('[data-history="recent"] [data-status="succeeded"]')).toHaveCount(2);
  await expect(detail.getByTestId("run-timeline-now")).toHaveCount(0);
  await expect(detail.locator('[data-testid="run-timeline-bar"].cs-live-bar')).toHaveCount(0);
  await detail.close();
});

test("history controls and aligned time lanes remain usable on desktop and phone", async ({ page, request }) => {
  const { job } = await applyAndRun(request, "branching.job.yaml");
  await page.goto(`/jobs?q=${job.alias}`);
  for (const width of [1440, 1280, 390]) {
    await page.setViewportSize({ width, height: 844 });
    await expect(page.getByLabel("History window")).toBeInViewport();
    await page.getByLabel("History window").selectOption("86400");
    await expect(page).toHaveURL(/history=86400/);
    const history = page.getByTestId("job-row").locator('[data-history="recent"]');
    await expect(history).toBeVisible();
    expect(await page.locator("main").evaluate(el => el.scrollWidth - el.clientWidth)).toBeLessThanOrEqual(1);
    if (width >= 1280) {
      const grid = await history.locator("[data-history-grid]").evaluateAll(nodes => nodes.map(node => node.getBoundingClientRect().x));
      const ticks = await page.locator("[data-history-tick]").evaluateAll(nodes => nodes.map(node => {
        const box = node.getBoundingClientRect();
        const p = Number(node.getAttribute("data-history-tick"));
        return p === 0 ? box.left : p === 1 ? box.right : box.x + box.width / 2;
      }));
      expect(ticks).toHaveLength(4);
      grid.forEach((x, index) => expect(Math.abs(x - ticks[index])).toBeLessThan(1));
    }
  }
});

test("SYNTHETIC auth gate: the login oscillator spans the viewport and honors reduced motion", async ({ page }) => {
  // Rendering-only auth state. Authentication behavior is covered by the auth lane.
  await page.route("**/auth/status", route => route.fulfill({ json: { enabled: true, methods: [] } }));
  await page.route("**/auth/whoami", route => route.fulfill({ status: 401, json: {} }));
  await page.goto("/jobs");
  await expect(page.getByText("Enter your API key to continue")).toBeVisible();
  const wave = page.getByTestId("ambient-oscillator");
  for (const width of [1440, 390]) {
    await page.setViewportSize({ width, height: 844 });
    await expect(wave).toHaveCSS("width", `${width}px`);
    await expect(wave).toHaveCSS("height", "60px");
    await expect(wave.locator("pattern")).toHaveAttribute("width", "40");
    expect(await page.evaluate(() => document.documentElement.scrollWidth - innerWidth)).toBeLessThanOrEqual(1);
  }
  const first = await wave.locator(".cs-wave").evaluate(el => getComputedStyle(el).transform);
  await expect.poll(() => wave.locator(".cs-wave").evaluate(el => getComputedStyle(el).transform)).not.toBe(first);
  await page.emulateMedia({ reducedMotion: "reduce" });
  await expect(wave.locator(".cs-wave")).toBeHidden();
  // A horizontal SVG path has zero layout height; its stroke still paints.
  await expect(wave.locator(".cs-wave-flat")).toHaveCSS("display", "block");
  await expect(wave.locator(".cs-wave-flat")).toHaveCSS("stroke", await wave.evaluate(el => getComputedStyle(el).color));
});
