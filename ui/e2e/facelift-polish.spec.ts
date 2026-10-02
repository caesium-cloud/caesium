import { expect, test } from "@playwright/test";
import { applyAndRun, applyDefinitions, awaitRun, failOnUnexpectedPageErrors, findJobByAlias, loadFixtureDefinition, triggerJob, uniqueSuffix } from "./helpers/fixtures";

failOnUnexpectedPageErrors();
test.use({ viewport: { width: 1280, height: 800 }, permissions: ["clipboard-read", "clipboard-write"] });

test("section navigation resets main scroll and Back restores the actual list entry", async ({ page, request }) => {
  const definition = await loadFixtureDefinition("run-history.job.yaml");
  definition.trigger = { type: "cron", configuration: { cron: "0 0 1 1 *" } } as typeof definition.trigger;
  const prefix = `polish-scroll-${uniqueSuffix()}`;
  // Real definitions make both destinations tall enough to expose a carried offset.
  await applyDefinitions(request, ...Array.from({ length: 14 }, (_, i) => ({ ...definition, metadata: { ...definition.metadata, alias: `${prefix}-${i}` } })));
  await page.goto("/jobs");
  await page.getByPlaceholder("Filter pipelines…").fill(prefix);
  await expect(page.getByTestId("job-row")).toHaveCount(14);
  // Stay within persistent rows; the live activity feed is session-local.
  await page.locator("main").evaluate(el => { el.scrollTop = 200; });
  await expect.poll(() => page.locator("main").evaluate(el => el.scrollTop)).toBe(200);
  await page.locator('aside nav a[href="/triggers"]').click();
  await expect(page.getByRole("heading", { name: "Triggers", exact: true })).toBeVisible();
  await expect.poll(() => page.locator("main").evaluate(el => el.scrollTop)).toBe(0);
  await page.goBack();
  await expect(page.getByPlaceholder("Filter pipelines…")).toHaveValue(prefix);
  await expect.poll(() => page.locator("main").evaluate(el => el.scrollTop)).toBe(200);
  await page.keyboard.press("g"); await page.keyboard.press("a");
  await expect(page.getByRole("heading", { name: "Atoms", exact: true })).toBeVisible();
  await expect.poll(() => page.locator("main").evaluate(el => el.scrollTop)).toBe(0);
});

test("mobile detail keeps navigation, the latest run, and timeline context reachable", async ({ page, request }) => {
  const { job, run } = await applyAndRun(request, "branching.job.yaml");
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto(`/jobs/${job.id}`);
  const counters = page.getByTestId("dag-counters");
  await expect(counters).toContainText("3 succeeded");
  await expect(counters).toContainText("1 skipped");
  await expect(counters).not.toContainText("blocked");
  const latest = page.getByRole("link", { name: /^run started/ });
  await expect(latest).toBeInViewport();
  const cache = page.getByTestId("job-detail-view-tabs").getByRole("link", { name: "Cache", exact: true });
  await expect(cache).toBeInViewport();
  const viewport = page.locator(".react-flow__viewport");
  await expect.poll(() => viewport.evaluate(el => new DOMMatrix(getComputedStyle(el).transform).a)).toBeGreaterThanOrEqual(.75);
  await cache.click();
  const dialog = page.getByRole("dialog");
  await expect(dialog.getByText("Server default", { exact: true }).first()).toBeVisible();
  await page.keyboard.press("Escape");
  await latest.click();
  await expect(page.getByTestId("run-heading")).toBeVisible();
  await expect(page.getByText(/tasks 4\/4 completed/)).toBeVisible();
  const timeline = page.getByRole("region", { name: "Execution timeline", exact: true });
  const label = page.getByTestId("run-timeline-task-row").first().locator(".sticky");
  const before = (await label.boundingBox())!.x;
  await timeline.evaluate(el => { el.scrollLeft = el.scrollWidth; });
  expect(Math.abs((await label.boundingBox())!.x - before)).toBeLessThan(1);
  await expect(page.getByText("Scroll timeline horizontally → · task names stay visible")).toBeVisible();
  await page.getByText("Skipped · reason", { exact: true }).click();
  await expect(page.locator("details[open]")).toContainText("not selected by branch task");
  await page.getByTestId("run-rerun-trigger").scrollIntoViewIfNeeded();
  await expect(page.getByTestId("run-rerun-trigger")).toBeInViewport();
  await expect(page.getByTestId("all-runs-link")).toHaveCount(1);
  // A scrolled detail must reset on a top-level keyboard destination too.
  await page.locator("main").evaluate(el => { el.scrollTop = 300; });
  await page.keyboard.press("g"); await page.keyboard.press("t");
  await expect(page).toHaveURL(/\/triggers$/);
  await expect.poll(() => page.locator("main").evaluate(el => el.scrollTop)).toBe(0);
  expect(run.tasks.filter(task => task.status === "skipped")).toHaveLength(1);
});

test("sidebar columns, wrapped filters, and filtered-empty state remain usable", async ({ page, request }) => {
  await applyAndRun(request, "branching.job.yaml");
  await page.goto("/jobs");
  await expect(page.getByTestId("job-row").first()).toBeVisible();
  const columns = await page.locator("aside nav [data-nav-count]").evaluateAll(counts => counts.map(count => count.getBoundingClientRect().right));
  expect(new Set(columns).size).toBe(1);
  await expect(page.locator("aside nav kbd")).toHaveCount(0);
  const jobsLink = page.locator('aside nav a[href="/jobs"]');
  await jobsLink.hover();
  await expect(page.getByRole("tooltip", { name: "Keyboard shortcut: G, then J" })).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(page.getByRole("tooltip")).toHaveCount(0);
  await page.setViewportSize({ width: 390, height: 844 });
  for (const name of [/^All/, /^running/, /^succeeded/, /^failed/, /^paused/]) await expect(page.getByRole("button", { name }).first()).toBeInViewport();
  await page.getByPlaceholder("Filter pipelines…").fill(`no-match-${uniqueSuffix()}`);
  await expect(page.getByText("No pipelines match")).toBeVisible();
  await page.getByRole("button", { name: "clear filters", exact: true }).click();
  await expect(page.getByTestId("job-row").first()).toBeVisible();
  await expect(page.getByTestId("job-row").first().getByRole("button", { name: "Trigger run", exact: true }).locator("..")).toHaveCSS("opacity", "1");
  expect(await page.locator("main").evaluate(el => el.scrollWidth - el.clientWidth)).toBeLessThanOrEqual(1);
});

test("atoms decode real command arguments and keep exact raw values and copying", async ({ page, request }) => {
  const { run } = await applyAndRun(request, "branching.job.yaml");
  const task = run.tasks.find(task => task.status === "succeeded" && task.command?.some(arg => arg.includes("&&")))!;
  expect(task).toBeTruthy();
  await page.goto("/atoms");
  await page.getByPlaceholder("Search by image, command, ID...").fill(task.atom_id!);
  const row = page.locator("tbody tr").first();
  await expect(row).toContainText("&&");
  await expect(row).not.toContainText("\\u0026");
  await row.getByRole("button", { name: `Details for atom ${task.atom_id}` }).click();
  const command = page.getByLabel("Command arguments (quoted)");
  await expect(command).toContainText("&&");
  await page.getByText("Raw command", { exact: true }).click();
  const response = await request.get("/v1/atoms");
  const atom = (await response.json()).find((a: { id: string }) => a.id === task.atom_id);
  await expect(page.locator("details[open] pre")).toHaveText(atom.command);
  await page.getByRole("button", { name: `Copy atom id: ${task.atom_id}`, exact: true }).last().click();
  expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(task.atom_id);
  await expect(page.getByText("Full ID", { exact: true })).toHaveCount(0);
});

test("mobile live overlay wraps all counters beside a long pipeline name", async ({ page, request }) => {
  const alias = `polish-live-warehouse-regional-customer-daily-export-with-retained-logs-${uniqueSuffix()}`;
  await applyDefinitions(request, {
    metadata: { alias }, apiVersion: "v1", kind: "Job",
    trigger: { type: "cron", configuration: { cron: "0 0 1 1 *" } },
    steps: [{ name: "wait-for-inspection", image: "alpine:3.23", cache: false, command: ["sh", "-c", "sleep 12"] }],
  });
  const job = await findJobByAlias(request, alias);
  await triggerJob(request, job.id);
  await awaitRun(request, job.id, { status: "running" });
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto(`/jobs/${job.id}`);
  const counters = page.getByTestId("dag-counters");
  await expect(counters).toContainText("1 running");
  await expect.poll(() => counters.evaluate(el => {
    const panel = el.closest("main")!.getBoundingClientRect();
    return [...el.children].every(child => {
      const box = child.getBoundingClientRect();
      return box.left >= panel.left && box.right <= panel.right;
    });
  })).toBe(true);
  await expect(page.getByRole("link", { name: /^run started/ })).toBeInViewport();
  expect(await page.locator("main").evaluate(el => el.scrollWidth - el.clientWidth)).toBeLessThanOrEqual(1);
  await awaitRun(request, job.id, { status: "succeeded" });
});
