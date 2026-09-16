import { expect, test } from "@playwright/test";
import {
  applyDefinitions,
  awaitRun,
  findJobByAlias,
  triggerJob,
  uniqueSuffix,
  type FixtureDefinition,
} from "./helpers/fixtures";

function inheritedCacheDefinition(alias: string, explicitOptOut = false): FixtureDefinition {
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
        cache: explicitOptOut ? false : undefined,
        dependsOn: ["inherited-cache"],
      },
    ],
  } as FixtureDefinition;
}

test("cache inventory labels inherited and overridden policy from resolved task definitions", async ({ page, request }) => {
  test.slow();
  const alias = `inherited-cache-${uniqueSuffix()}`;
  await applyDefinitions(request, inheritedCacheDefinition(alias));
  const job = await findJobByAlias(request, alias);

  await triggerJob(request, job.id);
  const firstRun = await awaitRun(request, job.id, { status: "succeeded" });
  await triggerJob(request, job.id);
  const rerun = await awaitRun(request, job.id, { status: "succeeded" });
  expect(rerun.id).not.toBe(firstRun.id);
  expect(rerun.tasks).toHaveLength(2);
  expect(rerun.tasks.every((task) => task.status === "cached")).toBe(true);

  // Preserve real entries created under the inherited configuration, then
  // change only the current task policy. The inventory must reflect the live
  // definition rather than infer policy from how this entry was populated.
  await applyDefinitions(request, inheritedCacheDefinition(alias, true));
  await triggerJob(request, job.id);
  const optOutRun = await awaitRun(request, job.id, { status: "succeeded" });
  expect(optOutRun.tasks.filter((task) => task.status === "cached")).toHaveLength(1);
  expect(optOutRun.tasks.filter((task) => task.status === "succeeded")).toHaveLength(1);

  let releaseTaskQuery!: () => void;
  const taskQueryReleased = new Promise<void>((resolve) => {
    releaseTaskQuery = resolve;
  });
  const taskURL = `**/v1/jobs/${job.id}/tasks`;
  await page.route(taskURL, async (route) => {
    await taskQueryReleased;
    await route.continue();
  });
  await page.goto(`/jobs/${job.id}/cache`);
  await expect(page.getByText("Loading...", { exact: true })).toBeVisible();
  await expect(page.getByTestId("cache-entry-policy")).toHaveCount(0);

  releaseTaskQuery();
  await expect(page.getByRole("heading", { name: "Cache Inventory", exact: true })).toBeVisible();
  const inherited = page.getByRole("row").filter({ hasText: "inherited-cache" });
  const overridden = page.getByRole("row").filter({ hasText: "explicit-opt-out" });
  await expect(inherited.getByTestId("cache-entry-policy")).toHaveText("Inherited: Enabled");
  await expect(overridden.getByTestId("cache-entry-policy")).toHaveText("Override: Disabled");

  // A successful response that omits the task is still not permission to infer
  // that an entry inherits the job-level policy.
  await page.unroute(taskURL);
  const missingPage = await page.context().newPage();
  await missingPage.route(taskURL, (route) => route.fulfill({ json: [] }));
  await missingPage.goto(`/jobs/${job.id}/cache`);
  await expect(missingPage.getByRole("row").filter({ hasText: "explicit-opt-out" }).getByTestId("cache-entry-policy")).toHaveText(
    "Policy unavailable",
  );
  await missingPage.close();

  // A failed task lookup must retain the explicit unknown state rather than
  // fall back to the job policy and report the opt-out as inherited enabled.
  const errorPage = await page.context().newPage();
  await errorPage.route(taskURL, (route) => route.fulfill({ status: 500, body: "task lookup failed" }));
  await errorPage.goto(`/jobs/${job.id}/cache`);
  await expect(errorPage.getByRole("heading", { name: "Cache Inventory", exact: true })).toBeVisible();
  await expect(errorPage.getByRole("row").filter({ hasText: "explicit-opt-out" }).getByTestId("cache-entry-policy")).toHaveText(
    "Policy unavailable",
  );
  await errorPage.close();
});
