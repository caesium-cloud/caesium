import { expect, test } from "@playwright/test";
import {
  applyDefinitions,
  awaitRun,
  findJobByAlias,
  loadFixtureDefinition,
  loadFixtureDefinitions,
  triggerJob,
} from "./helpers/fixtures";

function shortId(value: string) {
  return value.slice(0, 8);
}

test("operator can pause and unpause a job from the detail page", async ({ page, request }) => {
  // The minimal fixture uses a daily cron in the future, so the job will sit
  // idle while we exercise the pause toggles — no engine activity required.
  const definition = await loadFixtureDefinition("minimal.job.yaml");
  const alias = String(definition.metadata?.alias);
  await applyDefinitions(request, definition);

  await page.goto("/jobs");
  const row = page.locator('[data-testid="job-row"]', { hasText: alias }).first();
  await expect(row).toBeVisible();
  await row.getByRole("link", { name: alias }).click();

  await page.waitForURL(/\/jobs\/[^/]+$/);
  await expect(page.getByRole("heading", { name: alias })).toBeVisible();

  const triggerButton = page.getByRole("button", { name: "Trigger" });
  const pauseButton = page.getByRole("button", { name: "Pause" });
  await expect(triggerButton).toBeEnabled();
  await expect(pauseButton).toBeVisible();

  await pauseButton.click();

  // Once paused, the action button label flips to "Unpause" and the trigger
  // button is disabled so operators can't kick off new runs by accident.
  const unpauseButton = page.getByRole("button", { name: "Unpause" });
  await expect(unpauseButton).toBeVisible();
  await expect(triggerButton).toBeDisabled();

  // The header status badge should switch to "paused" alongside the alias.
  await expect(page.getByText("paused", { exact: true }).first()).toBeVisible();

  await unpauseButton.click();
  await expect(pauseButton).toBeVisible();
  await expect(triggerButton).toBeEnabled();
});

test("jobs list filters rows by search and shows empty state for no matches", async ({ page, request }) => {
  const first = await loadFixtureDefinition("minimal.job.yaml");
  const second = await loadFixtureDefinition("minimal.job.yaml");
  const firstAlias = String(first.metadata?.alias);
  const secondAlias = String(second.metadata?.alias);
  await applyDefinitions(request, first, second);

  await page.goto("/jobs");

  const firstRow = page.locator('[data-testid="job-row"]', { hasText: firstAlias }).first();
  const secondRow = page.locator('[data-testid="job-row"]', { hasText: secondAlias }).first();
  await expect(firstRow).toBeVisible();
  await expect(secondRow).toBeVisible();

  const searchInput = page.getByPlaceholder("Filter pipelines…");
  await searchInput.fill(firstAlias);

  await expect(firstRow).toBeVisible();
  await expect(page.locator('[data-testid="job-row"]', { hasText: secondAlias })).toHaveCount(0);

  await searchInput.fill("");
  await expect(firstRow).toBeVisible();
  await expect(secondRow).toBeVisible();

  await searchInput.fill("definitely-not-an-alias-xyz");
  await expect(page.locator('[data-testid="job-row"]')).toHaveCount(0);
  await expect(page.getByText("No pipelines match")).toBeVisible();
});

test("jobs live activity de-duplicates terminal events and resolves aliases", async ({ page, request }) => {
  const definitions = await loadFixtureDefinitions("run-history.job.yaml");
  const successAlias = String(definitions[0]?.metadata?.alias);
  const failureAlias = String(definitions[1]?.metadata?.alias);
  await applyDefinitions(request, ...definitions);
  const successJob = await findJobByAlias(request, successAlias);
  const failureJob = await findJobByAlias(request, failureAlias);

  await page.goto("/jobs");
  await expect(page.locator('[data-testid="job-row"]', { hasText: successAlias }).first()).toBeVisible();
  await expect(page.locator('[data-testid="job-row"]', { hasText: failureAlias }).first()).toBeVisible();

  await triggerJob(request, successJob.id);
  const successRun = await awaitRun(request, successJob.id, { status: "succeeded" });
  await triggerJob(request, failureJob.id);
  const failureRun = await awaitRun(request, failureJob.id, { status: "failed" });

  const feed = page.getByTestId("activity-feed");
  await expect(feed).toBeVisible();
  await expect(feed).toContainText(successAlias);
  await expect(feed).toContainText(failureAlias);
  await expect(feed).not.toContainText(successJob.id);
  await expect(feed).not.toContainText(failureJob.id);

  const successEntries = page.getByTestId("activity-entry").filter({ hasText: shortId(successRun.id) });
  await expect(successEntries.filter({ hasText: "Run completed" })).toHaveCount(1);
  await expect(successEntries.filter({ hasText: "Run failed" })).toHaveCount(0);

  const failureEntries = page.getByTestId("activity-entry").filter({ hasText: shortId(failureRun.id) });
  await expect(failureEntries.filter({ hasText: "Run failed" })).toHaveCount(1);
  await expect(failureEntries.filter({ hasText: "Run completed" })).toHaveCount(0);
});
