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
  const command = page.getByRole("combobox", { name: "Type a command or search" });
  await expect(command).toBeFocused();
  await expect(page.locator("footer").getByRole("dialog", { name: "Command palette" })).toBeVisible();
  await command.fill(job.alias);
  await page.locator('[cmdk-item][data-value^="job "]').filter({ hasText: job.alias }).click();
  await expect(page).toHaveURL(new RegExp(`/jobs/${job.id}$`));
});

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
  expect(await page.evaluate(() => getComputedStyle(document.documentElement).getPropertyValue("--cs-phase-38"))).toMatch(/^-?\d+ms$/);
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
