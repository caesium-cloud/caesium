import { expect, test } from "@playwright/test";
import { applyDefinitions, awaitRun, failOnUnexpectedPageErrors, findJobByAlias, triggerJob, uniqueSuffix, type FixtureDefinition } from "./helpers/fixtures";
import { readRunRows } from "./helpers/run-history";

failOnUnexpectedPageErrors();

test("recovery reads the status of each real run history row before and after reload", async ({ page, request }) => {
  const alias = `history-status-${uniqueSuffix()}`;
  await applyDefinitions(request, {
    apiVersion: "v1", kind: "Job", metadata: { alias },
    trigger: { type: "cron", configuration: { cron: "0 0 1 1 *" } },
    steps: [{ name: "history", engine: process.env.CAESIUM_E2E_ENGINE || "docker", image: "alpine:3.23", cache: false, command: ["sh", "-c", 'test "$CAESIUM_PARAM_OUTCOME" = succeeded'] }],
  } as FixtureDefinition);
  const job = await findJobByAlias(request, alias);
  await triggerJob(request, job.id, { params: { OUTCOME: "succeeded" } });
  const succeeded = await awaitRun(request, job.id, { status: "succeeded" });
  await triggerJob(request, job.id, { params: { OUTCOME: "failed" } });
  const failed = await awaitRun(request, job.id, { status: "failed" });
  const expected = [
    { id: succeeded.id, status: "succeeded" },
    { id: failed.id, status: "failed" },
  ].sort((a, b) => a.id.localeCompare(b.id));

  await page.goto(`/jobs/${job.id}/runs`);
  for (const reload of [false, true]) {
    if (reload) await page.reload();
    await expect(page.getByTestId("job-runs-list").getByTestId("run-history-row")).toHaveCount(2);
    await expect.poll(async () => (await readRunRows(page, job.id)).sort((a, b) => a.id.localeCompare(b.id))).toEqual(expected);
  }

  // An unreadable badge must not borrow another row's terminal status.
  const failedRow = page.getByTestId("run-history-row").filter({ has: page.getByRole("link", { name: `Open run ${failed.id}` }) });
  await failedRow.locator('[data-history-cell="status"] [data-status]').evaluate(el => el.removeAttribute("data-status"));
  expect((await readRunRows(page, job.id)).find(row => row.id === failed.id)?.status).toBeNull();
});
