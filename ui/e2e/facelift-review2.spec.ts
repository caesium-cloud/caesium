import AxeBuilder from "@axe-core/playwright";
import { expect, test, type Page } from "@playwright/test";
import { applyDefinitions, awaitRun, failOnUnexpectedPageErrors, findJobByAlias, triggerJob, uniqueSuffix } from "./helpers/fixtures";

failOnUnexpectedPageErrors();
test.use({ viewport: { width: 1280, height: 900 }, permissions: ["clipboard-read", "clipboard-write"] });
const engine = process.env.CAESIUM_E2E_ENGINE || "docker";

async function emit(page: Page, type: string, payload: unknown) {
  await page.evaluate(({ type, payload }) => {
    const source = (window as unknown as { qaSources: EventSource[] }).qaSources.find(source => source.readyState === 1)!;
    source.dispatchEvent(new MessageEvent(type, { data: JSON.stringify(payload) }));
  }, { type, payload });
}
async function exposeStream(page: Page) {
  // SYNTHETIC events still traverse the application's native EventSource handlers.
  await page.addInitScript(() => {
    const Original = window.EventSource;
    const sources: EventSource[] = [];
    (window as unknown as { qaSources: EventSource[] }).qaSources = sources;
    window.EventSource = class extends Original {
      constructor(url: string | URL, options?: EventSourceInit) { super(url, options); sources.push(this); }
    };
  });
}
async function readyStream(page: Page) {
  await expect.poll(() => page.evaluate(() => (window as unknown as { qaSources: EventSource[] }).qaSources.some(source => source.readyState === 1))).toBe(true);
}

async function fixture(request: Parameters<typeof applyDefinitions>[0], twoRuns = false) {
  const alias = `Review_ETL_${uniqueSuffix()}`;
  await applyDefinitions(request, {
    apiVersion: "v1", kind: "Job", metadata: { alias },
    trigger: { type: "cron", configuration: { cron: "0 0 1 1 *" } },
    steps: [{ name: "Extract_Case", engine, image: "alpine:3.23", command: ["sh", "-c", "echo 'hello && world'"] }],
  });
  const job = await findJobByAlias(request, alias);
  await triggerJob(request, job.id);
  const first = await awaitRun(request, job.id, { status: "succeeded" });
  if (!twoRuns) return { job, first, latest: first };
  await triggerJob(request, job.id);
  const latest = await awaitRun(request, job.id, { status: "succeeded" });
  expect(latest.id).not.toBe(first.id);
  return { job, first, latest };
}

async function isolateStream(page: Page) {
  await page.route("**/v1/events?**", route => {
    const url = new URL(route.request().url());
    url.searchParams.set("types", "review_controlled_stream");
    return route.continue({ url: url.toString() });
  });
  await exposeStream(page);
}

test("final pass: compare shortcut, dialog clipboard fallback, and DAG keyboard focus work through the rendered controls", async ({ page, request }, info) => {
  const { job, latest } = await fixture(request, true);
  await page.addInitScript(() => {
    const read = navigator.clipboard.readText.bind(navigator.clipboard);
    (window as unknown as { qaReadClipboard: () => Promise<string> }).qaReadClipboard = read;
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: undefined });
  });
  await page.goto(`/jobs/${job.id}/runs/${latest.id}`);
  await expect(page.getByTestId("run-compare-trigger")).toBeEnabled();
  await page.keyboard.press("c");
  await expect(page.getByRole("menu")).toBeVisible();
  await expect(page.getByTestId("run-compare-option").first()).toBeFocused();
  await page.keyboard.press("Escape");
  await expect(page.getByTestId("run-compare-trigger")).toBeFocused();
  await page.keyboard.press("p");
  const dialog = page.getByTestId("replay-dialog");
  await expect(dialog).toBeVisible();
  const copy = dialog.getByRole("button", { name: `Copy baseline run id: ${latest.id}`, exact: true });
  await copy.click();
  expect(await page.evaluate(() => (window as unknown as { qaReadClipboard: () => Promise<string> }).qaReadClipboard())).toBe(latest.id);
  await expect(copy).toBeFocused();
  await page.screenshot({ path: info.outputPath("dialog-copy.png") });
  await page.keyboard.press("Escape");
  const node = page.locator(".react-flow__node").first();
  await page.keyboard.press("Tab");
  await node.focus();
  await expect(node).toHaveCSS("outline-style", "solid");
  await expect(node).toHaveCSS("outline-width", "2px");
  await page.screenshot({ path: info.outputPath("dag-keyboard-focus.png") });
  await page.keyboard.press("Enter");
  await expect(page.getByTestId("task-detail-panel")).toBeVisible();
});

test("final pass SYNTHETIC: slow jobs reads land during sustained events and repeated retries remain distinct", async ({ page, request }) => {
  const { job, first } = await fixture(request);
  const jobs = await (await request.get("/v1/jobs")).json();
  let revision = 0, reads = 0, inFlight = 0, maxInFlight = 0;
  await page.route("**/v1/jobs", async route => {
    reads++; inFlight++; maxInFlight = Math.max(maxInFlight, inFlight);
    const snapshot = revision;
    try {
      await new Promise(resolve => setTimeout(resolve, 900));
      await route.fulfill({ json: jobs.map((entry: { id: string }) => entry.id === job.id ? { ...entry, alias: `${job.alias}-refresh-${snapshot}` } : entry) });
    } finally { inFlight--; }
  });
  await isolateStream(page);
  await page.goto(`/jobs?q=${job.alias}`);
  await readyStream(page);
  const row = page.getByTestId("job-row").filter({ hasText: job.alias });
  await expect(row).toContainText("refresh-0");
  const burstStarted = Date.now();
  const readsBeforeBurst = reads;
  for (let index = 1; index <= 24; index++) {
    revision = index;
    const event = { sequence: 900000 + index, type: "run_retried", job_id: job.id, run_id: first.id, timestamp: new Date().toISOString() };
    await emit(page, "run_retried", event);
    if (index === 1) await emit(page, "run_retried", event); // A retained replay must not add another activity entry.
    await page.waitForTimeout(100);
    if (index === 18) await expect(row).not.toContainText("refresh-0");
  }
  await expect(row).toContainText("refresh-24", { timeout: 7000 });
  expect(maxInFlight).toBe(1);
  // At most one read per 900ms latency interval, plus the trailing refresh.
  // Browser evaluations take longer on loaded CI runners than on a workstation.
  expect(reads - readsBeforeBurst).toBeLessThanOrEqual(Math.ceil((Date.now() - burstStarted) / 900) + 1);
  const feed = page.getByTestId("activity-feed");
  await expect(feed.locator('[aria-label="Run retried"]')).toHaveCount(20);
  await expect(feed.locator('[data-status="running"]')).toHaveCount(20);
  await page.unrouteAll({ behavior: "wait" });
});

test("final pass SYNTHETIC: retained lifecycle bursts coalesce paged job history and cancellation stops the walk", async ({ page, request }) => {
  const { job, first } = await fixture(request);
  const history = Array.from({ length: 1500 }, (_, index) => ({ ...first, id: index === 0 ? first.id : `review-history-${index}`,
    started_at: new Date(Date.parse(first.started_at) - index * 1000).toISOString(), tasks: [] }));
  let reads = 0, inFlight = 0, maxInFlight = 0;
  await page.route(`**/v1/jobs/${job.id}/runs?**`, async route => {
    const url = new URL(route.request().url());
    if (url.searchParams.get("limit") !== "500") return route.continue();
    reads++; inFlight++; maxInFlight = Math.max(maxInFlight, inFlight);
    const offset = Number(url.searchParams.get("offset") ?? 0);
    try {
      await new Promise(resolve => setTimeout(resolve, 200));
      await route.fulfill({ json: history.slice(offset, offset + 500), headers: { "X-Caesium-Total-Count": "1500", ...(offset + 500 < 1500 ? { "X-Caesium-Next-Offset": String(offset + 500) } : {}) } });
    } finally { inFlight--; }
  });
  await isolateStream(page); await page.goto(`/jobs/${job.id}`); await readyStream(page);
  await page.evaluate(({ jobId, run }) => {
    const source = (window as unknown as { qaSources: EventSource[] }).qaSources.find(source => source.readyState === 1)!;
    for (let index = 0; index < 500; index++) source.dispatchEvent(new MessageEvent("run_completed", { data: JSON.stringify({ sequence: 1000000 + index, type: "run_completed", job_id: jobId, run_id: run.id, timestamp: new Date().toISOString(), payload: run }) }));
  }, { jobId: job.id, run: first });
  await expect(page.getByTestId("view-featured-run")).toBeVisible();
  await page.waitForTimeout(1500);
  expect(reads).toBeLessThanOrEqual(6);
  expect(maxInFlight).toBe(1);
  // Start a new slow walk, leave the route while its first page is in flight,
  // and ensure its AbortSignal prevents background paging.
  const before = reads;
  await emit(page, "run_started", { sequence: 2000000, type: "run_started", job_id: job.id, run_id: first.id, timestamp: new Date().toISOString() });
  await expect.poll(() => reads).toBe(before + 1);
  await page.goto("/triggers");
  await page.waitForTimeout(800);
  expect(reads).toBe(before + 1);
  await page.unrouteAll({ behavior: "wait" });
});

test("final pass: a missed run event is reconciled with a cheap job read", async ({ page, request }) => {
  const { job, first } = await fixture(request);
  await isolateStream(page); await page.goto(`/jobs/${job.id}`); await readyStream(page);
  const link = page.getByTestId("view-featured-run");
  await expect(link).toHaveAttribute("href", `/jobs/${job.id}/runs/${first.id}`);
  await triggerJob(request, job.id);
  const next = await awaitRun(request, job.id, { status: "succeeded" });
  expect(next.id).not.toBe(first.id);
  await expect(link).toHaveAttribute("href", `/jobs/${job.id}/runs/${next.id}`, { timeout: 75000 });
  await page.unrouteAll({ behavior: "wait" });
});

test("second pass: trigger identifiers keep case, missing-key events are safe, and compare items are keyboard accessible", async ({ page, request }, info) => {
  const { job, first, latest } = await fixture(request, true);
  await page.goto(`/jobs/${job.id}`);
  await page.getByRole("button", { name: "Trigger job", exact: true }).click();
  const title = page.getByRole("heading", { name: "Trigger Job", exact: true });
  await expect(title).toContainText(job.alias);
  await expect(title).toHaveCSS("text-transform", "none");
  await page.evaluate(() => {
    const event = new KeyboardEvent("keydown", { bubbles: true });
    Object.defineProperty(event, "key", { value: undefined });
    document.querySelector('textarea,input')!.dispatchEvent(event);
  });
  await page.keyboard.press("Escape");
  await page.keyboard.press("ControlOrMeta+k");
  await expect(page.getByRole("combobox", { name: "Search pages, jobs, triggers, or atoms" })).toBeVisible();
  await page.keyboard.press("Escape");
  await page.goto(`/jobs/${job.id}/runs/${latest.id}`);
  const compare = page.getByRole("button", { name: /Compare to run/ });
  await compare.focus(); await page.keyboard.press("Enter");
  const item = page.getByRole("menuitem").filter({ hasText: first.id.slice(0, 8) });
  await expect(item).toBeVisible();
  await expect(item.locator('button,input,a,[tabindex="0"]')).toHaveCount(0);
  const axe = await new AxeBuilder({ page }).include('[role="menu"]').withRules(["nested-interactive"]).analyze();
  expect(axe.violations).toEqual([]);
  await page.screenshot({ path: info.outputPath("compare-and-identities.png") });
  await item.focus(); await page.keyboard.press("Enter");
  await expect(page).toHaveURL(new RegExp(`/runs/${latest.id}/diff\\?to=${first.id}$`));
});

test("second pass: real partition retry reopens the run through SSE before REST reconciliation", async ({ page, request }, info) => {
  const alias = `retry-stream-${uniqueSuffix()}`;
  await applyDefinitions(request, {
    apiVersion: "v1", kind: "Job", metadata: { alias },
    trigger: { type: "cron", configuration: { cron: "0 0 1 1 *" } },
    steps: [
      { name: "list", engine, image: "alpine:3.23", command: ["sh", "-c", `echo '##caesium::partitions ["one"]'`], next: ["work"] },
      { name: "work", engine, image: "alpine:3.23", command: ["sh", "-c", "sleep 12; exit 1"], dependsOn: ["list"], fanOut: { from: "list", env: "CAESIUM_PARTITION", failurePolicy: "continue", maxPartitions: 8, maxParallel: 1 } },
    ],
  });
  const job = await findJobByAlias(request, alias);
  await triggerJob(request, job.id);
  const run = await awaitRun(request, job.id, { status: "failed" });
  await exposeStream(page);
  await page.goto(`/jobs/${job.id}/runs/${run.id}`);
  await expect(page.getByTestId("run-heading")).toBeVisible(); await readyStream(page);
  await page.locator(".react-flow__node", { hasText: "work" }).click();
  await page.getByTestId("task-detail-panel").getByRole("button", { name: "Details", exact: true }).click();
  const retry = page.getByTestId("partition-table").getByRole("button", { name: "Retry", exact: true });
  await expect(retry).toBeEnabled();
  let release!: () => void;
  const gate = new Promise<void>(resolve => { release = resolve; });
  // Hold just the reconciliation read. All retry POSTs, SSE and partition reads are real.
  await page.route(`**/v1/jobs/${job.id}/runs/${run.id}`, async route => { await gate; await route.continue(); });
  try {
    await retry.click();
    await expect(page.getByTestId("run-timeline-now")).toBeVisible({ timeout: 10_000 });
    // Task start events must also be accepted after the terminal parent reopens.
    await expect(page.locator(".react-flow__node", { hasText: "work" }).getByTestId("status-icon-running")).toBeVisible({ timeout: 10_000 });
    await expect(page.getByTestId("run-timeline-task-row").filter({ hasText: "work" })).not.toContainText(/-\d+(?:\.\d+)?(?:ms|s)/);
    await page.screenshot({ path: info.outputPath("real-partition-reopened.png") });
  } finally { release(); await page.unrouteAll({ behavior: "wait" }); }
  await expect(page.getByTestId("run-timeline-now")).toBeHidden({ timeout: 30_000 });
  const response = await request.get(`/v1/jobs/${job.id}/runs/${run.id}`);
  expect((await response.json()).status).toBe("failed");
});

test("second pass SYNTHETIC: healthy SSE does not repeatedly walk 8000 historical runs", async ({ page, request }) => {
  const { job, first } = await fixture(request);
  let historyReads = 0;
  await isolateStream(page);
  const history = Array.from({ length: 8000 }, (_, i) => ({ ...first, id: i === 0 ? first.id : `synthetic-history-${i}`, status: i === 0 ? first.status : "failed" }));
  await page.route(new RegExp(`/v1/jobs/${job.id}/runs(?:\\?|$)`), route => {
    historyReads++;
    const url = new URL(route.request().url());
    const offset = Number(url.searchParams.get("offset") || 0), limit = Number(url.searchParams.get("limit") || 100);
    return route.fulfill({ json: history.slice(offset, offset + limit), headers: { "X-Caesium-Total-Count": "8000", ...(offset + limit < 8000 ? { "X-Caesium-Next-Offset": String(offset + limit) } : {}) } });
  });
  await page.clock.install();
  await page.goto(`/jobs/${job.id}`); await readyStream(page);
  await expect(page.getByRole("heading", { name: job.alias, exact: true })).toBeVisible();
  await expect.poll(() => historyReads).toBe(16);
  await page.clock.fastForward(31_000);
  // Keep an observation window after the fast-forward for asynchronous query work.
  await page.waitForTimeout(300);
  expect(historyReads).toBe(16);
  await page.unrouteAll({ behavior: "wait" });
});

test("final pass SYNTHETIC: replica disagreement retries on later polls without a feedback loop", async ({ page, request }) => {
  const { job, first } = await fixture(request);
  const response = await request.get(`/v1/jobs/${job.id}`);
  const snapshot = await response.json();
  await page.route(`**/v1/jobs/${job.id}`, route => route.fulfill({ json: { ...snapshot, latest_run: { ...first, status: "failed" } } }));
  let reads = 0;
  await page.route(`**/v1/jobs/${job.id}/runs?**`, route => {
    reads++;
    return route.fulfill({ json: [first], headers: { "X-Caesium-Total-Count": "1" } });
  });
  await page.clock.install();
  await isolateStream(page); await page.goto(`/jobs/${job.id}`); await readyStream(page);
  await expect.poll(() => reads).toBe(2); // Initial history, then one reconciliation.
  await page.waitForTimeout(1000);
  expect(reads).toBe(2);
  await page.clock.fastForward(65_000);
  await expect.poll(() => reads).toBe(3); // One later retry for eventual consistency.
  await page.waitForTimeout(1000);
  expect(reads).toBe(3);
  await page.unrouteAll({ behavior: "wait" });
});

test("second pass SYNTHETIC: sustained native stream events refresh history and older completion preserves latest run", async ({ page, request }) => {
  const { job, first, latest } = await fixture(request, true);
  const listResponse = await request.get("/v1/jobs");
  const jobs = await listResponse.json();
  const projected = jobs.map((entry: { id: string }) => entry.id === job.id ? { ...entry, latest_run: { ...latest, status: "running", completed_at: undefined } } : entry);
  let listReads = 0;
  let blocked: Promise<void> | undefined;
  let inFlight = 0;
  // Keep one route handler throughout, including the gated concurrent read.
  await page.route("**/v1/jobs", async route => {
    listReads++; inFlight++;
    try {
      if (blocked) await blocked;
      await route.fulfill({ json: projected });
    } finally { inFlight--; }
  });
  await isolateStream(page); await page.goto(`/jobs?q=${job.alias}`); await readyStream(page);
  const row = page.getByTestId("job-row").filter({ hasText: job.alias });
  await expect(row.getByRole("link", { name: /Open latest run/ })).toHaveAttribute("href", `/jobs/${job.id}/runs/${latest.id}`);
  await expect(row.locator(":scope > :nth-child(2)")).toHaveText("running");
  // Synthetic concurrent completion uses real run IDs; block refresh while checking the cache.
  let release!: () => void;
  blocked = new Promise<void>(resolve => { release = resolve; });
  const beforeGate = listReads;
  try {
    await emit(page, "run_completed", { type: "run_completed", job_id: job.id, run_id: first.id, timestamp: new Date().toISOString(), payload: first });
    await expect.poll(() => listReads).toBeGreaterThan(beforeGate);
    await expect(row.getByRole("link", { name: /Open latest run/ })).toHaveAttribute("href", `/jobs/${job.id}/runs/${latest.id}`);
    await expect(row.locator(":scope > :nth-child(2)")).toHaveText("running");
  } finally { blocked = undefined; release(); }
  await expect.poll(() => inFlight).toBe(0);
  await page.waitForTimeout(350); // Let the coalescer finish the gated query.
  const beforeBurst = listReads;
  await page.evaluate(({ jobId, run }) => {
    const source = (window as unknown as { qaSources: EventSource[] }).qaSources.find(source => source.readyState === 1)!;
    const start = performance.now(); let sequence = 0;
    const timer = setInterval(() => {
      source.dispatchEvent(new MessageEvent("run_started", { data: JSON.stringify({ type: "run_started", job_id: jobId, run_id: `synthetic-busy-${sequence++}`, timestamp: new Date().toISOString(), payload: run }) }));
      if (performance.now() - start >= 2400) clearInterval(timer);
    }, 80);
  }, { jobId: job.id, run: latest });
  await expect.poll(() => listReads - beforeBurst, { timeout: 2200, intervals: [100] }).toBeGreaterThanOrEqual(4);
  await page.waitForTimeout(2600); await page.unrouteAll({ behavior: "wait" });
});

test("second pass SYNTHETIC browser capability: all copy actions work without Clipboard API and report failure", async ({ page, request }, info) => {
  const { job, first } = await fixture(request);
  await page.addInitScript(() => {
    const read = navigator.clipboard.readText.bind(navigator.clipboard);
    (window as unknown as { qaReadClipboard: () => Promise<string> }).qaReadClipboard = read;
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: undefined });
  });
  const copied = () => page.evaluate(() => (window as unknown as { qaReadClipboard: () => Promise<string> }).qaReadClipboard());
  await page.goto(`/jobs/${job.id}/runs/${first.id}`);
  await page.locator(".react-flow__node", { hasText: "Extract_Case" }).click();
  const panel = page.getByTestId("task-detail-panel");
  await expect(panel.getByTestId("task-log-plaintext")).toContainText("hello && world");
  await panel.getByRole("button", { name: "Copy", exact: true }).click();
  expect(await copied()).toContain("hello && world");
  // Create a real HTTP trigger without firing it.
  const alias = `copy-webhook-${uniqueSuffix()}`;
  await applyDefinitions(request, { apiVersion: "v1", kind: "Job", metadata: { alias }, trigger: { type: "http", configuration: { path: `/review/${alias}`, auth: { scheme: "none" } } }, steps: [{ name: "noop", engine, image: "alpine:3.23", command: ["true"] }] });
  await page.goto("/triggers");
  const row = page.getByTestId("trigger-card").filter({ hasText: alias });
  await row.getByRole("button", { name: "Copy webhook URL", exact: true }).click();
  expect(await copied()).toContain(`/review/${alias}`);
  await page.evaluate(() => { document.execCommand = () => false; });
  await row.getByRole("button", { name: "Webhook URL copied", exact: true }).click();
  await expect(page.getByText("Clipboard unavailable", { exact: true })).toBeVisible();
  await page.goto("/system/database");
  const editor = page.getByRole("textbox", { name: "SQL query editor" });
  await expect(page.getByRole("button", { name: "Refresh schema", exact: true })).toBeEnabled();
  await expect(editor).toHaveValue(/SELECT/i);
  await editor.fill("SELECT 'AbCd' AS MixedCaseColumn");
  await expect(editor).toHaveValue("SELECT 'AbCd' AS MixedCaseColumn");
  await page.getByRole("button", { name: "Run query", exact: true }).click();
  const column = page.getByRole("columnheader").filter({ hasText: "MixedCaseColumn" });
  await expect(column).toBeVisible(); await expect(column).toHaveCSS("text-transform", "none");
  await page.getByRole("button", { name: "Copy JSON", exact: true }).click();
  expect(JSON.parse(await copied())).toEqual([{ MixedCaseColumn: "AbCd" }]);
  await column.scrollIntoViewIfNeeded();
  await page.screenshot({ path: info.outputPath("sql-case-and-copy.png") });
  await page.evaluate(() => { document.execCommand = () => false; });
  await page.getByRole("button", { name: "Copy JSON", exact: true }).click();
  await expect(page.getByText("Clipboard unavailable", { exact: true })).toBeVisible();
});

test("second pass SYNTHETIC: incident analytics errors use the failed glyph", async ({ page }, info) => {
  await page.route("**/v1/system/features", async route => {
    const response = await route.fetch();
    await route.fulfill({ json: { ...await response.json(), agent_remediation_enabled: true } });
  });
  await page.route("**/v1/incidents?**", route => route.fulfill({ status: 503, json: { message: "Review incidents unavailable" } }));
  await page.goto("/stats");
  await expect(page.locator('.text-danger').filter({ hasText: "Review incidents unavailable" }).last()).toBeVisible({ timeout: 30_000 });
  const error = page.locator('.text-danger').filter({ hasText: "Review incidents unavailable" }).last();
  await expect(error.locator('.cs-status-failed')).toHaveCount(1);
  await expect(error.locator('.cs-status-paused')).toHaveCount(0);
  await error.scrollIntoViewIfNeeded();
  await page.screenshot({ path: info.outputPath("incident-error.png") });
  await page.unrouteAll({ behavior: "wait" });
});

test("second pass SYNTHETIC layout: long-run axes use minutes and hours with endpoints inside the plot", async ({ page, request }, info) => {
  const { job, first } = await fixture(request);
  let minutes = 10;
  // Keep real historical events from replacing this explicitly synthetic layout snapshot.
  await page.route("**/v1/events?**", route => {
    const url = new URL(route.request().url()); url.searchParams.set("types", "review_layout_only");
    return route.continue({ url: url.toString() });
  });
  await page.route(`**/v1/jobs/${job.id}/runs/${first.id}`, route => {
    const end = new Date(), start = new Date(end.getTime() - minutes * 60_000);
    return route.fulfill({ json: { ...first, started_at: start.toISOString(), completed_at: end.toISOString(), tasks: first.tasks.map(task => ({ ...task, started_at: start.toISOString(), completed_at: end.toISOString() })) } });
  });
  for (const view of [{ minutes: 10, width: 1440 }, { minutes: 120, width: 390 }]) {
    minutes = view.minutes;
    await page.setViewportSize({ width: view.width, height: 844 });
    await page.goto(`/jobs/${job.id}/runs/${first.id}`);
    const ticks = page.getByTestId("timeline-tick");
    await expect(ticks.last()).toHaveText(minutes === 10 ? "10m" : "2h");
    expect(await ticks.allTextContents()).toEqual(minutes === 10 ? ["0ms", "2m", "4m", "6m", "8m", "10m"] : ["0ms", "30m", "1h", "1h 30m", "2h"]);
    expect(await ticks.last().evaluate(label => {
      const rect = label.getBoundingClientRect(), axis = label.parentElement!.getBoundingClientRect();
      return rect.left >= axis.left - 1 && rect.right <= axis.right + 1;
    })).toBe(true);
    if (view.width === 390) await expect(page.getByText("Scroll timeline horizontally → · task names stay visible")).toBeVisible();
    await page.screenshot({ path: info.outputPath(`timeline-${minutes}m-${view.width}.png`) });
  }
  await page.unrouteAll({ behavior: "wait" });
});
