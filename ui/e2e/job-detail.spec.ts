import { expect, test } from "@playwright/test";
import { applyAndRun, applyDefinitions, findJobByAlias, loadFixtureDefinition } from "./helpers/fixtures";

test("job detail DAG overlay reconciles failed task counts", async ({ page, request }) => {
  test.slow();

  const { job, run } = await applyAndRun(request, "run-history.job.yaml", {
    definitionIndex: 1,
    status: "failed",
  });

  expect(run.tasks).toHaveLength(2);
  expect(run.tasks.some((task) => task.status === "failed")).toBe(true);

  await page.goto(`/jobs/${job.id}`);
  await expect(page.getByRole("heading", { name: job.alias })).toBeVisible();

  const counters = page.getByTestId("dag-counters");
  await expect(counters).toBeVisible();
  await expect(counters).toContainText("1 done");
  await expect(counters).toContainText("1 failed");
  await expect(counters).toContainText("0 blocked");
  await expect(counters).not.toContainText("queued");
});

test("job detail header keeps Pause reachable and icon controls named at 1150px", async ({ page, request }) => {
  const definition = await loadFixtureDefinition("run-history.job.yaml");
  await applyDefinitions(request, definition);
  const job = await findJobByAlias(request, String(definition.metadata?.alias));

  await page.setViewportSize({ width: 1150, height: 800 });
  await page.goto(`/jobs/${job.id}`);

  await expect(page.getByRole("button", { name: "Trigger job" })).toBeVisible();
  await expect(page.getByRole("button", { name: "Open search" })).toBeVisible();
  await expect(page.getByRole("button", { name: "Toggle theme" })).toBeVisible();

  await page.getByRole("button", { name: "More job actions" }).click();
  await expect(page.getByRole("menuitem", { name: "Pause job" })).toBeVisible();
  await expect(page.getByRole("menuitem", { name: "Backfill job" })).toBeVisible();
});

test("job detail secondary views are deep-linkable and close with browser back", async ({ page, request }) => {
  const definition = await loadFixtureDefinition("run-history.job.yaml");
  await applyDefinitions(request, definition);
  const job = await findJobByAlias(request, String(definition.metadata?.alias));

  await page.goto(`/jobs/${job.id}/yaml`);
  await expect(page.getByRole("dialog", { name: "Job Definition (YAML)" })).toBeVisible();

  await page.goto(`/jobs/${job.id}`);
  await page.getByRole("link", { name: "Config" }).click();
  await expect(page).toHaveURL(new RegExp(`/jobs/${job.id}/config$`));
  await expect(page.getByRole("dialog", { name: "Configuration" })).toBeVisible();

  await page.goBack();
  await expect(page).toHaveURL(new RegExp(`/jobs/${job.id}$`));
  await expect(page.getByRole("dialog")).toBeHidden();
});

test("job detail trigger requires confirmation and lands on the run page", async ({ page, request }) => {
  const definition = await loadFixtureDefinition("run-history.job.yaml");
  await applyDefinitions(request, definition);
  const job = await findJobByAlias(request, String(definition.metadata?.alias));

  await page.goto(`/jobs/${job.id}`);
  await page.getByRole("button", { name: "Trigger job" }).click();
  await expect(page.getByRole("dialog", { name: "Trigger Job" })).toBeVisible();

  await page.getByLabel("logical_date").fill("2026-07-07T12:00:00Z");
  await page.getByRole("button", { name: "Confirm Trigger" }).click();

  await page.waitForURL(new RegExp(`/jobs/${job.id}/runs/[^/]+$`));
  await expect(page.getByRole("heading", { name: /Run / })).toBeVisible();
});
