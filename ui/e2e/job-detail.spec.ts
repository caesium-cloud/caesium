import { expect, test } from "@playwright/test";
import { applyAndRun } from "./helpers/fixtures";

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
  await expect(counters).toContainText("1/2 done");
  await expect(counters).toContainText("1 failed");
  await expect(counters).not.toContainText("queued");
});
