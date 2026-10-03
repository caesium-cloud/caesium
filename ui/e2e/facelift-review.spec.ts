import { expect, test } from "@playwright/test";
import { applyAndRun, applyDefinitions, awaitRun, failOnUnexpectedPageErrors, findJobByAlias, triggerJob, uniqueSuffix } from "./helpers/fixtures";

failOnUnexpectedPageErrors();
test.use({ viewport: { width: 1280, height: 960 }, permissions: ["clipboard-read", "clipboard-write"] });

async function parameterizedRun(request: Parameters<typeof applyDefinitions>[0]) {
  const alias = `ETL_Daily_${uniqueSuffix()}`;
  await applyDefinitions(request, {
    apiVersion: "v1", kind: "Job", metadata: { alias },
    trigger: { type: "cron", configuration: { cron: "0 0 1 1 *" } },
    steps: [{ name: "Extract_Case", image: "alpine:3.23", command: ["sh", "-c", 'echo "##caesium::output {\\"mode\\":\\"$CAESIUM_PARAM_MODE\\"}"'] }],
  });
  const job = await findJobByAlias(request, alias);
  await triggerJob(request, job.id, { mode: "specific" });
  const run = await awaitRun(request, job.id, { status: "succeeded" });
  return { job, run };
}

test("identifiers retain case and re-run shortcuts respect navigation chords and Alt-R", async ({ page, request }) => {
  const { job, run } = await parameterizedRun(request);
  await page.goto("/jobs");
  const row = page.getByTestId("job-row").filter({ hasText: job.alias });
  await expect(row).not.toContainText("manual");
  await row.getByRole("link", { name: job.alias, exact: true }).click();
  await expect(page.getByRole("heading", { name: job.alias, exact: true })).toHaveCSS("text-transform", "none");
  await expect(page.getByTestId("job-run-context")).not.toContainText("run run");
  await page.goto(`/jobs/${job.id}/runs/${run.id}`);
  await expect(page.getByTestId("run-heading")).toBeVisible();
  let triggers = 0;
  page.on("request", r => { if (r.method() === "POST" && new URL(r.url()).pathname === `/v1/jobs/${job.id}/run`) triggers++; });
  await page.getByTestId("run-heading").click();
  await page.keyboard.press("r");
  await page.keyboard.press("g");
  await page.keyboard.press("a");
  await expect(page).toHaveURL(/\/atoms$/);
  expect(triggers).toBe(0);
  const history = await request.get(`/v1/jobs/${job.id}/runs`);
  expect((await history.json()).map((r: { id: string }) => r.id)).toEqual([run.id]);
  await page.goto(`/jobs/${job.id}/runs/${run.id}`);
  await expect(page.getByTestId("run-heading")).toBeVisible();
  const rerunNavigation = page.waitForURL(new RegExp(`/jobs/${job.id}/runs/(?!${run.id}$)[^/]+$`));
  await page.keyboard.press("Alt+r");
  await rerunNavigation;
  const rerun = await awaitRun(request, job.id, { status: "succeeded" });
  expect(rerun.params).toEqual({ mode: "specific" });
  await page.goto(`/jobs/${job.id}`);
  const jobHeading = page.getByRole("heading", { name: job.alias, exact: true });
  await expect(jobHeading).toBeVisible();
  const failedCounter = page.getByTestId("dag-counters").locator('[data-status="failed"]');
  await expect(failedCounter).toContainText("0 failed");
  const color = await failedCounter.evaluate(el => getComputedStyle(el).color);
  expect(await failedCounter.locator(".cs-status-glyph").evaluate(el => getComputedStyle(el).color)).toBe(color);
  expect(color).toBe(await jobHeading.locator("..").locator("..").locator(".text-text-3").first().evaluate(el => getComputedStyle(el).color));
  await page.goto(`/jobs/${job.id}/runs/${run.id}/diff?to=${rerun.id}`);
  await expect(page.getByText("Extract_Case", { exact: true }).first()).toHaveCSS("text-transform", "none");
});

test("SYNTHETIC: absent Clipboard API uses native copy and denied copy exposes selectable full text", async ({ page, request }) => {
  const { job, run } = await parameterizedRun(request);
  await page.goto(`/jobs/${job.id}/runs/${run.id}`);
  await expect(page.getByTestId("run-heading")).toBeVisible();
  await page.evaluate(() => {
    const read = navigator.clipboard.readText.bind(navigator.clipboard);
    Object.defineProperty(window, "qaReadClipboard", { value: read });
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: undefined });
  });
  const chip = page.getByRole("button", { name: `Copy run id: ${run.id}`, exact: true }).first();
  await chip.click();
  await expect(chip.getByRole("status")).toHaveText("Copied");
  expect(await page.evaluate(() => (window as unknown as { qaReadClipboard: () => Promise<string> }).qaReadClipboard())).toBe(run.id);
  await page.evaluate(() => { document.execCommand = () => false; });
  await chip.click();
  const full = page.getByRole("textbox", { name: "Full run id", exact: true });
  await expect(full).toHaveValue(run.id);
  expect(await full.evaluate(el => (el as HTMLInputElement).selectionEnd)).toBe(run.id.length);
});

test("YAML selection uses the active theme in both editor surfaces and follows theme changes", async ({ page, request }, info) => {
  await page.goto("/jobdefs");
  const editor = page.locator(".cm-editor");
  const theme = info.project.name === "light" ? "light" : "dark";
  await expect(editor).toHaveCSS("color-scheme", theme);
  await editor.locator(".cm-content").click();
  await page.keyboard.press("ControlOrMeta+A");
  await expect(editor.locator(".cm-selectionBackground").first()).toBeVisible();
  await editor.screenshot({ path: info.outputPath("yaml-selected.png") });
  const other = theme === "light" ? "Dark" : "Light";
  await page.getByRole("button", { name: "Toggle theme", exact: true }).click();
  await page.getByRole("menuitem", { name: other, exact: true }).click();
  await expect(editor).toHaveCSS("color-scheme", other.toLowerCase());
  const { job } = await parameterizedRun(request);
  await page.goto(`/jobs/${job.id}/yaml`);
  await expect(page.getByTestId("job-manifest-yaml").locator(".cm-editor")).toHaveCSS("color-scheme", other.toLowerCase());
});

test("SYNTHETIC: malformed run timestamps do not crash job, history, run, breadcrumb or compare views", async ({ page, request }) => {
  const { job, run } = await parameterizedRun(request);
  await triggerJob(request, job.id);
  await expect.poll(async () => {
    const response = await request.get(`/v1/jobs/${job.id}/runs`);
    return (await response.json()).some((r: { id: string; status: string }) => r.id !== run.id && r.status === "succeeded");
  }).toBe(true);
  const second = await awaitRun(request, job.id, { status: "succeeded" });
  expect(second.id).not.toBe(run.id);
  function malformed(value: unknown): unknown {
    if (Array.isArray(value)) return value.map(malformed);
    if (!value || typeof value !== "object") return value;
    const result = Object.fromEntries(Object.entries(value).map(([key, v]) => [key, malformed(v)]));
    if (result.job_id === job.id && "started_at" in result && !("task_id" in result)) result.started_at = "not-a-date";
    return result;
  }
  await page.route(`**/v1/jobs/${job.id}**`, async route => {
    const response = await route.fetch();
    if (response.headers()["content-type"]?.includes("application/json")) await route.fulfill({ response, json: malformed(await response.json()) });
    else await route.fulfill({ response });
  });
  await page.goto(`/jobs/${job.id}`);
  await expect(page.getByRole("heading", { name: job.alias, exact: true })).toBeVisible();
  await expect(page.getByTestId("job-run-context")).toContainText("Unknown time");
  await page.goto(`/jobs/${job.id}/runs`);
  await expect(page.getByRole("dialog")).toContainText("Unknown time");
  await page.goto(`/jobs/${job.id}/runs/${run.id}`);
  await expect(page.getByTestId("run-heading").locator("..")).toContainText("Unknown time");
  await expect(page.getByRole("navigation", { name: "Breadcrumb" })).toContainText("run unknown");
  await page.goto(`/jobs/${job.id}/runs/${run.id}/diff?to=${second.id}`);
  await expect(page.getByText("run started unknown", { exact: true })).toHaveCount(2);
});

test("terminal branching timeline stays stable when the viewer's clock advances", async ({ page, request }) => {
  const { job, run } = await applyAndRun(request, "branching.job.yaml", { status: "succeeded" });
  await page.clock.install({ time: new Date(Date.parse(run.started_at) + 86_400_000) });
  await page.goto(`/jobs/${job.id}/runs/${run.id}`);
  const bars = page.getByTestId("run-timeline-bar");
  await expect(bars).toHaveCount(run.tasks.length);
  expect(await bars.evaluateAll(nodes => nodes.every(node => node.getAttribute("data-ghost") === "false"))).toBe(true);
  const positions = await bars.evaluateAll(nodes => nodes.map(node => (node as HTMLElement).style.left));
  await page.clock.fastForward(5000);
  expect(await bars.evaluateAll(nodes => nodes.map(node => (node as HTMLElement).style.left))).toEqual(positions);
  await expect(page.getByText("waits on upstream work", { exact: true })).toHaveCount(0);
});

test("SYNTHETIC: late-mounted CSS instruments share UTC phase and resync on visibility", async ({ page }) => {
  await page.goto("/system");
  await expect(page.getByRole("heading", { name: "System", exact: true })).toBeVisible();
  const insert = async (group: string) => page.evaluate(group => {
    const host = document.createElement("div"); host.className = "cs-animated"; host.dataset.phaseGroup = group;
    for (const seconds of [1, 2, 6, 22, 30, 38]) {
      const node = document.createElement("span");
      node.dataset.phaseProbe = String(seconds);
      node.style.cssText = `display:block;position:fixed;left:-100px;width:10px;height:10px;animation:cs-spin ${seconds}s linear infinite`;
      host.append(node);
    }
    document.body.append(host);
  }, group);
  await insert("first");
  const clock = page.locator("header").getByText(/^\d{2}:\d{2}:\d{2}$/);
  const before = await clock.textContent();
  await expect.poll(() => clock.textContent()).not.toBe(before);
  await insert("second");
  const phaseErrors = () => page.locator("[data-phase-probe]").evaluateAll(nodes => nodes.map(node => {
    const a = node.getAnimations()[0];
    if (!a || typeof a.currentTime !== "number") return Infinity;
    const duration = Number(a.effect!.getTiming().duration);
    const delta = Math.abs(a.currentTime % duration - Date.now() % duration);
    return Math.min(delta, duration - delta);
  }));
  await expect.poll(async () => Math.max(...await phaseErrors())).toBeLessThan(100);
  await page.evaluate(() => {
    document.querySelectorAll("[data-phase-probe]").forEach(node => { const a = node.getAnimations()[0]; a.startTime = Number(a.startTime) + 323; });
    document.dispatchEvent(new Event("visibilitychange"));
  });
  await expect.poll(async () => Math.max(...await phaseErrors())).toBeLessThan(100);
  await page.emulateMedia({ reducedMotion: "reduce" });
  await expect.poll(() => page.locator("[data-phase-probe]").evaluateAll(nodes => nodes.every(node => node.getAnimations().length === 0))).toBe(true);
});
