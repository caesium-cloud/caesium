import { expect, test } from "@playwright/test";
import { applyAndRun, failOnUnexpectedPageErrors } from "./helpers/fixtures";

failOnUnexpectedPageErrors();
test.use({ viewport: { width: 1280, height: 960 }, permissions: ["clipboard-read", "clipboard-write"] });

test("live run identities copy in full and the desktop prompt remains keyboard usable", async ({ page, request }) => {
  const { job, run } = await applyAndRun(request, "branching.job.yaml", { status: "succeeded" });
  await page.goto(`/jobs/${job.id}/runs/${run.id}`);
  await expect(page.getByTestId("run-heading")).toHaveText(job.alias);
  const chip = page.getByRole("button", { name: `Copy run id: ${run.id}`, exact: true }).first();
  await expect(chip).toHaveAttribute("title", run.id);
  await expect(chip).toContainText(run.id.slice(0, 8));
  await chip.click();
  await expect(page.getByRole("status", { name: "" }).filter({ hasText: /^Copied$/ })).toBeVisible();
  expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(run.id);
  await expect(chip.getByRole("status")).toHaveCount(0);
  await page.keyboard.press(":");
  const command = page.getByRole("combobox", { name: "Search pages, jobs, triggers, or atoms" });
  await expect(command).toBeFocused();
  await expect(page.getByRole("dialog", { name: "Navigation search" })).toBeVisible();
  await command.fill(job.alias);
  await expect(page.locator('[cmdk-item][data-value^="job "]').filter({ hasText: job.alias })).toBeVisible();
  await command.fill(job.id);
  await page.keyboard.press("Enter");
  await expect(page).toHaveURL(new RegExp(`/jobs/${job.id}$`));
});

for (const viewport of [{ width: 1440, height: 900 }, { width: 1280, height: 800 }, { width: 390, height: 844 }]) {
  test(`navigation search is discoverable and contained at ${viewport.width}px`, async ({ page }, testInfo) => {
    await page.setViewportSize(viewport);
    await page.goto("/jobs");
    const trigger = page.getByRole("button", { name: "Open search", exact: true });
    await expect(trigger).toContainText("Search pages");
    const footer = (await page.locator("footer").boundingBox())!;
    const button = (await trigger.boundingBox())!;
    expect(button.width).toBe(footer.width);
    // The middle of the bar and the shortcut area used to be inert.
    await page.mouse.click(footer.x + footer.width / 2, footer.y + footer.height / 2);
    const dialog = page.getByRole("dialog", { name: "Navigation search" });
    const input = dialog.getByRole("combobox", { name: "Search pages, jobs, triggers, or atoms" });
    await expect(input).toBeFocused();
    // Focus arrives before the opening animation finishes. Measure one settled
    // layout rather than comparing boxes captured on different animation frames.
    await expect(dialog).toHaveCSS("opacity", "1");
    await expect.poll(() => dialog.evaluate(el => {
      const box = el.getBoundingClientRect();
      return box.x >= 0 && box.y >= 0 && box.right <= innerWidth && box.bottom <= innerHeight && box.width <= 672 && el.scrollWidth === el.clientWidth;
    })).toBe(true);
    const close = dialog.getByRole("button", { name: "Close", exact: true });
    const inputBox = (await input.boundingBox())!;
    expect(inputBox.x + inputBox.width).toBeLessThanOrEqual((await close.boundingBox())!.x);
    await page.screenshot({ path: testInfo.outputPath("search-open.png") });
    await page.keyboard.press("Tab");
    await expect(close).toBeFocused();
    await page.keyboard.press("Tab");
    await expect(input).toBeFocused();
    await input.fill("no-search-match-4bc995eb");
    await expect(dialog.getByText("No results found.", { exact: true })).toBeVisible();
    await close.click();
    await expect(dialog).toBeHidden();
    await expect(trigger).toBeFocused();
    await page.mouse.click(footer.x + footer.width - 24, footer.y + footer.height / 2);
    await expect(input).toBeFocused();
    await page.keyboard.press("Escape");
    await expect(trigger).toBeFocused();
    await page.keyboard.press("Control+k");
    await expect(input).toBeFocused();
    await expect(dialog).toHaveCSS("opacity", "1");
    await page.mouse.click(4, viewport.height / 2);
    await expect(dialog).toBeHidden();
    await expect(trigger).toBeFocused();
    await page.keyboard.press("Meta+k");
    await expect(input).toBeFocused();
    await input.fill("stats");
    await page.setViewportSize({ width: viewport.width === 390 ? 1280 : 390, height: 844 });
    await expect(input).toHaveValue("stats");
    await expect(input).toBeFocused();
    await page.keyboard.press("Enter");
    await expect(page).toHaveURL(/\/stats$/);
    await expect(dialog).toBeHidden();
  });
}

test("self-hosted fonts and UTC instruments honor reduced motion", async ({ page, request }) => {
  const externalFonts: string[] = [];
  page.on("request", request => { if (/fonts\.(googleapis|gstatic)\.com/.test(request.url())) externalFonts.push(request.url()); });
  await page.goto("/system");
  await expect(page.getByRole("heading", { name: "System", exact: true })).toBeVisible();
  await page.evaluate(() => document.fonts.ready);
  expect(await page.evaluate(() => document.fonts.check('13px "Sometype Mono"'))).toBe(true);
  expect(externalFonts).toEqual([]);
  const clock = page.locator("header").getByText(/^\d{2}:\d{2}:\d{2}$/);
  const previous = await clock.textContent();
  await expect.poll(() => clock.textContent()).not.toBe(previous);
  await page.emulateMedia({ reducedMotion: "reduce" });
  await expect(page.locator("header .cs-wave-flat")).toHaveCSS("display", "block");
  await expect(page.locator("header .cs-wave-flat")).toHaveAttribute("d", "M0 10 H120");
  await expect(page.locator("header .cs-wave")).toBeHidden();
  expect(await page.locator("header .atom-orbit, header .atom-nucleus, header .cs-wave").evaluateAll(nodes => nodes.every(node => getComputedStyle(node).animationName === "none"))).toBe(true);
  // Draw actual voters only, including a real one-voter dqlite deployment.
  const healthResponse = await request.get("/health");
  expect(healthResponse.ok()).toBe(true);
  const cluster = (await healthResponse.json()).checks.cluster;
  const voters = cluster.members.filter((member: { role: string }) => member.role.toLowerCase() === "voter");
  const quorum = !cluster.clustered || !cluster.observed || cluster.quorum.status === "unknown" ? "unknown" : cluster.quorum.status === "unavailable" ? "lost" : "ok";
  const atom = page.locator("header svg[data-quorum]");
  await expect(atom).toHaveAttribute("data-quorum", quorum);
  await expect(atom.locator("[data-voter]")).toHaveCount(Math.min(9, voters.length));
});
