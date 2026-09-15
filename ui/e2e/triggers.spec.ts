import { expect, test } from "@playwright/test";
import {
  applyDefinitions,
  awaitRun,
  findJobByAlias,
  loadFixtureDefinition,
  loadFixtureDefinitions,
  triggerJob,
  uniqueSuffix,
  type FixtureDefinition,
} from "./helpers/fixtures";

function futureCronDefinition(alias: string, cron: string): FixtureDefinition {
  return {
    apiVersion: "v1",
    kind: "Job",
    metadata: { alias },
    trigger: { type: "cron", configuration: { cron, timezone: "UTC" } },
    steps: [{ name: "scheduled", engine: "docker", image: "alpine:3.23", command: ["sh", "-c", "true"] }],
  } as FixtureDefinition;
}

function cacheDefinition(alias: string): FixtureDefinition {
  return {
    apiVersion: "v1",
    kind: "Job",
    metadata: { alias },
    trigger: { type: "cron", configuration: { cron: "0 0 1 1 *", timezone: "UTC" } },
    steps: [{
      name: "cached",
      engine: "docker",
      image: "alpine:3.23",
      command: ["sh", "-c", "true"],
      cache: { ttl: "13h" },
    }],
  } as FixtureDefinition;
}

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

test("triggers page distinguishes future minute, day, and yearly fires with UTC timestamps", async ({ page, request }) => {
  await page.clock.setFixedTime(new Date("2026-09-14T12:00:00.000Z"));
  const suffix = uniqueSuffix();
  const schedules = [
    { alias: `next-minute-${suffix}`, cron: "*/1 * * * *", relative: "in 1m", timestamp: "2026-09-14 12:01:00 UTC" },
    { alias: `next-day-${suffix}`, cron: "0 0 * * *", relative: "in 12h", timestamp: "2026-09-15 00:00:00 UTC" },
    { alias: `next-year-${suffix}`, cron: "0 0 1 1 *", relative: "in 108d", timestamp: "2027-01-01 00:00:00 UTC" },
  ];
  await applyDefinitions(request, ...schedules.map(({ alias, cron }) => futureCronDefinition(alias, cron)));

  await page.goto("/triggers");
  await expect(page.getByRole("heading", { name: "Triggers", exact: true })).toBeVisible();

  for (const schedule of schedules) {
    const row = page.getByTestId("trigger-card").filter({ hasText: schedule.alias }).first();
    const nextFire = row.getByTestId("trigger-next-fire");
    await expect(nextFire).toContainText(`Next: ${schedule.relative}`);
    await expect(nextFire).not.toContainText("just now");
    await expect(nextFire.getByTestId("trigger-next-fire-timestamp")).toHaveText(schedule.timestamp);
  }
});

test("cache inventory renders a future expiry as a countdown", async ({ page, request }) => {
  test.slow();
  const alias = `cache-expiry-${uniqueSuffix()}`;
  await applyDefinitions(request, cacheDefinition(alias));
  const job = await findJobByAlias(request, alias);
  await triggerJob(request, job.id);
  await awaitRun(request, job.id, { status: "succeeded" });

  await page.goto(`/jobs/${job.id}/cache`);
  await expect(page.getByRole("heading", { name: "Cache Inventory", exact: true })).toBeVisible();
  const expiry = page.getByTestId("cache-expiry");
  await expect(expiry).toHaveText(/^in 12h$/);
  await expect(expiry).not.toHaveText("just now");
});
