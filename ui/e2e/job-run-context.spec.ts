import { expect, test } from "@playwright/test";
import { applyDefinitions, awaitRun, failOnUnexpectedPageErrors, findJobByAlias, triggerJob, uniqueSuffix } from "./helpers/fixtures";

failOnUnexpectedPageErrors();
test.use({ viewport: { width: 1440, height: 900 }, permissions: ["clipboard-read", "clipboard-write"] });

test("Jobs, overview, history and the run picker keep exact execution identity", async ({ page, request }, testInfo) => {
  const alias = `run-context-${uniqueSuffix()}`;
  await applyDefinitions(request, {
    apiVersion: "v1", kind: "Job", metadata: { alias },
    trigger: { type: "cron", configuration: { cron: "0 0 1 1 *" } },
    steps: [{ name: "check-context", engine: process.env.CAESIUM_E2E_ENGINE || "docker", image: "alpine:3.23", cache: false, command: ["echo", "run context"] }],
  });
  const job = await findJobByAlias(request, alias);
  const overviewURL = `/jobs/${job.id}`;
  await page.goto(overviewURL);
  await expect(page.getByText("No runs yet", { exact: true })).toBeVisible();
  await expect(page.getByTestId("view-featured-run")).toHaveCount(0);
  await expect(page.getByTestId("run-picker-trigger")).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Trigger job", exact: true })).toBeEnabled();
  await page.screenshot({ path: testInfo.outputPath("no-runs.png") });
  await triggerJob(request, job.id);
  const first = await awaitRun(request, job.id, { status: "succeeded" });
  await page.reload();
  await expect(page.getByTestId("view-featured-run")).toHaveAttribute("href", `${overviewURL}/runs/${first.id}`);
  await expect(page.getByTestId("run-picker-trigger")).toHaveCount(0);
  await page.screenshot({ path: testInfo.outputPath("one-run.png") });
  await triggerJob(request, job.id);
  const latest = await awaitRun(request, job.id, { status: "succeeded" });
  expect(latest.id).not.toBe(first.id);

  let launched = 0;
  page.on("request", req => { if (req.method() === "POST" && new URL(req.url()).pathname === `/v1/jobs/${job.id}/run`) launched++; });
  for (const theme of ["dark", "light"]) for (const [width, height] of [[1440, 900], [1280, 800], [390, 844]]) {
    await page.evaluate(value => localStorage.setItem("caesium-ui-theme", value), theme);
    await page.setViewportSize({ width, height });
    await page.goto(`/jobs?q=${alias}`);
    await page.reload();
    const row = page.getByTestId("job-row").filter({ hasText: alias });
    const direct = row.getByTestId("job-latest-run-link");
    await expect(direct).toHaveAttribute("href", `${overviewURL}/runs/${latest.id}`);
    await direct.click();
    await expect(page.getByTestId("run-identity")).toContainText(latest.id.slice(0, 8));
    await page.goBack();
    await row.getByRole("link", { name: alias, exact: true }).click();
    const nav = page.getByRole("navigation", { name: "Job navigation" });
    await expect(nav.getByRole("link", { name: "Job overview", exact: true })).toHaveAttribute("aria-current", "page");
    const view = page.getByTestId("view-featured-run");
    await expect(view).toHaveAttribute("href", `${overviewURL}/runs/${latest.id}`);
    await expect(view).toBeInViewport();
    await page.screenshot({ path: testInfo.outputPath(`overview-${theme}-${width}.png`) });
    await view.focus();
    await page.keyboard.press("Enter");
    await expect(page).toHaveURL(`${overviewURL}/runs/${latest.id}`);
    await expect(nav.getByText("Execution", { exact: true })).toHaveAttribute("aria-current", "page");
    await expect(page.getByRole("region", { name: "Execution timeline", exact: true })).toBeVisible();
    const picker = page.getByTestId("run-picker-trigger");
    await expect(picker).toBeInViewport();
    await picker.click();
    await expect(page.getByRole("menu")).toHaveCSS("opacity", "1");
    const selected = page.getByRole("menuitem", { name: new RegExp(`Open run ${latest.id}`) });
    await expect(selected).toHaveAttribute("aria-current", "page");
    const older = page.getByRole("menuitem", { name: new RegExp(`Open run ${first.id}`) });
    await expect(older).toContainText("UTC");
    await expect(older).toContainText(first.id.slice(0, 8));
    await page.screenshot({ path: testInfo.outputPath(`picker-${theme}-${width}.png`) });
    await page.keyboard.press("Escape");
    await expect(picker).toBeFocused();
    await picker.click();
    await older.focus();
    await page.keyboard.press("Enter");
    await expect(page).toHaveURL(`${overviewURL}/runs/${first.id}`);
    await expect(page.getByTestId("run-identity")).toContainText(first.id.slice(0, 8));
    await page.reload();
    await expect(page.getByTestId("run-identity")).toContainText(first.id.slice(0, 8));
    await page.getByRole("button", { name: `Copy run id: ${first.id}`, exact: true }).first().click();
    expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(first.id);
    await page.getByTestId("run-heading").click();
    await page.keyboard.press("a");
    await expect(page.getByRole("dialog", { name: "Run History", exact: true })).toBeVisible();
    await expect(page.getByTestId("job-runs-list").locator(`a[href="${overviewURL}/runs/${first.id}"]`)).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(page).toHaveURL(overviewURL);
    await view.click();
    await nav.getByRole("link", { name: "Job overview", exact: true }).click();
    await expect(page).toHaveURL(overviewURL);
    expect(await page.locator("main").evaluate(el => el.scrollWidth - el.clientWidth)).toBeLessThanOrEqual(1);
  }
  expect(launched).toBe(0);
});
