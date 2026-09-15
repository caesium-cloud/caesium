import { expect, test } from "@playwright/test";
import {
  applyDefinitions,
  awaitRun,
  findJobByAlias,
  triggerJob,
  uniqueSuffix,
  type FixtureDefinition,
} from "./helpers/fixtures";

function inheritedCacheDefinition(alias: string): FixtureDefinition {
  return {
    apiVersion: "v1",
    kind: "Job",
    metadata: { alias, cache: true },
    trigger: { type: "cron", configuration: { cron: "0 0 1 1 *", timezone: "UTC" } },
    steps: [
      {
        name: "inherited-cache",
        engine: "docker",
        image: "alpine:3.23",
        command: ["sh", "-c", "echo cached"],
        next: ["explicit-opt-out"],
      },
      {
        name: "explicit-opt-out",
        engine: "docker",
        image: "alpine:3.23",
        command: ["sh", "-c", "echo always-runs"],
        cache: false,
        dependsOn: ["inherited-cache"],
      },
    ],
  } as FixtureDefinition;
}

test("cache inventory labels a job-level cache hit as inherited", async ({ page, request }) => {
  test.slow();
  const alias = `inherited-cache-${uniqueSuffix()}`;
  await applyDefinitions(request, inheritedCacheDefinition(alias));
  const job = await findJobByAlias(request, alias);

  await triggerJob(request, job.id);
  const firstRun = await awaitRun(request, job.id, { status: "succeeded" });
  await triggerJob(request, job.id);
  const rerun = await awaitRun(request, job.id, { status: "succeeded" });
  expect(rerun.id).not.toBe(firstRun.id);
  expect(rerun.tasks.find((task) => task.status === "cached")?.task_id).toBeTruthy();
  expect(rerun.tasks.some((task) => task.status === "succeeded")).toBe(true);

  await page.goto(`/jobs/${job.id}/cache`);
  await expect(page.getByRole("heading", { name: "Cache Inventory", exact: true })).toBeVisible();
  const entry = page.getByRole("row").filter({ hasText: "inherited-cache" });
  await expect(entry.getByTestId("cache-entry-policy")).toHaveText("Inherited: Enabled");
  await expect(entry.getByTestId("cache-entry-policy")).not.toHaveText("Disabled");
});
