import { expect, test } from "@playwright/test";
import { applyAndRun } from "./helpers/fixtures";

test("All runs always links to the current job after direct entry and re-run", async ({ page, request }) => {
  test.slow();
  const { job, run } = await applyAndRun(request, "run-history.job.yaml", { status: "succeeded" });
  const runsURL = `/jobs/${job.id}/runs`;

  // A direct deep link has no useful browser history to traverse. The action
  // must still lead to the current job's complete list.
  await page.goto(`/jobs/${job.id}/runs/${run.id}`);
  await expect(page.getByRole("heading", { name: /Run / })).toBeVisible();
  const allRuns = page.getByTestId("all-runs-link");
  await expect(allRuns).toHaveAttribute("href", runsURL);
  await allRuns.click();
  await expect(page).toHaveURL(new RegExp(`${runsURL}$`));
  await expect(page.getByTestId("job-runs-list").locator(`a[href="/jobs/${job.id}/runs/${run.id}"]`)).toBeVisible();

  // Re-run opens a new detail URL. All runs must still resolve from the job id
  // in that route rather than returning to the old run via browser history.
  await page.goto(`/jobs/${job.id}/runs/${run.id}`);
  await page.getByRole("button", { name: "Re-run", exact: true }).click();
  await expect(page).toHaveURL(new RegExp(`/jobs/${job.id}/runs/(?!${run.id}$)[^/]+$`), { timeout: 30_000 });
  await expect(page.getByTestId("all-runs-link")).toHaveAttribute("href", runsURL);
  await page.getByTestId("all-runs-link").click();
  await expect(page).toHaveURL(new RegExp(`${runsURL}$`));
});
