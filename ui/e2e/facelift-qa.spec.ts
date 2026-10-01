import fs from "node:fs/promises";
import path from "node:path";
import { parseDocument } from "yaml";
import { expect, test, type Locator, type Page } from "@playwright/test";
import { applyAndRun, applyDefinitions, awaitRun, failOnUnexpectedPageErrors, findJobByAlias, loadFixtureDefinitions, triggerJob, uniqueSuffix } from "./helpers/fixtures";

failOnUnexpectedPageErrors();

async function expectMainFits(page: Page) {
  await expect.poll(() => page.locator("main").evaluate(main => ({
    width: main.clientWidth, overflow: main.scrollWidth - main.clientWidth,
  }))).toEqual({ width: 375, overflow: 0 });
}

function edgePairs(steps: { name: string; next?: string | string[]; dependsOn?: string | string[] }[]) {
  const list = (edge: string | string[] | undefined) => edge === undefined ? [] : Array.isArray(edge) ? edge : [edge];
  return [...new Set(steps.flatMap(step => [
    ...list(step.next).map(next => `${step.name}->${next}`),
    ...list(step.dependsOn).map(previous => `${previous}->${step.name}`),
  ]))].sort();
}

async function contrast(text: Locator, background: Locator) {
  const foreground = await text.evaluate(el => getComputedStyle(el).color);
  const surface = await background.evaluate(el => getComputedStyle(el).backgroundColor);
  const luminance = (color: string) => {
    const channels = color.match(/[\d.]+/g)!.slice(0, 3).map(channel => {
      const value = Number(channel) / 255;
      return value <= 0.04045 ? value / 12.92 : ((value + 0.055) / 1.055) ** 2.4;
    });
    return channels[0] * .2126 + channels[1] * .7152 + channels[2] * .0722;
  };
  const light = luminance(foreground), dark = luminance(surface);
  return (Math.max(light, dark) + .05) / (Math.min(light, dark) + .05);
}

test("ordinary scrolling reaches the receipt without growing the run graph", async ({ page, request }) => {
  await page.setViewportSize({ width: 1280, height: 720 });
  const { job, run } = await applyAndRun(request, "branching.job.yaml", { status: "succeeded" });
  await page.goto(`/jobs/${job.id}/runs/${run.id}`);
  const canvas = page.getByTestId("run-dag-canvas-viewport");
  await expect(canvas.locator(".react-flow__node")).toHaveCount(4);
  const height = await canvas.evaluate(el => el.clientHeight);
  const main = page.locator("main");
  const box = (await main.boundingBox())!;
  await page.mouse.move(box.x + box.width - 8, box.y + box.height / 2);
  const title = page.getByTestId("run-reproducibility-toggle");
  let reached = false;
  for (let attempt = 0; attempt < 8; attempt++) {
    const before = await main.evaluate(el => el.scrollTop);
    await page.mouse.wheel(0, 200);
    await expect.poll(() => main.evaluate(el => el.scrollTop)).toBeGreaterThan(before);
    await expect.poll(() => canvas.evaluate(el => el.clientHeight)).toBe(height);
    const receipt = (await title.boundingBox())!;
    if (receipt.y >= box.y && receipt.y + receipt.height <= box.y + box.height) {
      reached = true;
      break;
    }
  }
  expect(reached, "Receipt title must be reachable through the main scroll gutter").toBe(true);
  await title.click();
  await page.mouse.move(box.x + box.width - 8, box.y + box.height / 2);
  await page.mouse.wheel(0, 200);
  const receiptTitle = page.getByTestId("receipt-panel").getByText("Reproducibility Receipt", { exact: true });
  await expect.poll(async () => {
    const receipt = await receiptTitle.boundingBox();
    return Boolean(receipt && receipt.y >= box.y && receipt.y + receipt.height <= box.y + box.height);
  }).toBe(true);
  await expect(canvas).toHaveCSS("height", `${height}px`);
  // Resizing while scrolled must also use the unscrolled layout coordinates.
  await page.setViewportSize({ width: 1280, height: 721 });
  await expect.poll(() => canvas.evaluate(el => el.clientHeight)).toBeLessThanOrEqual(height + 1);
});

test("both structured and terminal task logs remain readable", async ({ page, request }, testInfo) => {
  const alias = `qa-log-modes-${uniqueSuffix()}`;
  await applyDefinitions(request, {
    apiVersion: "v1", kind: "Job", metadata: { alias },
    trigger: { type: "cron", configuration: { cron: "0 0 1 1 *" } },
    steps: [
      { name: "structured", image: "alpine:3.23", command: ["sh", "-c", "printf 'level=info message=ready\\nlevel=warning message=warning-retained\\n'"] },
      { name: "terminal", image: "alpine:3.23", command: ["sh", "-c", "echo plain retained output"] },
    ],
  });
  const job = await findJobByAlias(request, alias);
  await triggerJob(request, job.id);
  const run = await awaitRun(request, job.id, { status: "succeeded" });
  await page.goto(`/jobs/${job.id}/runs/${run.id}`);
  await page.locator(".react-flow__node", { hasText: "structured" }).click();
  const panel = page.getByTestId("task-detail-panel");
  const text = panel.getByTestId("task-log-structured-text");
  await expect(text).toContainText("warning-retained");
  await panel.getByPlaceholder("Filter visible rows").fill("warning");
  await expect(text).not.toContainText("ready");
  expect(await contrast(text, panel.getByTestId("task-log-structured-scroll"))).toBeGreaterThanOrEqual(4.5);
  await panel.screenshot({ path: testInfo.outputPath("structured-logs.png") });
  await page.keyboard.press("Escape");
  await expect(panel).toBeHidden();
  await page.locator(".react-flow__node", { hasText: "terminal" }).click();
  await expect(panel.getByTestId("task-log-plaintext")).toContainText("plain retained output");
  const terminal = panel.getByTestId("task-log-terminal");
  await expect(terminal).toBeVisible();
  const row = terminal.locator(".xterm-rows > div").filter({ hasText: "plain retained output" }).first();
  await expect(row).toBeVisible();
  expect(await contrast(row, terminal.locator(".xterm-rows"))).toBeGreaterThanOrEqual(4.5);
  await panel.screenshot({ path: testInfo.outputPath("terminal-logs.png") });
});

test("mobile JobDefs contains long lines and applies documented scalar-edge examples", async ({ page, request }) => {
  await page.setViewportSize({ width: 375, height: 812 });
  await page.goto("/jobdefs");
  const apply = page.getByRole("button", { name: "Apply definition", exact: true });
  await expect(apply).toBeEnabled();
  await expectMainFits(page);
  const reset = page.getByRole("button", { name: "Reset example" });
  const resetBox = (await reset.boundingBox())!;
  expect(resetBox.x + resetBox.width).toBeLessThanOrEqual(375);
  const scroller = page.locator(".cm-scroller");
  await expect.poll(() => scroller.evaluate(el => el.scrollWidth > el.clientWidth)).toBe(true);
  await scroller.evaluate(el => { el.scrollLeft = el.scrollWidth; });
  await expect.poll(() => scroller.evaluate(el => el.scrollLeft)).toBeGreaterThan(0);
  await expectMainFits(page);

  for (const filename of ["fanout-join.job.yaml", "incremental-cache.job.yaml", "task-outputs.job.yaml"]) {
    const alias = `qa-scalar-${uniqueSuffix()}`;
    const source = (await fs.readFile(path.resolve(process.cwd(), "../docs/examples", filename), "utf8"))
      .replace(/^ {2}alias:.*$/m, `  alias: ${alias}`)
      .replace(/^ {4}path:.*$/m, `    path: /hooks/qa-scalar/${alias}`)
      .replace(/^ {4}cron:.*$/m, '    cron: "0 0 1 1 *"');
    await page.locator('.cm-content[contenteditable="true"]').click();
    await page.keyboard.press("ControlOrMeta+A");
    const lint = page.waitForResponse(response => response.url().endsWith("/v1/jobdefs/lint") && response.request().postData()?.includes(alias) === true);
    await page.keyboard.insertText(source);
    const lintResponse = await lint;
    expect(lintResponse.ok()).toBe(true);
    expect((await lintResponse.json()).errors).toEqual([]);
    await expect(apply).toBeEnabled();
    await expectMainFits(page);
    const applied = page.waitForResponse(response => response.url().endsWith("/v1/jobdefs/apply") && response.request().postData()?.includes(alias) === true);
    await apply.click();
    expect((await applied).ok()).toBe(true);
    const job = await findJobByAlias(request, alias);
    const manifest = await request.get(`/v1/jobs/${job.id}/manifest?format=json`);
    expect(manifest.ok()).toBe(true);
    // The export reconstructs dependencies as equivalent successor edges.
    expect(edgePairs((await manifest.json()).steps)).toEqual(edgePairs(parseDocument(source).toJS().steps));
  }
});

test("HTTP trigger fields have labels and support keyboard creation", async ({ page }) => {
  await page.setViewportSize({ width: 375, height: 812 });
  await page.goto("/triggers");
  await page.getByRole("button", { name: "New HTTP Trigger", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "New HTTP Trigger", exact: true });
  const labels = ["Alias", "Webhook Path", "Secret", "Auth Scheme", "Signature Header", "Param Mapping JSON", "Default Params JSON"];
  for (const label of labels) {
    const control = dialog.getByLabel(label, { exact: true });
    await expect(control).toBeEnabled();
    await expect(control).toHaveAccessibleName(label);
  }
  await dialog.getByText("Alias", { exact: true }).click();
  await expect(dialog.getByLabel("Alias", { exact: true })).toBeFocused();
  for (const label of labels.slice(1)) {
    await page.keyboard.press("Tab");
    await expect(dialog.getByLabel(label, { exact: true })).toBeFocused();
  }
  const suffix = uniqueSuffix();
  await dialog.getByLabel("Alias", { exact: true }).fill(`qa-labelled-${suffix}`);
  await dialog.getByLabel("Webhook Path", { exact: true }).fill(`/hooks/qa-labelled-${suffix}`);
  await dialog.getByRole("button", { name: "Create Trigger", exact: true }).focus();
  const saved = page.waitForResponse(response => response.url().endsWith("/v1/triggers") && response.request().method() === "POST");
  await page.keyboard.press("Enter");
  expect((await saved).ok()).toBe(true);
  await expect(dialog).toBeHidden();
  await expect(page.getByText(`qa-labelled-${suffix}`, { exact: true })).toBeVisible();
});

test("navigation search describes working destinations and Holds is the only active entry", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 720 });
  await page.goto("/datasets/holds");
  const nav = page.locator("aside nav");
  await expect(nav.getByRole("link", { name: /^Holds\b/ })).toHaveAttribute("aria-current", "page");
  await expect(nav.getByRole("link", { name: /^Datasets\b/ })).not.toHaveAttribute("aria-current", "page");
  await expect(nav.locator('[aria-current="page"]')).toHaveCount(1);
  const footer = page.locator("footer");
  await expect(footer).not.toContainText(/why task|verify receipt|replay run|diff run run|blame run/);
  await page.keyboard.press(":");
  const search = page.getByRole("combobox", { name: "Search pages, jobs, triggers, or atoms" });
  await expect(search).toBeFocused();
  await search.fill("system");
  await page.keyboard.press("Enter");
  await expect(page).toHaveURL(/\/system$/);
  await expect(page.getByRole("heading", { name: "System", exact: true })).toBeVisible();
});

test("failed task banners use the failed glyph in Logs and Details", async ({ page, request }) => {
  const definition = (await loadFixtureDefinitions("run-history.job.yaml"))[1];
  definition.trigger!.configuration!.cron = "0 0 1 1 *";
  await applyDefinitions(request, definition);
  const job = await findJobByAlias(request, String(definition.metadata!.alias));
  await triggerJob(request, job.id);
  const run = await awaitRun(request, job.id, { status: "failed" });
  await page.goto(`/jobs/${job.id}/runs/${run.id}`);
  await page.locator(".react-flow__node").filter({ has: page.getByTestId("task-node-label").filter({ hasText: /^fail$/ }) }).click();
  const panel = page.getByTestId("task-detail-panel");
  const logs = panel.getByTestId("task-log-error");
  await expect(logs).toContainText("Task Error");
  await expect(logs.locator(".cs-status-failed.text-danger")).toBeVisible();
  await expect(logs.locator(".cs-status-paused")).toHaveCount(0);
  await panel.getByRole("button", { name: "Details", exact: true }).click();
  const details = panel.getByTestId("task-detail-error");
  await expect(details).toContainText("Error");
  await expect(details.locator(".cs-status-failed.text-danger")).toBeVisible();
  await expect(details.locator(".cs-status-paused")).toHaveCount(0);
});
