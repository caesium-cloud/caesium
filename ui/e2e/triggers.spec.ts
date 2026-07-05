import { expect, test } from "@playwright/test";
import { applyDefinitions, loadFixtureDefinition, loadFixtureDefinitions } from "./helpers/fixtures";

test("triggers page renders cron next-fire and event summaries", async ({ page, request }) => {
  const cronDefinitions = await loadFixtureDefinitions("run-history.job.yaml");
  const eventDefinition = await loadFixtureDefinition("event-trigger.job.yaml");
  const cronAlias = String(cronDefinitions[0]?.metadata?.alias ?? "");
  const eventAlias = String(eventDefinition.metadata?.alias ?? "");

  await applyDefinitions(request, ...cronDefinitions, eventDefinition);

  await page.goto("/triggers");
  await expect(page.getByRole("heading", { name: "Triggers", exact: true })).toBeVisible();

  const cronRow = page.getByTestId("trigger-card").filter({ hasText: cronAlias }).first();
  await expect(cronRow).toBeVisible();
  await expect(cronRow).toContainText("*/2 * * * *");
  await expect(cronRow).toContainText("Next:");
  await expect(cronRow).not.toContainText("Invalid cron");

  const eventRow = page.getByTestId("trigger-card").filter({ hasText: eventAlias }).first();
  await expect(eventRow).toBeVisible();
  await expect(eventRow).toContainText("deployment.* from github-actions");
  await expect(eventRow).toContainText("2 filters, 3 mapped params, 2 default params");
  await expect(eventRow).not.toContainText("{\"defaultParams\"");
});
